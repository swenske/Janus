package acme

import (
	"testing"
)

// A member another orders for holds the orderer's configuration and
// certificates: they read valid there, and the day it orders - the
// cluster's secrets come from it then - one still good isn't ordered
// again (v2026.10.25-3's lab: they read "due", and the new orderer
// re-issued them all at once).
func TestPulledCertificatesKept(t *testing.T) {
	m, _ := setup(t)
	m.Follow("lb-1", "thumb-of-lb-1")
	cfg := testConfig()
	cfg.Certificates = cfg.Certificates[:1]
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.InstallPulledConfig(data); err != nil {
		t.Fatal(err)
	}
	chain, key := issueTestCert([]string{"example.com", "www.example.com"}, defaultKeyType)
	if err := m.InstallPulled("site", []byte(chain+key)); err != nil {
		t.Fatal(err)
	}
	serial := serialHex(readBundle(t, "site").Leaf)
	if c := m.Status().Certificates[0]; c.State != "valid" || c.Serial != serial || c.RenewAtUnix == 0 {
		t.Errorf("the pulled certificate's status: %s, serial %s (want %s), renew at %d", c.State, c.Serial, serial, c.RenewAtUnix)
	}
	m.pass()
	if got := serialHex(readBundle(t, "site").Leaf); got != serial {
		t.Fatal("a member another orders for ordered it")
	}

	// It orders now: what it pulled is still good, nothing ordered.
	m.Follow("", "")
	m.pass()
	if got := serialHex(readBundle(t, "site").Leaf); got != serial {
		t.Error("the new orderer ordered again a certificate still good")
	}
	if c := m.Status().Certificates[0]; c.State != "valid" {
		t.Errorf("state %q once it orders", c.State)
	}
	// Asked for, it renews - without the orderer's certificate as the
	// one replaced (another account's).
	if _, err := m.Renew([]string{"site"}); err != nil {
		t.Fatal(err)
	}
	m.pass()
	if got := serialHex(readBundle(t, "site").Leaf); got == serial {
		t.Error("not renewed when asked")
	}
}
