package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetFleet: once a node trusts the fleet, its service credential is
// deleted - on disk and in memory - and it reloads as trusting it;
// SetCA records a new pin.
func TestSetFleet(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{Name: "lb1", Address: "192.0.2.10:9505", Port: 9500, CACertPEM: []byte("old CA"), ServiceCertPEM: []byte("cert"), ServiceKeyPEM: []byte("key")}
	if err := s.Add(n); err != nil {
		t.Fatal(err)
	}
	if n.TrustsFleet() {
		t.Fatal("a new node trusts the fleet")
	}
	if err := s.SetFleet(n.ID); err != nil {
		t.Fatal(err)
	}
	if cert, key := n.ServiceCredential(); !n.TrustsFleet() || cert != nil || key != nil {
		t.Errorf("in memory: fleet %v, credential kept %v", n.TrustsFleet(), cert != nil || key != nil)
	}
	for _, f := range []string{"service.crt", "service.key"} {
		if _, err := os.Stat(filepath.Join(dir, "nodes", n.ID, f)); !os.IsNotExist(err) {
			t.Errorf("%s still on disk: %v", f, err)
		}
	}
	if err := s.SetCA(n.ID, []byte("new CA")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAddress(n.ID, "192.0.2.11:9505"); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := again.Get(n.ID)
	if !ok || !r.TrustsFleet() || string(r.CA()) != "new CA" || r.Addr() != "192.0.2.11:9505" {
		t.Fatalf("reloaded: %+v", r)
	}
}
