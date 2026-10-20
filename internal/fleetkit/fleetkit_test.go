package fleetkit

import (
	"bytes"
	"errors"
	"io"
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

// scryptWorkFactor reads the work factor an armored age file's header
// names: "-> scrypt <salt> <logN>".
func scryptWorkFactor(t *testing.T, kit []byte) string {
	t.Helper()
	head := make([]byte, 256)
	n, _ := io.ReadFull(armor.NewReader(bytes.NewReader(bytes.TrimSpace(kit))), head)
	for _, line := range strings.Split(string(head[:n]), "\n") {
		if f := strings.Fields(line); len(f) == 4 && f[0] == "->" && f[1] == "scrypt" {
			return f[3]
		}
	}
	t.Fatalf("no scrypt stanza in %q", head[:n])
	return ""
}

// A kit made outside the tests' setting is at age's default work
// factor, 18 - the tests lower it (TestMain), production never does.
func TestDefaultWorkFactor(t *testing.T) {
	SetWorkFactorForTests(DefaultWorkFactor)
	t.Cleanup(func() { SetWorkFactorForTests(10) })
	kit, err := Make(Kit{Name: "f"}, "pass phrase")
	if err != nil {
		t.Fatal(err)
	}
	if got := scryptWorkFactor(t, kit); got != "18" {
		t.Fatalf("kit at the default work factor says %s, want 18", got)
	}
	SetWorkFactorForTests(10)
	kit, _ = Make(Kit{Name: "f"}, "pass phrase")
	if got := scryptWorkFactor(t, kit); got != "10" {
		t.Fatalf("kit for the tests says %s, want 10", got)
	}
}
