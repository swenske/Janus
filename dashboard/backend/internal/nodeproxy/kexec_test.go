package nodeproxy

import (
	"bytes"
	"context"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// fakeReboots plays the node's Reboot: it records the modes asked for,
// and refuses kexec when told to, as a node with Secure Boot on and an
// image not signed by Janus does.
type fakeReboots struct {
	janusv1alpha1.UnimplementedSystemServiceServer
	mu       sync.Mutex
	modes    []janusv1alpha1.RebootMode
	noKexec  bool
	rebooted int
}

func (f *fakeReboots) Reboot(_ context.Context, req *janusv1alpha1.RebootRequest) (*janusv1alpha1.RebootResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modes = append(f.modes, req.GetMode())
	if f.noKexec && req.GetMode() == janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC {
		return nil, status.Error(codes.FailedPrecondition, "kexec: Secure Boot is on and the UKI isn't signed by a Janus release certificate - reboot through the firmware instead")
	}
	f.rebooted++
	return &janusv1alpha1.RebootResponse{}, nil
}

// The node page's updates and reboot pass the reboot mode on to the node:
// kexec when asked, the firmware otherwise; any other mode is refused
// before the node is called.
func TestRebootModeReachesTheNode(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	life := &fakeLifecycle{files: map[string][]byte{}}
	reboots := &fakeReboots{}
	janusv1alpha1.RegisterLifecycleServiceServer(srv, life)
	janusv1alpha1.RegisterSystemServiceServer(srv, reboots)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{ID: "kexec-test"}
	conns.Store(node.ID, conn)
	t.Cleanup(func() { closeNodeConn(node.ID) })

	bundle, _ := bundleServer(t)
	mux := http.NewServeMux()
	registerLifecycleRoutes(mux, node)
	registerSystemRoutes(mux, node)
	post := func(path, contentType string, body []byte) (int, string) {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}
	// The page's form: the files, then the choices.
	upload := func(mode string) (string, []byte) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		for field := range releaseFormFields {
			fw, _ := mw.CreateFormFile(field, field)
			_, _ = fw.Write([]byte(field))
		}
		_ = mw.WriteField("reboot_mode", mode)
		_ = mw.Close()
		return mw.FormDataContentType(), b.Bytes()
	}

	updates := []struct {
		name string
		send func(mode string) (int, string)
	}{
		{"upgrade-url", func(mode string) (int, string) {
			return post("/api/lifecycle/upgrade-url", "application/json", []byte(`{"reference":"https://releases.example/v1/","reboot_mode":"`+mode+`"}`))
		}},
		{"upgrade-relay", func(mode string) (int, string) {
			return post("/api/lifecycle/upgrade-relay", "application/json", []byte(`{"reference":"`+bundle.URL+`/bundle","reboot_mode":"`+mode+`"}`))
		}},
		{"upgrade-upload", func(mode string) (int, string) {
			contentType, body := upload(mode)
			return post("/api/lifecycle/upgrade-upload", contentType, body)
		}},
	}
	for _, u := range updates {
		for mode, want := range map[string]janusv1alpha1.RebootMode{
			"":         janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT,
			"firmware": janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT,
			"kexec":    janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC,
		} {
			before := len(life.upgrades)
			if code, body := u.send(mode); code != http.StatusOK {
				t.Fatalf("%s, reboot_mode %q: %d %s", u.name, mode, code, body)
			}
			if len(life.upgrades) != before+1 {
				t.Fatalf("%s, reboot_mode %q: %d upgrades", u.name, mode, len(life.upgrades)-before)
			}
			if got := life.upgrades[before].GetRebootMode(); got != want {
				t.Errorf("%s, reboot_mode %q: the node was asked %v", u.name, mode, got)
			}
		}
		before := len(life.upgrades)
		if code, body := u.send("powercycle"); code != http.StatusBadRequest || !strings.Contains(body, "firmware or kexec") {
			t.Errorf("%s, reboot_mode powercycle: %d %s", u.name, code, body)
		}
		if len(life.upgrades) != before {
			t.Errorf("%s: an unknown reboot_mode still upgraded the node", u.name)
		}
	}

	power := func(body string) (int, string) { return post("/api/system/power", "application/json", []byte(body)) }
	if code, body := power(`{"action":"reboot"}`); code != http.StatusOK {
		t.Fatalf("reboot: %d %s", code, body)
	}
	if code, body := power(`{"action":"reboot","mode":"kexec"}`); code != http.StatusOK {
		t.Fatalf("reboot through kexec: %d %s", code, body)
	}
	if code, body := power(`{"action":"reboot","mode":"soft"}`); code != http.StatusBadRequest {
		t.Errorf("reboot, mode soft: %d %s", code, body)
	}
	reboots.noKexec = true
	if code, body := power(`{"action":"reboot","mode":"kexec"}`); code != http.StatusBadRequest || !strings.Contains(body, "reboot through the firmware instead") {
		t.Errorf("kexec refused by the node: %d %s", code, body)
	}
	want := []janusv1alpha1.RebootMode{janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT, janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC, janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC}
	if len(reboots.modes) != len(want) || reboots.rebooted != 2 {
		t.Fatalf("the node was asked %v, rebooted %d times", reboots.modes, reboots.rebooted)
	}
	for i := range want {
		if reboots.modes[i] != want[i] {
			t.Errorf("reboot %d: the node was asked %v, want %v", i, reboots.modes[i], want[i])
		}
	}
}
