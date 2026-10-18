package support

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/rbac"
)

const testKey = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBOGUS\n-----END EC PRIVATE KEY-----\n"

// fakeSystem answers what a node's SystemService would, briefly; the
// network service isn't there at all (an older node, say).
type fakeSystem struct {
	janusv1alpha1.UnimplementedSystemServiceServer
	logLine string
}

func (fakeSystem) Version(context.Context, *emptypb.Empty) (*janusv1alpha1.VersionResponse, error) {
	return &janusv1alpha1.VersionResponse{Version: "v2026.10.18"}, nil
}

func (fakeSystem) ServiceList(context.Context, *emptypb.Empty) (*janusv1alpha1.ServiceListResponse, error) {
	return &janusv1alpha1.ServiceListResponse{Services: []*janusv1alpha1.ServiceInfo{{Id: "janusd"}, {Id: "haproxy"}}}, nil
}

func (fakeSystem) LogBoots(context.Context, *emptypb.Empty) (*janusv1alpha1.LogBootsResponse, error) {
	return &janusv1alpha1.LogBootsResponse{Available: true, Boots: []*janusv1alpha1.LogBoot{{Offset: 0}, {Offset: -1}}}, nil
}

func (f fakeSystem) Logs(req *janusv1alpha1.LogsRequest, s grpc.ServerStreamingServer[janusv1alpha1.Data]) error {
	line := f.logLine
	if line == "" {
		line = req.GetId() + " says hello\n"
	}
	return s.Send(&janusv1alpha1.Data{Bytes: []byte(line + testKey)})
}

func (fakeSystem) Dmesg(req *janusv1alpha1.DmesgRequest, s grpc.ServerStreamingServer[janusv1alpha1.Data]) error {
	return s.Send(&janusv1alpha1.Data{Bytes: []byte("[    1.0] booting\n[    2.0] audit: type=1400 avc:  denied  { read } for comm=\"x\"\n")})
}

// Events and Audit follow once the backlog is sent: they never end.
func (fakeSystem) Events(_ *janusv1alpha1.EventsRequest, s grpc.ServerStreamingServer[janusv1alpha1.Event]) error {
	for i := uint64(1); i <= 2; i++ {
		if err := s.Send(&janusv1alpha1.Event{Id: i, Type: "test.event"}); err != nil {
			return err
		}
	}
	<-s.Context().Done()
	return nil
}

func (f fakeSystem) Audit(req *janusv1alpha1.EventsRequest, s grpc.ServerStreamingServer[janusv1alpha1.Event]) error {
	return f.Events(req, s)
}

type fakeHAProxy struct {
	janusv1alpha1.UnimplementedHAProxyServiceServer
}

func (fakeHAProxy) GetConfig(context.Context, *emptypb.Empty) (*janusv1alpha1.GetConfigResponse, error) {
	return &janusv1alpha1.GetConfigResponse{Config: []byte("listen stats\n    stats auth admin:hunter2\n")}, nil
}

func (fakeHAProxy) Stats(context.Context, *emptypb.Empty) (*janusv1alpha1.HAProxyStatsResponse, error) {
	return &janusv1alpha1.HAProxyStatsResponse{RawCsv: []byte("# pxname,svname\nweb,FRONTEND\n")}, nil
}

