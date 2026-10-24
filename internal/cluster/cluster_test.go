package cluster

import (
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/pki"
)

const testID = "7f3c2a1e-5b6d-4c8e-9f01-23456789abcd"

// signMember is what an orchestrator does with a node's signing
// request: a member certificate of ca for it.
func signMember(t *testing.T, ca *pki.CA, csrPEM []byte, clusterID, member string) []byte {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatal("no CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	u, _ := url.Parse(MemberURI(clusterID, member))
	cert, err := ca.IssueFor(csr.PublicKey, pki.IssueOptions{
		CommonName: member, Roles: []string{pki.RoleClusterMember}, DNSNames: []string{member}, URIs: []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, Validity: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cluster"), func() string { return "lb-1" })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func membership(member string) Membership {
	return Membership{ID: testID, Name: "edge", Member: member, PeersPort: 10057, Members: []Member{
		{Name: "lb-1", Address: "10.0.20.11"}, {Name: "lb-2", Address: "10.0.20.12"}, {Name: "lb-3", Address: "10.0.20.13", API: "10.0.20.13:9505"},
	}}
}

func TestMembershipCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(m *Membership)
		want string
	}{
		{"sound", func(m *Membership) {}, ""},
		{"id", func(m *Membership) { m.ID = "edge" }, "lowercase UUID"},
		{"name", func(m *Membership) { m.Name = "Edge Cluster" }, "cluster name"},
		{"member name", func(m *Membership) { m.Members[1].Name = "lb_2" }, "member name"},
		{"duplicate", func(m *Membership) { m.Members[1].Name = "lb-1" }, "two members are named"},
		{"address", func(m *Membership) { m.Members[2].Address = "lb-3.example" }, "isn't an IP address"},
		{"api", func(m *Membership) { m.Members[2].API = "10.0.20.13" }, "host:port"},
		{"self", func(m *Membership) { m.Member = "lb-9" }, "isn't among the members"},
		{"port", func(m *Membership) { m.PeersPort = 70000 }, "1 to 65535"},
		{"managed", func(m *Membership) { m.Managed = []string{"dns"} }, "unknown managed domain"},
		{"none", func(m *Membership) { m.Members = nil }, "at least one member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := membership("lb-1")
			tc.edit(&m)
			err := m.Check()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestMemberURI(t *testing.T) {
	u, _ := url.Parse(MemberURI(testID, "lb-1"))
	id, name, ok := ParseMemberURI(u)
	if !ok || id != testID || name != "lb-1" {
		t.Fatalf("got %q %q %v", id, name, ok)
	}
	for _, bad := range []string{"janus://cluster/" + testID + "/node/lb-1", "https://cluster/" + testID + "/member/lb-1", "janus://cluster/edge/member/lb-1", "janus://cluster/" + testID + "/member/LB"} {
		u, _ := url.Parse(bad)
		if _, _, ok := ParseMemberURI(u); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestStandalone(t *testing.T) {
	s := newStore(t)
	if s.IsMember() || s.LocalPeer() != "lb-1" || s.ClusterID() != "" || s.CAs() != nil {
		t.Fatal("a fresh store is a member")
	}
	cfg := string(s.PeersConfig())
	for _, want := range []string{"-L lb-1.", "set-var proc.janus_member str(lb-1)", "peers janus\n    bind 127.0.0.1:10057\n    server lb-1\n"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q in:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "janus_cluster") {
		t.Error("a node that is no member names a cluster")
	}
	path := filepath.Join(t.TempDir(), PeersFile)
	changed, err := s.WritePeersFile(path)
	if err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	if changed, err = s.WritePeersFile(path); err != nil || changed {
		t.Fatalf("second write: %v %v", changed, err)
	}
	if _, err := s.AcceptMember(nil); err == nil {
		t.Error("a node that is no member accepts a member")
	}
}

func TestJoinRenewLeave(t *testing.T) {
	s := newStore(t)
	ca, err := pki.NewCA("Janus cluster edge")
	if err != nil {
		t.Fatal(err)
	}
	// The certificate before the key: refused.
	if _, _, err := s.Set(SetRequest{Membership: membership("lb-1"), CAPEM: ca.CertPEM, MemberPEM: ca.CertPEM}); err == nil || !strings.Contains(err.Error(), "signing request") {
		t.Fatalf("no key: %v", err)
	}
	csr, err := s.NewCSR()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, PendingKeyFile)); err != nil {
		t.Fatal("no pending key")
	}
	// A certificate for another member, another cluster, without the DNS name, of another CA: refused.
	other, _ := pki.NewCA("another")
	for name, cert := range map[string][]byte{
		"other member":  signMember(t, ca, csr, testID, "lb-2"),
		"other cluster": signMember(t, ca, csr, "00000000-0000-4000-8000-000000000000", "lb-1"),
		"other CA":      signMember(t, other, csr, testID, "lb-1"),
	} {
		if _, _, err := s.Set(SetRequest{Membership: membership("lb-1"), CAPEM: ca.CertPEM, MemberPEM: cert}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if s.IsMember() {
		t.Fatal("a refused Set made a member")
	}
	cert := signMember(t, ca, csr, testID, "lb-1")
	section, certChanged, err := s.Set(SetRequest{Membership: membership("lb-1"), CAPEM: ca.CertPEM, MemberPEM: cert})
	if err != nil || !section || !certChanged {
		t.Fatalf("join: %v %v %v", section, certChanged, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, PendingKeyFile)); err == nil {
		t.Error("the pending key stayed")
	}
	if !s.IsMember() || s.LocalPeer() != "lb-1" || s.ClusterID() != testID || len(s.CAs()) != 1 || s.MemberCertificate() == nil {
		t.Fatal("not a member after the join")
	}
	cfg := string(s.PeersConfig())
	for _, want := range []string{
		"set-var proc.janus_cluster str(" + testID + ")",
		"set-var proc.janus_member str(lb-1)",
		"bind 10.0.20.11:10057 ssl crt " + filepath.Join(s.Dir, MemberFile) + " ca-file " + filepath.Join(s.Dir, CAFile) + " verify required",
		"default-server ssl crt",
		"\n    server lb-1\n",
		"server lb-2 10.0.20.12:10057 verifyhost lb-2",
		"server lb-3 10.0.20.13:10057 verifyhost lb-3",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q in:\n%s", want, cfg)
		}
	}
	// What the files hold: the key with the certificate, 0600.
	memberPEM, err := os.ReadFile(filepath.Join(s.Dir, MemberFile))
	if err != nil || !strings.Contains(string(memberPEM), "PRIVATE KEY") {
		t.Fatalf("member.pem: %v", err)
	}
	if st, _ := os.Stat(filepath.Join(s.Dir, MemberFile)); st.Mode().Perm() != 0o600 {
		t.Errorf("member.pem mode %v", st.Mode().Perm())
	}
	// Another member's certificate, verified by the CA: accepted; another cluster's: refused.
	peerCSR, _ := newStore(t).NewCSR()
	leaf := parseLeaf(t, signMember(t, ca, peerCSR, testID, "lb-2"))
	if name, err := s.AcceptMember([]*x509.Certificate{leaf, ca.Cert}); err != nil || name != "lb-2" {
		t.Errorf("AcceptMember: %q %v", name, err)
	}
	stranger := parseLeaf(t, signMember(t, ca, peerCSR, "00000000-0000-4000-8000-000000000000", "lb-2"))
	if _, err := s.AcceptMember([]*x509.Certificate{stranger, ca.Cert}); err == nil {
		t.Error("another cluster's member accepted")
	}
	// Reopened: the same.
	re, err := Open(s.Dir, s.Hostname)
	if err != nil || !re.IsMember() || re.LocalPeer() != "lb-1" || string(re.PeersConfig()) != cfg {
		t.Fatalf("reopen: %v", err)
	}
	// A new member, no certificate: the section changes, not the certificate.
	m := membership("lb-1")
	m.Members = append(m.Members, Member{Name: "lb-4", Address: "10.0.20.14"})
	if section, certChanged, err = s.Set(SetRequest{Membership: m}); err != nil || !section || certChanged {
		t.Fatalf("add a member: %v %v %v", section, certChanged, err)
	}
	// The same again: nothing.
	if section, certChanged, err = s.Set(SetRequest{Membership: m}); err != nil || section || certChanged {
		t.Fatalf("same again: %v %v %v", section, certChanged, err)
	}
	// Renewal: a certificate for the current key, without a new request.
	csr2, _ := s.NewCSR()
	_ = os.Remove(filepath.Join(s.Dir, PendingKeyFile))
	if _, _, err := s.Set(SetRequest{Membership: m, MemberPEM: signMember(t, ca, csr2, testID, "lb-1")}); err == nil {
		t.Error("a certificate for a key the node no longer has accepted")
	}
	renewed, err := pkiReissue(ca, s.MemberCertificate())
	if err != nil {
		t.Fatal(err)
	}
	if section, certChanged, err = s.Set(SetRequest{Membership: m, MemberPEM: renewed}); err != nil || section || !certChanged {
		t.Fatalf("renew: %v %v %v", section, certChanged, err)
	}
	// Another cluster: refused until it leaves.
	o := membership("lb-1")
	o.ID = "00000000-0000-4000-8000-000000000000"
	if _, _, err := s.Set(SetRequest{Membership: o, CAPEM: ca.CertPEM}); err == nil || !strings.Contains(err.Error(), "leaves it first") {
		t.Fatalf("another cluster: %v", err)
	}
	if err := s.Leave(); err != nil {
		t.Fatal(err)
	}
	if s.IsMember() || s.LocalPeer() != "lb-1" {
		t.Fatal("still a member after leaving")
	}
	for _, name := range []string{MembershipFile, MemberFile, CAFile} {
		if _, err := os.Stat(filepath.Join(s.Dir, name)); err == nil {
			t.Errorf("%s stayed", name)
		}
	}
}

func parseLeaf(t *testing.T, p []byte) *x509.Certificate {
	t.Helper()
	leaf, _, err := parseChain(p)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// pkiReissue is a renewal: the same key, names and roles, a new
// certificate.
func pkiReissue(ca *pki.CA, cur *x509.Certificate) ([]byte, error) {
	return ca.IssueFor(cur.PublicKey, pki.IssueOptions{
		CommonName: cur.Subject.CommonName, Roles: cur.Subject.Organization, DNSNames: cur.DNSNames, URIs: cur.URIs,
		ExtKeyUsage: cur.ExtKeyUsage, Validity: 30 * 24 * time.Hour,
	})
}

func TestLockRelease(t *testing.T) {
	s := newStore(t)
	if _, err := s.Release(nil); err == nil {
		t.Error("a node in no cluster released something")
	}
	if _, _, locked := s.Lock("haproxy"); locked {
		t.Error("a node in no cluster is locked")
	}
	ca, _ := pki.NewCA("Janus cluster edge")
	csr, _ := s.NewCSR()
	m := membership("lb-1")
	m.Managed = []string{"haproxy", "files", "vrrp"}
	if _, _, err := s.Set(SetRequest{Membership: m, CAPEM: ca.CertPEM, MemberPEM: signMember(t, ca, csr, testID, "lb-1")}); err != nil {
		t.Fatal(err)
	}
	if id, name, locked := s.Lock("haproxy"); !locked || id != testID || name != "edge" {
		t.Errorf("haproxy: %q %q %v", id, name, locked)
	}
	if _, _, locked := s.Lock("bgp"); locked {
		t.Error("bgp is locked, unmanaged")
	}
	if _, err := s.Release([]string{"dns"}); err == nil {
		t.Error("an unknown domain released")
	}
	released, err := s.Release([]string{"haproxy"})
	if err != nil || len(released) != 1 || released[0] != "haproxy" {
		t.Fatalf("release haproxy: %v %v", released, err)
	}
	if _, _, locked := s.Lock("haproxy"); locked {
		t.Error("haproxy still locked")
	}
	// Kept across a restart.
	re, err := Open(s.Dir, s.Hostname)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, locked := re.Lock("haproxy"); locked {
		t.Error("haproxy locked again after a restart")
	}
	if _, _, locked := re.Lock("files"); !locked {
		t.Error("files no longer locked after a restart")
	}
	// Nothing left to release of these: nothing done.
	if released, err := s.Release([]string{"haproxy"}); err != nil || released != nil {
		t.Errorf("again: %v %v", released, err)
	}
	if released, err := s.Release(nil); err != nil || len(released) != 2 {
		t.Errorf("the rest: %v %v", released, err)
	}
}
