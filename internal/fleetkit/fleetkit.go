// Package fleetkit is a fleet's recovery kit: its root's certificate and
// key, encrypted with a passphrase - an armored age file (scrypt), text
// that fits a password manager's note and that `age -d` opens too. The
// Controller makes one when it sets its fleet up; janusctl fleet init
// makes one for a fleet without a Controller. Either opens with the
// other: the kit is what a fleet is.
package fleetkit

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/swenske/Janus/internal/pki"
)

// Format is what a kit says it is.
const Format = "janus-recovery-kit/1"

// Kit is a recovery kit opened.
type Kit struct {
	Format string `json:"format"`
	// Controller is the ID of the Controller that made it; empty for a
	// fleet janusctl made.
	Controller string `json:"controller,omitempty"`
	// Name is a fleet's name, as janusctl fleet init gave it.
	Name     string    `json:"name,omitempty"`
	Created  time.Time `json:"created"`
	RootCert string    `json:"root_cert"`
	RootKey  string    `json:"root_key"`
}

// ErrKit is a kit or passphrase that doesn't open, or not a kit.
var ErrKit = errors.New("this kit doesn't open with this passphrase, or isn't a fleet's recovery kit")

// Make encrypts k with passphrase.
func Make(k Kit, passphrase string) ([]byte, error) {
	k.Format = Format
	plain, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return nil, err
	}
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	aw := armor.NewWriter(&out)
	w, err := age.Encrypt(aw, r)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Open decrypts a kit (armored or not) with its passphrase - case and
// surrounding spaces don't matter.
func Open(kit []byte, passphrase string) (*Kit, error) {
	id, err := age.NewScryptIdentity(strings.ToUpper(strings.TrimSpace(passphrase)))
	if err != nil {
		return nil, ErrKit
	}
	var in io.Reader = bytes.NewReader(kit)
	if bytes.HasPrefix(bytes.TrimSpace(kit), []byte(armor.Header)) {
		in = armor.NewReader(bytes.NewReader(bytes.TrimSpace(kit)))
	}
	r, err := age.Decrypt(in, id)
	if err != nil {
		return nil, ErrKit
	}
	plain, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, ErrKit
	}
	var k Kit
	if err := json.Unmarshal(plain, &k); err != nil || k.Format != Format {
		return nil, ErrKit
	}
	return &k, nil
}

// Root is the kit's root CA, its key with it.
func (k *Kit) Root() (*pki.CA, error) {
	root, err := pki.LoadCA([]byte(k.RootCert), []byte(k.RootKey))
	if err != nil {
		return nil, ErrKit
	}
	return root, nil
}

// passphraseAlphabet is Crockford's base 32: no I, L, O or U to mistake.
const passphraseAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewPassphrase is six groups of four characters - 120 bits.
func NewPassphrase() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(passphraseAlphabet[int(c)%len(passphraseAlphabet)])
	}
	return sb.String(), nil
}
