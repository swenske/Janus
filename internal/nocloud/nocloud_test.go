package nocloud

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"

	"github.com/swenske/Janus/internal/pki"
)

// buildVolume creates a whole-disk (no partition table - the real
// convention a NoCloud volume uses, see FindVolume's own doc comment)
// vfat filesystem with the given label, and drops files into it -
// go-diskfs auto-detects FAT before ext4/iso9660 in GetFilesystem(0),
// so this exercises the same code path a real ISO9660 volume would,
// without needing xorriso/genisoimage available in this environment.
func buildVolume(t *testing.T, label string, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cidata.img")

	d, err := diskfs.Create(path, 8*1024*1024, diskfs.SectorSize(512))
	if err != nil {
		t.Fatalf("diskfs.Create: %v", err)
	}
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeFat32, VolumeLabel: label})
	if err != nil {
		t.Fatalf("CreateFilesystem: %v", err)
	}
	for name, content := range files {
		f, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("OpenFile %s: %v", name, err)
		}
		n, err := f.Write([]byte(content))
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if n != len(content) {
			t.Fatalf("write %s: short write, wrote %d of %d bytes", name, n, len(content))
		}
	}
	return path
}

func TestFindVolumeMatchesLabelCaseInsensitively(t *testing.T) {
	path := buildVolume(t, "CIDATA", map[string]string{"user-data": `{"controller_address":"h:1","controller_ca_cert":"ca"}`})

	got, err := FindVolume([]string{path})
	if err != nil {
		t.Fatalf("FindVolume: %v", err)
	}
	if got != path {
		t.Errorf("FindVolume = %q, want %q", got, path)
	}
}

func TestFindVolumeSkipsUnlabeledVolumes(t *testing.T) {
	path := buildVolume(t, "SOMETHING-ELSE", map[string]string{"user-data": "{}"})

	_, err := FindVolume([]string{path})
	if err != ErrNotFound {
		t.Errorf("FindVolume = %v, want ErrNotFound", err)
	}
}

func TestFindVolumeSkipsUnopenableCandidates(t *testing.T) {
	_, err := FindVolume([]string{"/nonexistent/path/does/not/exist"})
	if err != ErrNotFound {
		t.Errorf("FindVolume = %v, want ErrNotFound", err)
	}
}

func TestReadLocalUserData(t *testing.T) {
	path := buildVolume(t, "cidata", map[string]string{
		"user-data": `{"controller_address":"controller.example.com:8443","controller_ca_cert":"fake-ca-pem"}`,
	})

	cfg, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cfg.ControllerAddress != "controller.example.com:8443" {
		t.Errorf("ControllerAddress = %q", cfg.ControllerAddress)
	}
	if string(cfg.ControllerCACert) != "fake-ca-pem" {
		t.Errorf("ControllerCACert = %q", cfg.ControllerCACert)
	}
}

func TestParseUserDataRegistrationToken(t *testing.T) {
	cfg, err := parseUserData([]byte(`{"controller_address":"c:8443","controller_ca_cert":"pem","registration_token":"tok"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistrationToken != "tok" {
		t.Errorf("RegistrationToken = %q, want tok", cfg.RegistrationToken)
	}
	// A token is presented to a Controller - meaningless without one.
	if _, err := parseUserData([]byte(`{"registration_token":"tok","network":{"hostname":"lb1"}}`)); err == nil {
		t.Error("registration_token without a Controller: accepted")
	}
}

func TestParseUserDataFleetRoot(t *testing.T) {
	cfg, err := parseUserData([]byte(`{"controller_address":"c:8443","controller_ca_cert":"pem","controller_fleet_root_cert":"root"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.ControllerFleetRoot) != "root" {
		t.Errorf("ControllerFleetRoot = %q, want root", cfg.ControllerFleetRoot)
	}
	if cfg, err := parseUserData([]byte(`{"controller_address":"c:8443","controller_ca_cert":"pem"}`)); err != nil || cfg.ControllerFleetRoot != nil {
		t.Errorf("without one: %q, %v", cfg.ControllerFleetRoot, err)
	}
}

func TestParseUserDataNetwork(t *testing.T) {
	cfg, err := parseUserData([]byte(`{"network": {"hostname": "lb1", "interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24"], "gateway": "192.0.2.1"}], "ntp": {"servers": ["ntp.example.net"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControllerAddress != "" || cfg.Network.GetHostname() != "lb1" || cfg.Network.GetInterfaces()[0].GetGateway() != "192.0.2.1" || cfg.Network.GetNtp().GetServers()[0] != "ntp.example.net" {
		t.Errorf("parsed %+v", cfg)
	}
	for name, doc := range map[string]string{
		"empty":             `{}`,
		"invalid network":   `{"network": {"hostname": "not a hostname"}}`,
		"unknown field":     `{"network": {"hostnam": "lb1"}}`,
		"controller halves": `{"controller_ca_cert": "pem", "network": {}}`,
	} {
		if _, err := parseUserData([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReadRejectsIncompleteUserData(t *testing.T) {
	path := buildVolume(t, "cidata", map[string]string{
		"user-data": `{"controller_address":"h:1"}`, // missing controller_ca_cert
	})

	if _, err := Read(path); err == nil {
		t.Error("expected an error for user-data missing controller_ca_cert")
	}
}

func TestReadSeedFromModeA_PlainHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"controller_address":"from-seedfrom:8443","controller_ca_cert":"seedfrom-ca"}`))
	}))
	defer srv.Close()

	path := buildVolume(t, "cidata", map[string]string{
		"meta-data": `{"seedfrom":"` + srv.URL + `"}`,
	})

	cfg, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if cfg.ControllerAddress != "from-seedfrom:8443" {
		t.Errorf("ControllerAddress = %q, want fetched value", cfg.ControllerAddress)
	}
}

