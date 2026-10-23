package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// signCSR is a CA's answer to csrPEM: a chain for the request's key and
// names, valid 90 days.
func signCSR(t *testing.T, csrPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("not a CSR: %q", csrPEM)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("the CSR's signature: %v", err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour * 24 * 365), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	ca, _ = x509.ParseCertificate(caDER)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

func applyOrdered(t *testing.T, m *Manager) {
	t.Helper()
	cfg := testConfig()
	cfg.Path = orderedPath(testThumb)
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply: %v %v", errs, err)
	}
}

// TestOrderedByTheController: the node orders nothing itself; HAProxy
// answers HTTP-01 with the Controller's account; a request's key stays
// on the node until its certificate comes, which is checked, written,
// swapped in; a renewal asked for waits for the Controller.
func TestOrderedByTheController(t *testing.T) {
	m, hp := setup(t)
	own := m.Thumbprint()
	hp.mu.Lock()
	hp.started = time.Now().Add(-time.Minute)
	hp.loaded[certPath("site")] = true // HAProxy loaded the stand-in: a new certificate is swapped in
	hp.mu.Unlock()
	applyOrdered(t, m)
	if env := m.Env(); len(env) != 1 || env[0] != ThumbprintEnv+"="+testThumb {
		t.Errorf("HAProxy's environment: %v", env)
	}
	if st := m.Status(); st.AccountThumbprint != testThumb || st.PathMode != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_ORDERED || st.AccountUri != "" {
		t.Errorf("status: %q %v %q", st.AccountThumbprint, st.PathMode, st.AccountUri)
	}
	for deadline := time.Now().Add(5 * time.Second); hp.reloadCount() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("HAProxy wasn't reloaded for the thumbprint it answers HTTP-01 with")
		}
	}
	m.pass()
	if b := readBundle(t, "site"); !b.Placeholder {
		t.Fatal("the node ordered a certificate itself")
	}
	if res := m.CheckPath(nil, "", ""); res.Ok || !strings.Contains(res.Error, "the Controller orders") {
		t.Errorf("check-path: %+v", res)
	}
	if _, err := m.Renew([]string{"site"}); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); !st.Certificates[0].InProgress {
		t.Error("a renewal asked for isn't shown, for the Controller to see")
	}

	if _, err := m.InstallCertificate("site", "-----BEGIN CERTIFICATE-----\n", "", time.Time{}); err == nil || !strings.Contains(err.Error(), "no request is waiting") {
		t.Errorf("installed without a request: %v", err)
	}
	if _, err := m.CertificateRequest("nope"); !errors.Is(err, ErrNoCertificate) {
		t.Errorf("a certificate not configured: %v", err)
	}
	req, err := m.CertificateRequest("site")
	if err != nil {
		t.Fatal(err)
	}
	if req.KeyType != "ec256" || len(req.Domains) != 2 {
		t.Errorf("request: %+v", req)
	}
	if data, err := os.ReadFile(pendingPath("site")); err != nil || !strings.Contains(string(data), "PRIVATE KEY") {
		t.Fatalf("the request's key isn't kept: %v", err)
	}
	// Another request's chain doesn't fit this one's key.
	other, err := m.CertificateRequest("site")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.InstallCertificate("site", signCSR(t, req.CsrPem), "", time.Time{}); err == nil {
		t.Error("a chain for a replaced request was installed")
	}
	st, err := m.InstallCertificate("site", signCSR(t, other.CsrPem), "", time.Time{})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if st.State != "valid" || st.InProgress || st.Serial == "" {
		t.Errorf("installed: %+v", st)
	}
	hp.mu.Lock()
	swapped := slices.Contains(hp.swapped, certPath("site"))
	hp.mu.Unlock()
	if b := readBundle(t, "site"); b.Placeholder || !swapped {
		t.Error("the certificate wasn't written and swapped in")
	}
	if _, err := os.Stat(pendingPath("site")); !os.IsNotExist(err) {
		t.Error("the request's key is still there")
	}
	if m.Thumbprint() != own {
		t.Error("the node's own account key changed")
	}

	// A failure the Controller reports.
	next := time.Now().Add(time.Hour).Truncate(time.Second)
	st, err = m.InstallCertificate("wild", "", "urn:ietf:params:acme:error:rateLimited", next)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastError != "urn:ietf:params:acme:error:rateLimited" || st.Failures != 1 || st.NextAttemptUnix != next.Unix() {
		t.Errorf("failure: %+v", st)
	}
}

