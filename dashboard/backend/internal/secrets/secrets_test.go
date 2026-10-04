package secrets

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSealOpen: a value opens with the key and the purpose it was sealed
// for - not another purpose, not another key; the key is made once and
// read back after.
func TestSealOpen(t *testing.T) {
	dir := t.TempDir()
	k, err := LoadOrCreate("", dir)
	if err != nil || !k.BesideData || k.Path != filepath.Join(dir, "master.key") {
		t.Fatalf("beside the data: %+v, %v", k, err)
	}
	sealed, err := k.Seal([]byte("the issuing CA's key"), "fleet/issuing.key")
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreate("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := again.Open(sealed, "fleet/issuing.key"); err != nil || string(plain) != "the issuing CA's key" {
		t.Errorf("reopened key: %q, %v", plain, err)
	}
	if _, err := k.Open(sealed, "another purpose"); !errors.Is(err, ErrSealed) {
		t.Errorf("another purpose: %v", err)
	}
	other, err := LoadOrCreate(filepath.Join(t.TempDir(), "secrets", "master.key"), dir)
	if err != nil || other.BesideData {
		t.Fatalf("a key file of its own: %+v, %v", other, err)
	}
	if _, err := other.Open(sealed, "fleet/issuing.key"); !errors.Is(err, ErrSealed) {
		t.Errorf("another key: %v", err)
	}
	if _, err := k.Open([]byte("junk"), "fleet/issuing.key"); !errors.Is(err, ErrSealed) {
		t.Errorf("junk: %v", err)
	}
}