func TestReadSeedFromModeC_PinnedCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"controller_address":"pinned:8443","controller_ca_cert":"ca"}`))
	}))
	defer srv.Close()

	caPEM := certToPEM(t, srv.Certificate())
	path := buildVolume(t, "cidata", map[string]string{
		"meta-data": `{"seedfrom":"` + srv.URL + `","seedfrom_ca_cert":"` + escapeJSON(caPEM) + `"}`,
	})

	cfg, err := Read(path)
	if err != nil {
		t.Fatalf("Read with pinned CA: %v", err)
	}
	if cfg.ControllerAddress != "pinned:8443" {
		t.Errorf("ControllerAddress = %q", cfg.ControllerAddress)
	}
}

func TestReadSeedFromModeC_WrongCAFailsHandshake(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"controller_address":"pinned:8443","controller_ca_cert":"ca"}`))
	}))
	defer srv.Close()

	// A genuinely unrelated self-signed cert, not httptest's own fixed
	// default (httptest.NewTLSServer reuses the *same* static built-in
	// certificate across every server unless told otherwise - two
	// separate NewTLSServer calls would have the identical cert, not
	// "wrong" at all, a real mistake caught by this test's own first
	// draft passing when it should have failed).
	wrongCAPEM := generateUnrelatedSelfSignedCertPEM(t)

	path := buildVolume(t, "cidata", map[string]string{
		"meta-data": `{"seedfrom":"` + srv.URL + `","seedfrom_ca_cert":"` + escapeJSON(wrongCAPEM) + `"}`,
	})

	if _, err := Read(path); err == nil {
		t.Error("expected a TLS handshake failure against the wrong pinned CA")
	}
}

func generateUnrelatedSelfSignedCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "unrelated-test-ca"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestFetchUserDataRefusesHTTPWithPinnedCA(t *testing.T) {
	if _, err := fetchUserData("http://example.com/user-data", "some-ca-pem"); err == nil {
		t.Error("expected an error combining http:// with seedfrom_ca_cert")
	}
}

func TestFetchUserDataRefusesUnknownScheme(t *testing.T) {
	if _, err := fetchUserData("ftp://example.com/user-data", ""); err == nil {
		t.Error("expected an error for a non-http(s) scheme")
	}
}

func certToPEM(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

// escapeJSON round-trips s through encoding/json so it can be embedded
// verbatim inside a hand-written JSON literal in these tests - a PEM
// cert's embedded newlines aren't valid unescaped inside a JSON string.
func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1]) // strip the surrounding quotes json.Marshal added
}

func TestParseUserDataFleet(t *testing.T) {
	root, _ := pki.NewCAFor("root", time.Hour)
	issuing, _ := root.IssueCA("issuing", time.Hour)
	signed, err := pki.SignBundle(root, pki.Bundle{Version: 2, Issued: time.Now(), IssuingCAs: []string{string(issuing.CertPEM)}})
	if err != nil {
		t.Fatal(err)
	}
	rootJSON, _ := json.Marshal(string(root.CertPEM))
	asString, _ := json.Marshal(string(signed))
	// The bundle file's object as is, or that JSON as a string.
	for _, bundle := range []string{string(signed), string(asString)} {
		cfg, err := parseUserData([]byte(`{"fleet_root_cert":` + string(rootJSON) + `,"fleet_bundle":` + bundle + `}`))
		if err != nil {
			t.Fatalf("%.30s: %v", bundle, err)
		}
		if string(cfg.FleetRoot) != string(root.CertPEM) {
			t.Errorf("root %q", cfg.FleetRoot)
		}
		if _, err := pki.CheckFleet(cfg.FleetRoot, cfg.FleetBundle); err != nil {
			t.Errorf("the bundle read back: %v", err)
		}
	}
	other, _ := pki.NewCAFor("another root", time.Hour)
	otherJSON, _ := json.Marshal(string(other.CertPEM))
	if _, err := parseUserData([]byte(`{"fleet_root_cert":` + string(otherJSON) + `,"fleet_bundle":` + string(signed) + `}`)); err == nil {
		t.Error("a bundle another root signed was taken")
	}
	if _, err := parseUserData([]byte(`{"fleet_root_cert":` + string(rootJSON) + `}`)); err == nil {
		t.Error("a root without its bundle was taken")
	}
}