// TestCertificateRequestKeyTypes: a CSR for each key type, the names as
// configured, the common name only when it fits.
func TestCertificateRequestKeyTypes(t *testing.T) {
	m, _ := setup(t)
	long := strings.Repeat("a", 60) + ".example.com"
	cfg := testConfig()
	cfg.Path = orderedPath(testThumb)
	cfg.Certificates = nil
	for _, kt := range []string{"ec256", "ec384", "rsa2048"} {
		cfg.Certificates = append(cfg.Certificates, &janusv1alpha1.ACMECertificate{Name: kt, Domains: []string{kt + ".example.com", long}, KeyType: kt})
	}
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply: %v %v", errs, err)
	}
	for _, kt := range []string{"ec256", "ec384", "rsa2048"} {
		req, err := m.CertificateRequest(kt)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode([]byte(req.CsrPem))
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(csr.DNSNames, ",") != kt+".example.com,"+long || csr.Subject.CommonName != kt+".example.com" {
			t.Errorf("%s: names %v, CN %q", kt, csr.DNSNames, csr.Subject.CommonName)
		}
		if _, err := m.InstallCertificate(kt, signCSR(t, req.CsrPem), "", time.Time{}); err != nil {
			t.Errorf("%s: install: %v", kt, err)
		}
		if got := keyTypeOf(readBundle(t, kt).Leaf); got != kt {
			t.Errorf("%s: installed a %s key", kt, got)
		}
	}
	cfg = proto.Clone(m.Config()).(*janusv1alpha1.ACMEConfig)
	cfg.Certificates = append(cfg.Certificates, &janusv1alpha1.ACMECertificate{Name: "longcn", Domains: []string{long}})
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply: %v %v", errs, err)
	}
	req, err := m.CertificateRequest("longcn")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(req.CsrPem))
	if csr, _ := x509.ParseCertificateRequest(block.Bytes); csr.Subject.CommonName != "" {
		t.Errorf("a %d-character common name", len(csr.Subject.CommonName))
	}
}

// TestOrderedPathValidated: the Controller's thumbprint is needed, no
// proxy goes with it, and leaving the mode gives HAProxy the node's own
// thumbprint back.
func TestOrderedPathValidated(t *testing.T) {
	for _, c := range []struct {
		p  *janusv1alpha1.ACMEPath
		ok bool
	}{
		{orderedPath(testThumb), true},
		{orderedPath(""), false},
		{orderedPath("not a thumbprint"), false},
		{&janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_ORDERED, AccountThumbprint: testThumb, ProxyUrl: "http://p:3128"}, false},
		{&janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: "http://p:3128", AccountThumbprint: testThumb}, false},
	} {
		if errs := validatePath(normalizePath(c.p)); (len(errs) == 0) != c.ok {
			t.Errorf("%v: %v", c.p, errs)
		}
	}
	m, _ := setup(t)
	applyOrdered(t, m)
	cfg := proto.Clone(m.Config()).(*janusv1alpha1.ACMEConfig)
	cfg.Path = nil
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply: %v %v", errs, err)
	}
	if env := m.Env(); env[0] != ThumbprintEnv+"="+m.Thumbprint() {
		t.Errorf("back to directly: %v", env)
	}
}

// TestOrderedDNSProviders: ordered by the Controller, a dns-01
// certificate names one of the Controller's providers - the node needs
// none of its own.
func TestOrderedDNSProviders(t *testing.T) {
	cfg := testConfig()
	cfg.DnsProviders = nil
	normalize(cfg)
	if errs := Validate(cfg); len(errs) == 0 {
		t.Error("a dns-01 certificate without its provider passed, the node ordering")
	}
	cfg.Path = orderedPath(testThumb)
	if errs := Validate(cfg); len(errs) > 0 {
		t.Errorf("ordered by the Controller: %v", errs)
	}
}
