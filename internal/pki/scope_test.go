package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"slices"
	"testing"
	"time"
)

func scopedLeaf(t *testing.T, ca *CA, s *Scope) (*x509.Certificate, tls.Certificate) {
	t.Helper()
	certPEM, keyPEM, err := ca.Issue(IssueOptions{CommonName: "web-dev", Scope: s, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	der, _ := pemDecode(t, certPEM)
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(append(certPEM, ca.CertPEM...), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return leaf, pair
}

// TestScope: a scoped certificate carries no role of its own, and what
// it may do on every node and on the nodes it names, by their CA's key -
// the same key through a cross-signed certificate.
func TestScope(t *testing.T) {
	tf := newTestFleet(t)
	nodeA, _ := NewCA("node a")
	nodeB, _ := NewCA("node b")
	web := []ScopePerm{{Role: RoleOperator, Domains: []string{"haproxy"}}}
	leaf, _ := scopedLeaf(t, tf.issuing, &Scope{Any: []ScopePerm{{Role: RoleReader}}, Nodes: []ScopeNodes{{CAKeys: [][]byte{CAKey(nodeA.Cert)}, Perms: web}}})
	if !slices.Equal(leaf.Subject.Organization, []string{RoleScoped}) {
		t.Errorf("Organization %v", leaf.Subject.Organization)
	}
	s, ok, err := ParseScope(leaf)
	if err != nil || !ok {
		t.Fatalf("parse: %v %v", ok, err)
	}
	if got := s.For(CAKey(nodeA.Cert)); len(got) != 2 || got[0].Role != RoleReader || got[1].Role != RoleOperator || !slices.Equal(got[1].Domains, []string{"haproxy"}) {
		t.Errorf("node a: %+v", got)
	}
	if got := s.For(CAKey(nodeB.Cert)); len(got) != 1 || got[0].Role != RoleReader || len(got[0].Domains) > 0 {
		t.Errorf("node b: %+v", got)
	}
	if roles := s.Roles(); !slices.Equal(roles, []string{RoleReader, RoleOperator}) || s.Count() != 1 {
		t.Errorf("roles %v, count %d", roles, s.Count())
	}
	// A rotated CA: the cross-signed certificate the Controller pins has
	// the new CA's key.
	next, _ := NewCA("node a, rotated")
	crossPEM, err := nodeA.CrossSign(next)
	if err != nil {
		t.Fatal(err)
	}
	crossDER, _ := pemDecode(t, crossPEM)
	cross, err := x509.ParseCertificate(crossDER)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(CAKey(cross), CAKey(next.Cert)) || bytes.Equal(CAKey(next.Cert), CAKey(nodeA.Cert)) {
		t.Error("CAKey isn't the CA's key")
	}
	// Without a scope: none.
	plainPEM, _, _ := tf.issuing.Issue(IssueOptions{CommonName: "x", Roles: []string{RoleAdmin}})
	der, _ := pemDecode(t, plainPEM)
	plain, _ := x509.ParseCertificate(der)
	if _, ok, err := ParseScope(plain); ok || err != nil {
		t.Errorf("a plain certificate's scope: %v %v", ok, err)
	}
	// Refused when made: another role, a key that isn't a SHA-256, too
	// many nodes.
	many := make([][]byte, MaxScopeNodes+1)
	for i := range many {
		many[i] = make([]byte, 32)
	}
	for name, bad := range map[string]*Scope{
		"a Controller role": {Any: []ScopePerm{{Role: RoleController}}},
		"a short key":       {Nodes: []ScopeNodes{{CAKeys: [][]byte{{1, 2}}, Perms: web}}},
		"too many nodes":    {Nodes: []ScopeNodes{{CAKeys: many, Perms: web}}},
	} {
		if _, _, err := tf.issuing.Issue(IssueOptions{CommonName: "x", Scope: bad}); err == nil {
			t.Errorf("%s: issued", name)
		}
	}
}

// TestScopedHandshake: a node lets a scoped certificate of its fleet
// through TLS - its extension isn't critical -, and a limited issuing CA
// signs no scope giving more than its roles.
func TestScopedHandshake(t *testing.T) {
	tf := newTestFleet(t)
	local, _ := NewCA("node")
	limited := Fingerprint(tf.other.Cert.Raw)
	signed, err := SignBundle(tf.root, Bundle{Version: 1, Issued: time.Now(), IssuingCAs: []string{string(tf.issuing.CertPEM), string(tf.other.CertPEM)},
		Limits: map[string][]string{limited: {RoleOperator, RoleReader}}})
	if err != nil {
		t.Fatal(err)
	}
	fleet, _ := OpenFleet(t.TempDir())
	if err := fleet.Set(tf.root.CertPEM, signed); err != nil {
		t.Fatal(err)
	}
	srvPEM, srvKey, _ := local.Issue(IssueOptions{CommonName: "localhost", DNSNames: []string{"localhost"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	srv, _ := tls.X509KeyPair(srvPEM, srvKey)
	cfg := NodeTLSConfig(func() *CA { return local }, NewServerCert(local, t.TempDir(), srv), fleet)
	here := [][]byte{CAKey(local.Cert)}
	for _, c := range []struct {
		name string
		ca   *CA
		s    *Scope
		ok   bool
	}{
		{"the issuing CA's operator here", tf.issuing, &Scope{Nodes: []ScopeNodes{{CAKeys: here, Perms: []ScopePerm{{Role: RoleOperator}}}}}, true},
		{"the issuing CA's admin everywhere", tf.issuing, &Scope{Any: []ScopePerm{{Role: RoleAdmin}}}, true},
		{"the limited CA's operator", tf.other, &Scope{Nodes: []ScopeNodes{{CAKeys: here, Perms: []ScopePerm{{Role: RoleOperator}}}}}, true},
		{"the limited CA's admin elsewhere", tf.other, &Scope{Any: []ScopePerm{{Role: RoleReader}}, Nodes: []ScopeNodes{{CAKeys: [][]byte{make([]byte, 32)}, Perms: []ScopePerm{{Role: RoleAdmin}}}}}, false},
		{"the limited CA's empty scope", tf.other, &Scope{}, false},
	} {
		_, pair := scopedLeaf(t, c.ca, c.s)
		err := handshake(t, cfg, pair, local.CertPool())
		if (err == nil) != c.ok {
			t.Errorf("%s: let in %v (%v), want %v", c.name, err == nil, err, c.ok)
		}
	}
}
