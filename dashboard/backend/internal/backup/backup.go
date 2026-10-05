// Package backup is the Controller's backups: an archive of everything it
// keeps - and its nodes' configurations -, encrypted with age to whoever
// may restore it, with a manifest the Controller signs. The Controller
// keeps the public keys only: it writes backups and can't read them. A
// backup kit - the age identity that decrypts them and the key that
// checks their signature, encrypted with a passphrase - is what a
// restore needs; an admin's SSH or age key can decrypt them too.
//
// A backup is one file (and one S3 object):
//
//	janus-backup 1\n
//	{"payload": "<the manifest, JSON, base64>", "signature": "<Ed25519, base64>"}\n
//	<age-encrypted tar.gz>
//
// The manifest names the encrypted part's SHA-256: the signature covers
// all of it. age doesn't say who encrypted - without the signature, a
// file someone else put in the bucket would restore as well.
package backup

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/agessh"
)

const (
	magic     = "janus-backup 1"
	kitFormat = "janus-backup-kit-1"
)

// Manifest is what a backup says of itself.
type Manifest struct {
	Format     string    `json:"format"`
	Controller string    `json:"controller"`
	Version    string    `json:"version"`
	Created    time.Time `json:"created"`
	// SHA256 and Size are the encrypted archive's.
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Nodes is each node's configuration's fate: "" (saved) or why not.
	Nodes map[string]string `json:"nodes,omitempty"`
	// Skipped are files left out (too large).
	Skipped []string `json:"skipped,omitempty"`
}

type envelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// ParseRecipient reads one who may decrypt backups: an age public key
// (age1...), or an SSH one (ssh-ed25519, ssh-rsa).
func ParseRecipient(s string) (age.Recipient, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "age1"):
		return age.ParseX25519Recipient(s)
	case strings.HasPrefix(s, "ssh-"):
		return agessh.ParseRecipient(s)
	}
	return nil, errors.New("a recipient is an age public key (age1...) or an SSH one (ssh-ed25519, ssh-rsa)")
}

// Seal encrypts archive to recipients and signs it: the backup file.
func Seal(archive []byte, m Manifest, recipients []age.Recipient, signer ed25519.PrivateKey) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("nobody to encrypt the backup for")
	}
	var enc bytes.Buffer
	w, err := age.Encrypt(&enc, recipients...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(archive); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(enc.Bytes())
	m.Format, m.SHA256, m.Size = "janus-backup-1", hex.EncodeToString(sum[:]), int64(enc.Len())
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	env, err := json.Marshal(envelope{Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signer, payload))})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(magic + "\n")
	out.Write(env)
	out.WriteByte('\n')
	out.Write(enc.Bytes())
	return out.Bytes(), nil
}

// ErrSignature is a backup whose signature doesn't check: not made by the
// Controller whose key is given, or changed since.
var ErrSignature = errors.New("the backup's signature doesn't check: not this Controller's, or changed since")

// Open checks a backup file's signature with the Controller's key, and
// returns its manifest and encrypted part.
func Open(file []byte, signer ed25519.PublicKey) (*Manifest, []byte, error) {
	br := bufio.NewReader(bytes.NewReader(file))
	first, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(first) != magic {
		return nil, nil, errors.New("not a Janus backup")
	}
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, nil, errors.New("a Janus backup without its manifest")
	}
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, nil, fmt.Errorf("the backup's manifest: %w", err)
	}
	payload, err1 := base64.StdEncoding.DecodeString(env.Payload)
	sig, err2 := base64.StdEncoding.DecodeString(env.Signature)
	if err1 != nil || err2 != nil || !ed25519.Verify(signer, payload, sig) {
		return nil, nil, ErrSignature
	}
	var m Manifest
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, nil, err
	}
	enc := file[len(first)+len(line):]
	sum := sha256.Sum256(enc)
	if int64(len(enc)) != m.Size || hex.EncodeToString(sum[:]) != m.SHA256 {
		return nil, nil, ErrSignature
	}
	return &m, enc, nil
}

// Decrypt opens a backup's encrypted part with identity: the kit's, or an
// admin's SSH or age key.
func Decrypt(enc []byte, identities ...age.Identity) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(enc), identities...)
	if err != nil {
		return nil, fmt.Errorf("this key doesn't open the backup: %w", err)
	}
	return io.ReadAll(r)
}
