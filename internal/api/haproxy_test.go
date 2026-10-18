package api

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/pki"
)

// testdata/show-stat.csv is real "show stat" output from this project's
// own haproxy build: a frontend, backend "web" with a health-checked
// server that's down and an unchecked one, and an empty backend.
func TestParseBackends(t *testing.T) {
	csv, err := os.ReadFile("testdata/show-stat.csv")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseBackends(string(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "web" || got[1].Name != "empty" {
		t.Fatalf("backends = %v", got)
	}
	web := got[0].Servers
	if len(web) != 2 {
		t.Fatalf("web servers = %v", web)
	}
	if web[0].Name != "web1" || web[0].Address != "127.0.0.1:1" || web[0].State != "down" {
		t.Errorf("web1 = %+v", web[0])
	}
	if web[1].Name != "web2" || web[1].Address != "10.9.9.9:80" || web[1].State != "no check" {
		t.Errorf("web2 = %+v", web[1])
	}
	if len(got[1].Servers) != 0 {
		t.Errorf("empty backend has servers: %v", got[1].Servers)
	}

	if _, err := parseBackends("no header\n"); err == nil {
		t.Error("CSV without a header parsed")
	}
}

// testdata/show-stat-socket-stats.csv is real "show stat" output (HAProxy
// 3.4.6) of issue #64's configuration: an HTTP frontend under `option
// socket-stats` - its two binds have rows of their own (type 3, named
// after the bind) - and a TCP frontend without it. Listeners are no
// backend's servers, and no frontend is a backend.
func TestParseBackendsSocketStats(t *testing.T) {
	csv, err := os.ReadFile("testdata/show-stat-socket-stats.csv")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseBackends(string(csv))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range got {
		s := b.Name + ":"
		for _, sv := range b.Servers {
			s += " " + sv.Name + "@" + sv.Address + "=" + sv.State
		}
		names = append(names, s)
	}
	want := []string{"be_web: web1@192.0.2.10:80=no check web2@192.0.2.11:80=maint", "be_relay: relay1@192.0.2.20:22067=no check"}
	if !slices.Equal(names, want) {
		t.Errorf("backends = %q, want %q", names, want)
	}

	// A show stat without the type column: as before, by svname.
	old := "# pxname,svname,status,addr,\nfe,FRONTEND,OPEN,,\nbe,web1,UP,10.0.0.1:80,\nbe,BACKEND,UP,,\n"
	if got, err := parseBackends(old); err != nil || len(got) != 1 || len(got[0].Servers) != 1 || got[0].Servers[0].Name != "web1" {
		t.Errorf("without a type column: %v, %v", got, err)
	}
}

// TestGetConfigRedactedForReaders: who may apply a configuration reads
// it whole; a reader - with its own certificate or through the
// Controller - gets it masked, with the hash of what it got
// (JANUS-2026-006).
func TestGetConfigRedactedForReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "haproxy.cfg")
	cfg := "userlist ops\n    user a insecure-password s3cret\nlisten stats\n    stats auth admin:hunter2\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &HAProxy{Manager: &haproxy.Manager{ConfigPath: path}}
	as := func(roles ...string) context.Context {
		return peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "u", Organization: roles}})
	}
	ctl := func(roles string) context.Context {
		return metadata.NewIncomingContext(as(pki.RoleController), metadata.Pairs(pki.AsUserKey, "carol", pki.AsRolesKey, roles))
	}
	for _, c := range []struct {
		name   string
		ctx    context.Context
		masked bool
	}{
		{"admin", as(pki.RoleAdmin), false},
		{"operator", as(pki.RoleOperator), false},
		{"reader", as(pki.RoleReader), true},
		{"reader through the Controller", ctl(pki.RoleReader), true},
		{"operator through the Controller", ctl(pki.RoleOperator), false},
	} {
		resp, err := h.GetConfig(c.ctx, &emptypb.Empty{})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := string(resp.GetConfig())
		if leaked := strings.Contains(got, "s3cret") || strings.Contains(got, "hunter2"); leaked == c.masked || resp.GetRedacted() != c.masked {
			t.Errorf("%s: redacted %v, got %q", c.name, resp.GetRedacted(), got)
		}
		if resp.GetSha256() != sha256Hex(resp.GetConfig()) {
			t.Errorf("%s: sha256 isn't the hash of what was returned", c.name)
		}
	}

	if err := os.WriteFile(path, []byte("global\n    maxconn 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, err := h.GetConfig(as(pki.RoleReader), &emptypb.Empty{}); err != nil || resp.GetRedacted() {
		t.Errorf("a configuration without a secret: redacted %v, %v", resp.GetRedacted(), err)
	}
}
