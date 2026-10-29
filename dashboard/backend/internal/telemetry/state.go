package telemetry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is <data-dir>/telemetry.json: whether reports are sent, under
// which ID, and how the last one went.
type State struct {
	Enabled bool `json:"enabled"`
	// InstallID names this Controller's reports, nothing else (not
	// controller-id, which tags its machines): 128 random bits, made
	// again on request.
	InstallID string    `json:"install_id,omitempty"`
	EnabledAt time.Time `json:"enabled_at,omitzero"`
	LastSent  time.Time `json:"last_sent,omitzero"`
	LastTry   time.Time `json:"last_try,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	// Failures since the last report sent.
	Failures int `json:"failures,omitempty"`
}

// Store keeps the State.
type Store struct {
	path string
	// Locked: the environment turned reports off
	// (JANUS_CONTROLLER_TELEMETRY=off) - the page can't turn them on.
	Locked bool

	mu sync.Mutex
	st State
}

// ErrLocked refuses turning reports on when the environment turned them
// off.
var ErrLocked = errors.New("telemetry is turned off by the Controller's environment (JANUS_CONTROLLER_TELEMETRY=off)")

// Open reads dataDir's telemetry.json, if any.
func Open(dataDir string, locked bool) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "telemetry.json"), Locked: locked}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, err
	}
	return s, nil
}

// Get is the state; Enabled is false while Locked.
func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Enabled = st.Enabled && !s.Locked
	return st
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SetEnabled turns reports on or off; on, an ID is made if there's none.
func (s *Store) SetEnabled(on bool, now time.Time) error {
	if on && s.Locked {
		return ErrLocked
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.st
	if on && !next.Enabled {
		next.EnabledAt, next.Failures = now, 0
	}
	if on && next.InstallID == "" {
		id, err := newID()
		if err != nil {
			return err
		}
		next.InstallID = id
	}
	next.Enabled = on
	return s.save(next)
}

// Regenerate gives the Controller a new ID, and returns the old one (to
// be forgotten by the receiver).
func (s *Store) Regenerate() (old string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := newID()
	if err != nil {
		return "", err
	}
	next := s.st
	old, next.InstallID = next.InstallID, id
	next.LastSent, next.Failures = time.Time{}, 0
	return old, s.save(next)
}

// Recorded notes a report's outcome.
func (s *Store) Recorded(at time.Time, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.st
	next.LastTry = at
	if err != nil {
		next.LastError = err.Error()
		next.Failures++
	} else {
		next.LastSent, next.LastError, next.Failures = at, "", 0
	}
	return s.save(next)
}

// Next is when the next report is due - the zero time when none is.
// The first one goes within an hour of turning them on (a moment the ID
// picks: Controllers don't all report at once), then one a day; a
// report that failed is tried again an hour later, then the next day.
func (s *Store) Next() time.Time {
	st := s.Get()
	if !st.Enabled || st.InstallID == "" {
		return time.Time{}
	}
	var next time.Time
	if st.LastSent.IsZero() {
		sum := sha256.Sum256([]byte(st.InstallID))
		next = st.EnabledAt.Add(time.Duration(binary.BigEndian.Uint64(sum[:8])%3600) * time.Second)
	} else {
		next = st.LastSent.Add(24 * time.Hour)
	}
	if st.Failures > 0 {
		wait := 24 * time.Hour
		if st.Failures == 1 {
			wait = time.Hour
		}
		if retry := st.LastTry.Add(wait); retry.After(next) {
			next = retry
		}
	}
	return next
}

// Due reports whether a report is due at now.
func (s *Store) Due(now time.Time) bool {
	next := s.Next()
	return !next.IsZero() && !now.Before(next)
}

// save writes next - tmp, fsync, rename - then keeps it.
func (s *Store) save(next State) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.st = next
	return nil
}
