package clusters

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/cluster"
	"github.com/swenske/Janus/internal/pki"
)

// sealer is a master key that only checks the purpose comes back.
type sealer struct{}

func (sealer) Seal(p []byte, purpose string) ([]byte, error) {
	return append([]byte(purpose+"|"), p...), nil
}

func (sealer) Open(s []byte, purpose string) ([]byte, error) {
	if !bytes.HasPrefix(s, []byte(purpose+"|")) {
		return nil, errors.New("sealed for another purpose")
	}
	return s[len(purpose)+1:], nil
}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, sealer{})
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func withMembers(t *testing.T, s *Store, id string, members ...Member) {
	t.Helper()
	if _, err := s.Update(id, func(c *Cluster) error {
		c.Members = members
		c.SecretsFrom = members[0].Name
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStore(t *testing.T) {
	s, dir := open(t)
	c, err := s.Create("edge", 0, []string{"haproxy", "files"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PeersPort != DefaultPeersPort || len(c.ID) != 36 {
		t.Errorf("created: %+v", c)
	}
	if _, err := s.Create("edge", 0, nil); err == nil {
		t.Error("two clusters named edge")
	}
	if _, err := s.Create("Edge Cluster", 0, nil); err == nil {
		t.Error("a name that isn't a label")
	}
	if _, err := s.Create("other", 0, []string{"dns"}); err == nil {
		t.Error("an unknown domain")
	}
	withMembers(t, s, c.ID, Member{NodeID: "n1", Name: "lb-1", Address: "10.0.0.1"}, Member{NodeID: "n2", Name: "lb-2", Address: "10.0.0.2", Vars: map[string]string{"priority": "100"}})
	// A node is a member of one cluster.
	o, err := s.Create("other", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(o.ID, func(c *Cluster) error {
		c.Members = []Member{{NodeID: "n1", Name: "x", Address: "10.0.0.9"}}
		return nil
	}); err == nil || !strings.Contains(err.Error(), "member of cluster edge") {
		t.Errorf("a node in two clusters: %v", err)
	}
	if s.ClusterOf("n2") != c.ID || s.ClusterOf("n9") != "" {
		t.Error("ClusterOf")
	}
	// A refused change changes nothing.
	if _, err := s.Update(c.ID, func(c *Cluster) error {
		c.Members[1].Name = "lb-1"
		return nil
	}); err == nil {
		t.Error("two members named lb-1")
	}
	got, _ := s.Get(c.ID)
	if got.Members[1].Name != "lb-2" {
		t.Error("a refused change stuck")
	}
	// Copies: changing what Get returned changes nothing.
	got.Members[1].Vars["priority"] = "1"
	again, _ := s.Get(c.ID)
	if again.Members[1].Vars["priority"] != "100" {
		t.Error("Get returned the record itself")
	}
	// Reopened: the same.
	re, err := Open(dir, sealer{})
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := re.Get(c.ID); !ok || len(r.Members) != 2 || r.SecretsFrom != "lb-1" || len(re.List()) != 2 {
		t.Errorf("reopened: %+v %v", r, ok)
	}
	// State.
	if err := s.UpdateState(c.ID, func(st *State) {
		st.Members["lb-1"] = MemberState{Applied: 3, Drift: []string{"haproxy"}}
	}); err != nil {
		t.Fatal(err)
	}
	if st := re.State(c.ID); st.Members["lb-1"].Applied != 3 || st.Members["lb-1"].Drift[0] != "haproxy" {
		t.Errorf("state: %+v", st)
	}
	if err := s.Remove(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(c.ID); ok || s.ClusterOf("n1") != "" {
		t.Error("removed, still there")
	}
}

func TestCAAndSigning(t *testing.T) {
	s, dir := open(t)
	c, err := s.Create("edge", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := s.CA(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Sealed: another master key doesn't open it.
	other, _ := Open(dir, otherSealer{})
	if _, err := other.CA(c.ID); err == nil {
		t.Error("the CA opened with another master key")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	chain, err := SignMember(ca, csr, c.ID, "lb-1")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(chain)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// What the node's cluster store accepts as a member of this cluster.
	id, name, err := cluster.MemberIdentity(leaf)
	if err != nil || id != c.ID || name != "lb-1" || leaf.DNSNames[0] != "lb-1" {
		t.Errorf("member identity: %q %q %v", id, name, err)
	}
	if err := leaf.CheckSignatureFrom(ca.Cert); err != nil {
		t.Error("not signed by the cluster's CA")
	}
	if !strings.Contains(string(chain), strings.TrimSpace(string(ca.CertPEM))) {
		t.Error("the CA doesn't follow the certificate")
	}
	if _, err := SignMember(ca, []byte("nope"), c.ID, "lb-1"); err == nil {
		t.Error("garbage signed")
	}
	_ = pki.RoleClusterMember
}

type otherSealer struct{}

func (otherSealer) Seal(p []byte, purpose string) ([]byte, error) { return p, nil }
func (otherSealer) Open(s []byte, purpose string) ([]byte, error) {
	return nil, errors.New("not this key")
}

const keepalived = `vrrp_instance front {
    virtual_router_id 51
    priority {{ .Member.Vars.priority }}
    unicast_src_ip {{ .Member.Address }}
    unicast_peer {
    {{- range .Peers }}
        {{ .Address }}
    {{- end }}
    }
}
`

func TestVersionsAndRender(t *testing.T) {
	s, _ := open(t)
	c, _ := s.Create("edge", 0, []string{"haproxy", "files", "vrrp"})
	withMembers(t, s, c.ID,
		Member{NodeID: "n1", Name: "lb-1", Address: "10.0.0.1", Vars: map[string]string{"priority": "150"}},
		Member{NodeID: "n2", Name: "lb-2", Address: "10.0.0.2", Vars: map[string]string{"priority": "100"}},
		Member{NodeID: "n3", Name: "lb-3", Address: "10.0.0.3"})
	c, _ = s.Get(c.ID)
	for name, files := range map[string]map[string][]byte{
		"a private key":     {"files/certs/x.pem": []byte("-----BEGIN PRIVATE KEY-----\n")},
		"another path":      {"etc/passwd": []byte("x")},
		"a broken template": {"keepalived.conf": []byte("{{ .Member.Vars.priority ")},
	} {
		if _, err := s.NewVersion(c.ID, "admin", "", files); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	files := map[string][]byte{"haproxy.cfg": []byte("global\n"), "files/maps/hosts.map": []byte("a b\n"), "keepalived.conf": []byte(keepalived)}
	v1, err := s.NewVersion(c.ID, "admin", "first", files)
	if err != nil || v1.Version != 1 {
		t.Fatalf("v1: %+v %v", v1, err)
	}
	files["haproxy.cfg"] = []byte("global\n    maxconn 1000\n")
	v2, err := s.NewVersion(c.ID, "admin", "", files)
	if err != nil || v2.Version != 2 {
		t.Fatalf("v2: %+v %v", v2, err)
	}
	vs, _ := s.Versions(c.ID)
	if len(vs) != 2 || vs[0].Version != 2 || vs[1].Note != "first" {
		t.Errorf("versions: %+v", vs)
	}
	v, err := s.Version(c.ID, 2)
	if err != nil || string(v.Content["haproxy.cfg"]) != "global\n    maxconn 1000\n" {
		t.Fatalf("v2 content: %v", err)
	}
	if _, err := s.Version(c.ID, 9); !errors.Is(err, ErrNoVersion) {
		t.Errorf("v9: %v", err)
	}
	r, err := Render(v, c, "lb-2")
	if err != nil {
		t.Fatal(err)
	}
	vrrp := string(r.Modules["vrrp"])
	for _, want := range []string{"priority 100", "unicast_src_ip 10.0.0.2", "10.0.0.1", "10.0.0.3"} {
		if !strings.Contains(vrrp, want) {
			t.Errorf("lb-2's keepalived.conf lacks %q:\n%s", want, vrrp)
		}
	}
	if strings.Contains(vrrp, "        10.0.0.2") {
		t.Error("lb-2 is its own peer")
	}
	if string(r.Files["maps/hosts.map"]) != "a b\n" || string(r.HAProxy) != "global\n    maxconn 1000\n" {
		t.Errorf("rendered: %+v", r)
	}
	r1, _ := Render(v, c, "lb-1")
	if r1.Hashes()["vrrp"] == r.Hashes()["vrrp"] || r1.Hashes()["haproxy"] != r.Hashes()["haproxy"] {
		t.Error("hashes: VRRP differs per member, haproxy.cfg doesn't")
	}
	// A variable a member lacks: refused, not rendered empty.
	if _, err := Render(v, c, "lb-3"); err == nil || !strings.Contains(err.Error(), "priority") {
		t.Errorf("lb-3 without a priority: %v", err)
	}
	if _, err := Render(v, c, "lb-9"); err == nil {
		t.Error("rendered for a stranger")
	}
}

func TestJournal(t *testing.T) {
	s, _ := open(t)
	c, _ := s.Create("edge", 0, nil)
	for i := range journalMax + 5 {
		if err := s.Journal(c.ID, Entry{Actor: "cluster", Action: "apply", Version: i}); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Entries(c.ID, 3)
	if len(got) != 3 || got[0].Version != journalMax+4 || got[2].Version != journalMax+2 {
		t.Errorf("newest first: %+v", got)
	}
	if all := s.Entries(c.ID, 1<<20); len(all) > journalMax {
		t.Errorf("%d entries kept", len(all))
	}
}
