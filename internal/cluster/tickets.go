package cluster

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The TLS ticket key ring: a file HAProxy reads on a bind line
// ("tls-ticket-keys /etc/haproxy/cluster/tls-ticket-keys") so that a
// session resumes on any process that holds the same keys - across a
// reload, and across a cluster's members once the ring is pulled from
// the member that rotated it. HAProxy keeps TicketKeys keys: the last
// one encrypts, the others still decrypt. Each key is 80 random bytes,
// base64, one per line - what "set ssl tls-key" takes at runtime.

// TicketKeys is how many keys the ring holds: HAProxy's TLS_TICKETS_NO.
const TicketKeys = 3

const ticketKeyBytes = 80

// TicketKeysPath is the ring's file.
func (s *Store) TicketKeysPath() string { return filepath.Join(s.Dir, TicketKeysFile) }

// EnsureTicketKeys makes the ring if there is none, so a configuration
// may name it from the first boot on.
func (s *Store) EnsureTicketKeys() (created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.TicketKeysPath()); err == nil {
		return false, nil
	}
	var keys []string
	for range TicketKeys {
		k, err := newTicketKey()
		if err != nil {
			return false, err
		}
		keys = append(keys, k)
	}
	return true, s.writeTicketKeysLocked(keys)
}

// RotateTicketKeys adds a key to the ring and drops the oldest: the key
// added, for HAProxy to take at runtime.
func (s *Store) RotateTicketKeys() (key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.ticketKeysLocked()
	if err != nil {
		return "", err
	}
	if key, err = newTicketKey(); err != nil {
		return "", err
	}
	keys = append(keys, key)
	if len(keys) > TicketKeys {
		keys = keys[len(keys)-TicketKeys:]
	}
	return key, s.writeTicketKeysLocked(keys)
}

// RenewTicketKeys replaces every key of the ring: the keys, for HAProxy
// to take at runtime, oldest first. No key of the ring before is used
// or accepted once HAProxy has them all - what a node leaving a cluster,
// or a cluster a member left, does.
func (s *Store) RenewTicketKeys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for range TicketKeys {
		k, err := newTicketKey()
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, s.writeTicketKeysLocked(keys)
}

// TicketKeysRing is the ring's keys, oldest first.
func (s *Store) TicketKeysRing() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ticketKeysLocked()
}

// ReplaceTicketKeys installs a ring pulled from another member, checked
// first.
func (s *Store) ReplaceTicketKeys(data []byte) ([]string, error) {
	keys, err := parseTicketKeys(data)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return keys, s.writeTicketKeysLocked(keys)
}

// TicketKeysState describes the ring: how many keys, when it last
// changed, and a fingerprint to compare members by.
type TicketKeysState struct {
	Keys    int
	Changed time.Time
	SHA256  string
}

// TicketKeysState reads the ring's state; zero when there is none.
func (s *Store) TicketKeysState() TicketKeysState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.TicketKeysPath())
	if err != nil {
		return TicketKeysState{}
	}
	st, err := os.Stat(s.TicketKeysPath())
	if err != nil {
		return TicketKeysState{}
	}
	keys, _ := parseTicketKeys(data)
	sum := sha256.Sum256(data)
	return TicketKeysState{Keys: len(keys), Changed: st.ModTime(), SHA256: hex.EncodeToString(sum[:])}
}

func (s *Store) ticketKeysLocked() ([]string, error) {
	data, err := os.ReadFile(s.TicketKeysPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("no TLS ticket key ring yet")
	}
	if err != nil {
		return nil, err
	}
	return parseTicketKeys(data)
}

func (s *Store) writeTicketKeysLocked(keys []string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	var b bytes.Buffer
	for _, k := range keys {
		b.WriteString(k + "\n")
	}
	return writeDurable(s.Dir, TicketKeysFile, b.Bytes(), 0o600)
}

func newTicketKey() (string, error) {
	raw := make([]byte, ticketKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// parseTicketKeys checks a ring: TicketKeys lines, each 80 bytes base64.
func parseTicketKeys(data []byte) ([]string, error) {
	var keys []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(string(line))
		if err != nil || len(raw) != ticketKeyBytes {
			return nil, errors.New("the TLS ticket key ring holds a key that isn't 80 bytes, base64")
		}
		keys = append(keys, string(line))
	}
	if len(keys) != TicketKeys {
		return nil, fmt.Errorf("the TLS ticket key ring holds %d keys, not %d", len(keys), TicketKeys)
	}
	return keys, nil
}
