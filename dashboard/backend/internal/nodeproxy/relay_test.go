package nodeproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// fakeLifecycle plays the node: it keeps what's uploaded and records the
// Upgrade it's asked for.
type fakeLifecycle struct {
	janusv1alpha1.UnimplementedLifecycleServiceServer
	mu       sync.Mutex
	files    map[string][]byte
	upgrades []*janusv1alpha1.UpgradeRequest
}

func (f *fakeLifecycle) UploadReleaseFile(stream grpc.ClientStreamingServer[janusv1alpha1.UploadReleaseFileRequest, janusv1alpha1.UploadReleaseFileResponse]) error {
	var name string
	var data []byte
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if req.GetFilename() != "" {
			name = req.GetFilename()
		}
		data = append(data, req.GetChunk()...)
	}
	f.mu.Lock()
	f.files[name] = data
	f.mu.Unlock()
	return stream.SendAndClose(&janusv1alpha1.UploadReleaseFileResponse{StagingDir: "/etc/.state/upgrade-incoming", BytesWritten: uint64(len(data))})
}

func (f *fakeLifecycle) Upgrade(req *janusv1alpha1.UpgradeRequest, stream grpc.ServerStreamingServer[janusv1alpha1.UpgradeResponse]) error {
	f.mu.Lock()
	f.upgrades = append(f.upgrades, req)
	f.mu.Unlock()
	_ = stream.Send(&janusv1alpha1.UpgradeResponse{Stage: "writing-data", Progress: 0.4, Message: "writing slot B"})
	return stream.Send(&janusv1alpha1.UpgradeResponse{Stage: "rebooting", Progress: 1, Message: "rebooting into slot B"})
}

func fakeNode(t *testing.T) (*fakeLifecycle, janusv1alpha1.LifecycleServiceClient) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	fake := &fakeLifecycle{files: map[string][]byte{}}
	janusv1alpha1.RegisterLifecycleServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fake, janusv1alpha1.NewLifecycleServiceClient(conn)
}

// bundleServer serves a release bundle, the rootfs big enough to take
// several reads.
func bundleServer(t *testing.T) (*httptest.Server, map[string][]byte) {
	t.Helper()
	files := map[string][]byte{
		"rootfs.squashfs": []byte(strings.Repeat("squashfs", 300000)),
		"rootfs.verity":   []byte("verity"),
		"uki-a.efi":       []byte("uki a"),
		"uki-b.efi":       []byte("uki b"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[strings.TrimPrefix(r.URL.Path, "/bundle/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, files
}

func relayLines(t *testing.T, rec *httptest.ResponseRecorder) []relayProgress {
	t.Helper()
	var lines []relayProgress
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		var p relayProgress
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			t.Fatalf("not an NDJSON line: %q", sc.Text())
		}
		lines = append(lines, p)
	}
	return lines
}

func TestRelayUpgrade(t *testing.T) {
	fake, client := fakeNode(t)
	srv, files := bundleServer(t)
	sum := sha256.Sum256(files["rootfs.squashfs"])
	rec := httptest.NewRecorder()

	relayUpgrade(context.Background(), client, srv.URL+"/bundle", relayRequest{SHA256: hex.EncodeToString(sum[:]), WaitForHealth: true, AllowSchematicChange: true}, newProgressWriter(rec))

	lines := relayLines(t, rec)
	if last := lines[len(lines)-1]; last.Stage != "done" || last.Message != "rebooting into slot B" {
		t.Fatalf("last line %+v (all: %+v)", last, lines)
	}
	for name, want := range files {
		if string(fake.files[name]) != string(want) {
			t.Errorf("the node got %d bytes of %s, want %d", len(fake.files[name]), name, len(want))
		}
	}
	// Progress per file, the last report counting every byte.
	var squashDone bool
	for _, l := range lines {
		if l.Stage == "download" && l.File == "rootfs.squashfs" && l.Bytes == int64(len(files["rootfs.squashfs"])) && l.Total == l.Bytes {
			squashDone = true
		}
	}
	if !squashDone {
		t.Errorf("no complete progress report for rootfs.squashfs: %+v", lines)
	}
	if len(fake.upgrades) != 1 {
		t.Fatalf("%d upgrades", len(fake.upgrades))
	}
	up := fake.upgrades[0]
	if up.GetSource().GetReference() != "/etc/.state/upgrade-incoming" || up.GetSource().GetSha256() != hex.EncodeToString(sum[:]) || !up.GetSource().GetAllowSchematicChange() || !up.GetWaitForHealth() {
		t.Fatalf("Upgrade request %+v", up)
	}
	var sawNodeStage bool
	for _, l := range lines {
		if l.Stage == "writing-data" && l.Percent == 40 {
			sawNodeStage = true
		}
	}
	if !sawNodeStage {
		t.Errorf("the node's stages weren't passed on: %+v", lines)
	}
}

func TestRelayUpgradeWrongSHA256(t *testing.T) {
	fake, client := fakeNode(t)
	srv, _ := bundleServer(t)
	rec := httptest.NewRecorder()
	relayUpgrade(context.Background(), client, srv.URL+"/bundle", relayRequest{SHA256: strings.Repeat("0", 64)}, newProgressWriter(rec))
	lines := relayLines(t, rec)
	if last := lines[len(lines)-1]; last.Stage != "error" || !strings.Contains(last.Message, "isn't the one expected") {
		t.Fatalf("last line %+v", last)
	}
	if len(fake.upgrades) != 0 {
		t.Fatal("a bundle with the wrong sha256 was installed")
	}
}

func TestRelayUpgradeMissingFile(t *testing.T) {
	fake, client := fakeNode(t)
	srv, _ := bundleServer(t)
	rec := httptest.NewRecorder()
	relayUpgrade(context.Background(), client, srv.URL+"/elsewhere", relayRequest{}, newProgressWriter(rec))
	lines := relayLines(t, rec)
	if last := lines[len(lines)-1]; last.Stage != "error" || !strings.Contains(last.Message, "rootfs.squashfs") || !strings.Contains(last.Message, "404") {
		t.Fatalf("last line %+v", last)
	}
	if len(fake.upgrades) != 0 {
		t.Fatal("upgraded without a bundle")
	}
}

func TestRelayRouteRefusesBadReference(t *testing.T) {
	mux := http.NewServeMux()
	registerLifecycleRoutes(mux, nil)
	for _, ref := range []string{"", "/etc/.state/x", "ftp://example.invalid/b"} {
		rec := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"reference": ref})
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/lifecycle/upgrade-relay", strings.NewReader(string(body))))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("reference %q: %d %s", ref, rec.Code, rec.Body)
		}
	}
}
