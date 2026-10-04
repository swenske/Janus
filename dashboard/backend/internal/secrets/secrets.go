// Package secrets seals what the Controller keeps that must not be
// readable from its data directory alone - the fleet's issuing CA key -
// with its master key: AES-256-GCM, each value bound to what it is (its
// purpose, as additional data), so one can't be swapped for another.
//
// The master key is a file of its own, outside the data directory when
// JANUS_CONTROLLER_MASTER_KEY_FILE says where: a copy of the data
// directory (a backup) then holds nothing usable. Without it, the key is
// made next to the data - which protects nothing against a copy of that
// directory, and the Controller says so.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const keySize = 32

var magic = []byte("JSK1")

// Key is the Controller's master key.
type Key struct {
	aead cipher.AEAD
	// Path is where it was read from; BesideData that it's in the data
	// directory itself.
	Path       string
	BesideData bool
}

// LoadOrCreate reads the master key at path, or - when path is empty -
// at dataDir/master.key, making it the first time.
func LoadOrCreate(path, dataDir string) (*Key, error) {
	beside := path == ""
	if beside {
		path = filepath.Join(dataDir, "master.key")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		raw = make([]byte, keySize)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		// O_EXCL: two Controllers sharing a key file never both make one.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create the master key %s: %w", path, err)
		}
		if _, err := f.Write(raw); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("read the master key: %w", err)
	}
	if len(raw) != keySize {
		return nil, fmt.Errorf("the master key %s is %d bytes, not %d", path, len(raw), keySize)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Key{aead: aead, Path: path, BesideData: beside}, nil
}

// Seal encrypts plaintext for purpose.
func (k *Key) Seal(plaintext []byte, purpose string) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(append([]byte{}, magic...), nonce...)
	return k.aead.Seal(out, nonce, plaintext, []byte(purpose)), nil
}

// ErrSealed is a value this key didn't seal for that purpose.
var ErrSealed = errors.New("not sealed by this Controller's master key for this purpose")

// Open decrypts what Seal made for purpose.
func (k *Key) Open(sealed []byte, purpose string) ([]byte, error) {
	n := len(magic) + k.aead.NonceSize()
	if len(sealed) < n || !bytes.Equal(sealed[:len(magic)], magic) {
		return nil, ErrSealed
	}
	plain, err := k.aead.Open(nil, sealed[len(magic):n], sealed[n:], []byte(purpose))
	if err != nil {
		return nil, ErrSealed
	}
	return plain, nil
}
