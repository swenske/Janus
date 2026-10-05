package fleetkit

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"filippo.io/age/armor"

	"github.com/swenske/Janus/internal/pki"
)

func TestKit(t *testing.T) {
	root, err := pki.NewCAFor("root", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := root.KeyPEM()
	pass, err := NewPassphrase()
	if err != nil || len(pass) != 29 || strings.Count(pass, "-") != 5 {
		t.Fatalf("passphrase %q, %v", pass, err)
	}
	kit, err := Make(Kit{Name: "lab", Created: time.Now(), RootCert: string(root.CertPEM), RootKey: string(key)}, pass)
	if err != nil || !bytes.HasPrefix(kit, []byte(armor.Header)) {
		t.Fatalf("make: %v %.40q", err, kit)
	}
	k, err := Open(kit, " "+strings.ToLower(pass)+"\n")
	if err != nil || k.Name != "lab" || k.Format != Format {
		t.Fatalf("open: %+v %v", k, err)
	}
	got, err := k.Root()
	if err != nil || !got.Cert.Equal(root.Cert) {
		t.Errorf("root: %v", err)
	}
	if _, err := Open(kit, "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"); !errors.Is(err, ErrKit) {
		t.Errorf("a wrong passphrase: %v", err)
	}
	if _, err := Open([]byte("hello"), pass); !errors.Is(err, ErrKit) {
		t.Errorf("not a kit: %v", err)
	}
}
