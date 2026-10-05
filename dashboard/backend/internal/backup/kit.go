package backup

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// Kit is the backup kit opened: what decrypts the backups, and the key
// that checks them.
type Kit struct {
	Format     string    `json:"format"`
	Controller string    `json:"controller"`
	Created    time.Time `json:"created"`
	// Identity is the age identity the backups are encrypted to
	// (AGE-SECRET-KEY-1...).
	Identity string `json:"identity"`
	// SigningKey is the Controller's backup signing public key (base64).
	SigningKey string `json:"signing_key"`
}

// ErrKit is a kit or passphrase that doesn't open, or not a backup kit.
var ErrKit = errors.New("this backup kit doesn't open with this passphrase")

// NewKit makes a backup kit: a new age identity, its recipient (what the
// Controller keeps), and the kit - armored, encrypted with passphrase -
// holding it with signing's public key.
func NewKit(controller string, signing ed25519.PublicKey, passphrase string) (kit []byte, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, "", err
	}
	plain, err := json.MarshalIndent(Kit{Format: kitFormat, Controller: controller, Created: time.Now().UTC(), Identity: id.String(), SigningKey: base64.StdEncoding.EncodeToString(signing)}, "", "  ")
	if err != nil {
		return nil, "", err
	}
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, "", err
	}
	var out bytes.Buffer
	aw := armor.NewWriter(&out)
	w, err := age.Encrypt(aw, r)
	if err != nil {
		return nil, "", err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	if err := aw.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), id.Recipient().String(), nil
}

// OpenKit decrypts a backup kit (armored or not) with its passphrase.
func OpenKit(kit []byte, passphrase string) (*Kit, error) {
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
	if err := json.Unmarshal(plain, &k); err != nil || k.Format != kitFormat {
		return nil, ErrKit
	}
	return &k, nil
}

// Keys are the kit's identity and the signing key it trusts.
func (k *Kit) Keys() (age.Identity, ed25519.PublicKey, error) {
	id, err := age.ParseX25519Identity(k.Identity)
	if err != nil {
		return nil, nil, ErrKit
	}
	pub, err := base64.StdEncoding.DecodeString(k.SigningKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, nil, ErrKit
	}
	return id, ed25519.PublicKey(pub), nil
}