func node(t *testing.T, sys fakeSystem) *grpc.ClientConn {
	t.Helper()
	ln := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	janusv1alpha1.RegisterSystemServiceServer(srv, sys)
	janusv1alpha1.RegisterHAProxyServiceServer(srv, fakeHAProxy{})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///node", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return ln.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// unpack reads a bundle: its files by name.
func unpack(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
}

func section(m Manifest, file string) Section {
	for _, s := range m.Sections {
		if s.File == file {
			return s
		}
	}
	return Section{}
}

func collect(t *testing.T, c *Collector) (map[string]string, Manifest) {
	t.Helper()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Write(ctx, &out); err != nil {
		t.Fatal(err)
	}
	files := unpack(t, out.Bytes())
	var m Manifest
	if err := json.Unmarshal([]byte(files["MANIFEST.json"]), &m); err != nil {
		t.Fatalf("MANIFEST.json: %v", err)
	}
	return files, m
}

// TestAdminBundle: everything the node has, nothing secret.
func TestAdminBundle(t *testing.T) {
	files, m := collect(t, &Collector{Conn: node(t, fakeSystem{}), Node: "edge-1", Role: "os:admin", By: "test"})
	if m.Format != Format || m.Node != "edge-1" || m.Role != "os:admin" {
		t.Errorf("manifest = %+v", m)
	}
	for _, want := range []string{"README.txt", "version.json", "system/services.json", "haproxy/haproxy.cfg", "haproxy/stat.csv", "events.jsonl", "audit.jsonl",
		"logs/janusd.log", "logs/haproxy.boot-1.log", "kernel/dmesg.log", "kernel/dmesg.boot-1.log", "kernel/avc-denials.log", "MANIFEST.json"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s missing; files: %v", want, keys(files))
		}
	}
	all := strings.Join(values(files), "\n")
	for _, secret := range []string{"hunter2", "MHcCAQEEIBOGUS", "BEGIN EC PRIVATE KEY"} {
		if strings.Contains(all, secret) {
			t.Errorf("the bundle holds %q", secret)
		}
	}
	if !strings.Contains(files["haproxy/haproxy.cfg"], "stats auth admin:<redacted>") {
		t.Errorf("haproxy.cfg = %q", files["haproxy/haproxy.cfg"])
	}
	if !strings.Contains(files["logs/janusd.log"], "janusd says hello") || !strings.Contains(files["logs/janusd.log"], "[private key removed from the bundle]") {
		t.Errorf("janusd.log = %q", files["logs/janusd.log"])
	}
	if strings.Count(files["events.jsonl"], "\n") != 2 || !strings.Contains(files["kernel/avc-denials.log"], "avc:  denied") || strings.Contains(files["kernel/avc-denials.log"], "booting") {
		t.Errorf("events %q, denials %q", files["events.jsonl"], files["kernel/avc-denials.log"])
	}
	if s := section(m, "network/status.json"); s.Status != "skipped" || s.Reason != "not in this node's version" {
		t.Errorf("a service the node doesn't have: %+v", s)
	}
	if s := section(m, "logs/janusd.log"); s.Status != "included" || s.Method != "SystemService/Logs" || s.Bytes == 0 {
		t.Errorf("janusd.log's section: %+v", s)
	}
}

// TestReaderBundle: what a reader may read, the rest said to be left out
// - and never asked for.
func TestReaderBundle(t *testing.T) {
	var asked []string
	allowed := func(method string) bool {
		ok := rbac.Allowed("/janus.v1alpha1."+method, []string{"os:reader"})
		if ok {
			asked = append(asked, method)
		}
		return ok
	}
	files, m := collect(t, &Collector{Conn: node(t, fakeSystem{}), Allowed: allowed, Role: "os:reader"})
	for _, absent := range []string{"logs/janusd.log", "kernel/dmesg.log", "audit.jsonl"} {
		if _, ok := files[absent]; ok {
			t.Errorf("a reader's bundle holds %s", absent)
		}
	}
	for _, f := range []string{"logs/", "kernel/dmesg.log", "audit.jsonl"} {
		if s := section(m, f); s.Status != "skipped" || s.Reason != "not for this role" {
			t.Errorf("%s: %+v", f, s)
		}
	}
	if _, ok := files["events.jsonl"]; !ok {
		t.Error("a reader's bundle lacks the events")
	}
	for _, a := range asked {
		if a == "SystemService/Logs" || a == "SystemService/Dmesg" || a == "SystemService/Audit" {
			t.Errorf("%s asked for a reader", a)
		}
	}
}

// TestTruncated: a file past MaxFile keeps its end, from a line's start.
func TestTruncated(t *testing.T) {
	long := strings.Repeat("an old line\n", 2000) + "the last line\n"
	files, m := collect(t, &Collector{Conn: node(t, fakeSystem{logLine: long}), MaxFile: 1024})
	log := files["logs/janusd.log"]
	if len(log) > 1024 || !strings.HasPrefix(log, "an old line") || !strings.Contains(log, "the last line") {
		t.Errorf("janusd.log (%d bytes) = %q...", len(log), log[:min(len(log), 40)])
	}
	if s := section(m, "logs/janusd.log"); s.Status != "truncated" {
		t.Errorf("section = %+v", s)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func values(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
