package acme

import (
	"os"
	"slices"
	"strings"
	"testing"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
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

// A pulled configuration without a certificate this node's haproxy.cfg
// still uses keeps that one - the rest, the secrets with it, installed -
// until haproxy.cfg no longer needs it (a cluster's version applied
// member by member: the member pulled from runs the new haproxy.cfg
// already, this one not yet).
func TestPulledConfigKeepsCertificatesInUse(t *testing.T) {
	m, hp := setup(t)
	if errs, err := m.Apply(testConfig(), "", false); err != nil {
		t.Fatal(errs, err)
	}
	hp.validate = func() (bool, []string) {
		if _, err := os.Stat(certPath("site")); err != nil {
			return false, []string{"unable to load " + certPath("site")}
		}
		return true, nil
	}
	pulled := testConfig()
	pulled.Certificates = pulled.Certificates[1:] // "site" gone
	pulled.DnsProviders[0].Settings["GANDIV5_PERSONAL_ACCESS_TOKEN"] = "new-token"
	data, err := Marshal(pulled)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.InstallPulledConfig(data); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	names := []string{}
	for _, c := range m.cfg.Certificates {
		names = append(names, c.Name)
	}
	token := m.cfg.DnsProviders[0].Settings["GANDIV5_PERSONAL_ACCESS_TOKEN"]
	m.mu.Unlock()
	if !slices.Contains(names, "site") || !slices.Contains(names, "wild") {
		t.Errorf("certificates after the pull: %v - site, still used, should stay", names)
	}
	if token != "new-token" {
		t.Errorf("the pulled secret wasn't installed: %q", token)
	}
	// No longer used: the next pull takes it away.
	hp.validate = nil
	if err := m.InstallPulledConfig(data); err != nil {
		t.Fatal(err)
	}
	if c := m.Config().Certificates; len(c) != 1 || c[0].Name != "wild" {
		t.Errorf("certificates once unused: %v", c)
	}
}

// Canonical is what a cluster's version keeps: normalized, without a
// secret; the same for two configurations that differ by their secrets.
func TestCanonical(t *testing.T) {
	a := testConfig()
	a.Path = &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: " http://proxy:3128 ", ProxyUsername: "acme", ProxyPassword: "pw"}
	a.Account.EabKeyId, a.Account.EabHmacKey = "kid", "hmac"
	b := testConfig()
	b.Path = &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: "http://proxy:3128", ProxyUsername: "acme"}
	b.Account.EabKeyId = "kid"
	b.DnsProviders[0].Settings["GANDIV5_PERSONAL_ACCESS_TOKEN"] = ""
	b.Certificates[0].Domains = []string{"example.com", "www.example.com"}
	b.Certificates[0].KeyType, b.Certificates[0].Challenge = "ec256", "http-01"
	ca, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Errorf("two configurations differing by their secrets:\n%s\n%s", ca, cb)
	}
	for _, s := range []string{"pw", "hmac", "secret-token", "Example.COM"} {
		if strings.Contains(string(ca), `"`+s+`"`) {
			t.Errorf("%q in the canonical form:\n%s", s, ca)
		}
	}
	if !strings.Contains(string(ca), "GANDIV5_PERSONAL_ACCESS_TOKEN") {
		t.Errorf("the setting's name is kept (its value empty):\n%s", ca)
	}
	if got := Secrets(a); !slices.Equal(got, []string{"account.eab_hmac_key", "path.proxy_password", "dns_providers[gandi].settings.GANDIV5_PERSONAL_ACCESS_TOKEN"}) {
		t.Errorf("Secrets: %v", got)
	}
	if got := Secrets(b); len(got) != 0 {
		t.Errorf("Secrets of a canonical one: %v", got)
	}
	back, err := ParseConfig(ca)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := Canonical(back); string(again) != string(ca) {
		t.Errorf("not a fixed point:\n%s\n%s", ca, again)
	}
	if _, err := ParseConfig([]byte(`{"certificates": [], "unknown": 1}`)); err == nil {
		t.Error("an unknown field accepted")
	}
}
