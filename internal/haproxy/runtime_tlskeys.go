package haproxy

import (
	"errors"
	"fmt"
	"strings"
)

// ErrTLSKeysNotLoaded is a TLS ticket key file the running configuration
// doesn't name on any bind line.
var ErrTLSKeysNotLoaded = errors.New("HAProxy's configuration doesn't use this TLS ticket key file")

// SetTLSKey hands HAProxy a key for the ticket key ring it loaded from
// file ("set ssl tls-key"): it encrypts the tickets from now on, the
// oldest key of the ring is forgotten. No reload; the file itself is
// the caller's to keep in step (internal/cluster).
func (m *Manager) SetTLSKey(file, key string) error {
	fileArg, err := cliToken("TLS ticket key file", file)
	if err != nil {
		return err
	}
	keyArg, err := cliToken("TLS ticket key", key)
	if err != nil {
		return err
	}
	out, err := m.statsCommand("set ssl tls-key " + fileArg + " " + keyArg)
	if err != nil {
		return err
	}
	answer := strings.TrimSpace(string(out))
	switch {
	case strings.Contains(answer, "TLS ticket key updated"):
		return nil
	case strings.Contains(answer, "unable to locate referenced filename"):
		return ErrTLSKeysNotLoaded
	}
	return fmt.Errorf("set ssl tls-key: %s", answer)
}
