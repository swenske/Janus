package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH keys: what janusctl signs in with (janusctl login -ssh-key). A key
// belongs to one account; the certificate janusctl gets is for the key
// itself, with the key's role - the account's, or a lower one -, until
// the key's expiry if it has one.

// SSHKey is one of an account's keys.
type SSHKey struct {
	// Fingerprint is the key's SHA256:... - how it's named.
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	// PublicKey is the key, as an authorized_keys line (no comment).
	PublicKey string `json:"public_key"`
	// Role is the most the key may do; empty: the account's role.
	Role       Role       `json:"role,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// Expired reports whether the key can no longer sign in.
func (k SSHKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

// ParseSSHKey reads an authorized_keys line - a key janusctl can sign TLS
// handshakes with: Ed25519, ECDSA (P-256, P-384) or RSA of 2048 bits at
// least. A FIDO key (sk-*) can't: it signs SSH's data only.
func ParseSSHKey(line string) (ssh.PublicKey, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return nil, errors.New("not an SSH public key (the line of a .pub file)")
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384:
		return pub, nil
	case ssh.KeyAlgoRSA:
		ck, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return nil, errors.New("an unreadable RSA key")
		}
		if k, ok := ck.CryptoPublicKey().(*rsa.PublicKey); !ok || k.N.BitLen() < 2048 {
			return nil, errors.New("an RSA key must be at least 2048 bits")
		}
		return pub, nil
	case ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
		return nil, errors.New("a FIDO key (sk-*) signs SSH's data only, not janusctl's connections: use an Ed25519 key - in ssh-agent - or a key file")
	}
	return nil, fmt.Errorf("a %s key isn't supported: Ed25519, ECDSA or RSA", pub.Type())
}

var errSSHKeyTaken = errors.New("this key is already an account's")

// AddSSHKey gives user a key named name, with role at most (empty: the
// account's), valid ttl (0: until removed).
func (s *Store) AddSSHKey(user, name, line string, role Role, ttl time.Duration) (SSHKey, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return SSHKey{}, errors.New("name the key (1 to 60 characters): where it is")
	}
	if role != "" {
		if _, err := ParseRole(string(role)); err != nil {
			return SSHKey{}, err
		}
	}
	pub, err := ParseSSHKey(line)
	if err != nil {
		return SSHKey{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	if !ok {
		return SSHKey{}, ErrNoUser
	}
	if role != "" && !u.Role.AtLeast(role) {
		return SSHKey{}, fmt.Errorf("a key can't do more than its account: %s is %s", u.Name, u.Role)
	}
	fp := ssh.FingerprintSHA256(pub)
	for _, other := range s.users {
		if slices.ContainsFunc(other.SSHKeys, func(k SSHKey) bool { return k.Fingerprint == fp }) {
			return SSHKey{}, errSSHKeyTaken
		}
	}
	now := s.now().UTC()
	k := SSHKey{Fingerprint: fp, Name: name, PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), Role: role, CreatedAt: now}
	if ttl > 0 {
		exp := now.Add(ttl)
		k.ExpiresAt = &exp
	}
	old := u.SSHKeys
	u.SSHKeys = append(slices.Clone(u.SSHKeys), k)
	if err := s.save(); err != nil {
		u.SSHKeys = old
		return SSHKey{}, err
	}
	return k, nil
}

// RemoveSSHKey takes a key away from user.
func (s *Store) RemoveSSHKey(user, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	if !ok {
		return ErrNoUser
	}
	i := slices.IndexFunc(u.SSHKeys, func(k SSHKey) bool { return k.Fingerprint == fingerprint })
	if i < 0 {
		return errors.New("no such key")
	}
	old := u.SSHKeys
	u.SSHKeys = slices.Delete(slices.Clone(u.SSHKeys), i, i+1)
	if err := s.save(); err != nil {
		u.SSHKeys = old
		return err
	}
	return nil
}

// RemoveSSHKeys takes every key away from user - an admin's help for a
// lost laptop.
func (s *Store) RemoveSSHKeys(user string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	if !ok {
		return 0, ErrNoUser
	}
	n, old := len(u.SSHKeys), u.SSHKeys
	u.SSHKeys = nil
	if err := s.save(); err != nil {
		u.SSHKeys = old
		return 0, err
	}
	return n, nil
}

// SSHKeyFor is user's key fingerprint if it may sign in now - the
// account enabled, the key not expired -, with the role it signs in with.
func (s *Store) SSHKeyFor(user, fingerprint string) (ssh.PublicKey, Role, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[user]
	if !ok || u.Disabled {
		return nil, "", ErrInvalid
	}
	i := slices.IndexFunc(u.SSHKeys, func(k SSHKey) bool { return k.Fingerprint == fingerprint })
	if i < 0 || u.SSHKeys[i].Expired(s.now()) {
		return nil, "", ErrInvalid
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(u.SSHKeys[i].PublicKey))
	if err != nil {
		return nil, "", ErrInvalid
	}
	role := u.Role
	if r := u.SSHKeys[i].Role; r != "" {
		role = Lower(r, u.Role)
	}
	return pub, role, nil
}

// SSHKeyUsed records a sign-in with a key.
func (s *Store) SSHKeyUsed(user, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[user]
	if !ok {
		return
	}
	for i := range u.SSHKeys {
		if u.SSHKeys[i].Fingerprint == fingerprint {
			now := s.now().UTC()
			u.SSHKeys[i].LastUsedAt = &now
			_ = s.save() // informational: never fails a sign-in
		}
	}
}
