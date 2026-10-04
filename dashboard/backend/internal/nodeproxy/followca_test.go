package nodeproxy

import (
	"crypto/tls"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
)

// TestFollowCA: a node that replaced its CA is still reached - through
// its new CA, cross-signed by the one pinned - and the Controller pins
// the new one; it then follows the next replacement too.
func TestFollowCA(t *testing.T) {
	nodeDir := t.TempDir()
	b, err := pki.LoadOrBootstrap(nodeDir, "localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	sc := pki.NewServerCert(b.CA, nodeDir, b.ServerCert)
	local := pki.NewLocal(b.CA)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: sc.GetCertificate, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()

	storeDir := t.TempDir()
	st, err := store.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	client, key, err := b.CA.Issue(pki.IssueOptions{CommonName: "svc", Roles: []string{pki.RoleAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{Name: "lb1", Address: ln.Addr().String(), Port: 9500, CACertPEM: b.CA.CertPEM, ServiceCertPEM: client, ServiceKeyPEM: key}
	if err := st.Add(node); err != nil {
		t.Fatal(err)
	}
	prev := NodeCA
	NodeCA = func(n *store.Node, caPEM []byte) error { return st.SetCA(n.ID, caPEM) }
	t.Cleanup(func() { NodeCA = prev })

	dial := func(stage string) {
		t.Helper()
		cfg, err := nodeTLS(node, false)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ServerName = "localhost"
		conn, err := tls.Dial("tcp", node.Addr(), cfg)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		conn.Close()
	}
	dial("before any rotation")
	pinned := string(node.CA())
	for i := 1; i <= 2; i++ {
		if _, err := sc.Rotate(local, "localhost", nil, nil); err != nil {
			t.Fatal(err)
		}
		dial("right after a rotation")
		if string(node.CA()) == pinned {
			t.Fatalf("rotation %d: the Controller still pins the old CA", i)
		}
		pinned = string(node.CA())
		dial("with the new pin")
	}
	reopened, err := store.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := reopened.Get(node.ID); string(n.CA()) != pinned {
		t.Error("the new pin isn't kept on disk")
	}
}
