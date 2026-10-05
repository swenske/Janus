// Package enroll is the Controller's enrollment tokens: a token admits up
// to a number of nodes before a date - bare metal, a batch -, without
// the approval step, and gives each its labels (so its accounts'
// permissions). A node presents it as its registration token (NoCloud,
// Install, janusctl image seed-controller). Only its SHA-256 is kept
// (<data-dir>/enroll-tokens.json, 0600); its form,
// "janus-enroll_<id>_<secret>", names the record to check.
package enroll

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/labels"
)

// Prefix starts every enrollment token.
const Prefix = "janus-enroll_"

const (
	file = "enroll-tokens.json"
	// MaxUses and MaxDays bound a token: a batch, not a standing key.
	MaxUses = 1000
	MaxDays = 365
)

// Token is a token's record - never its secret.
type Token struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Hash      string            `json:"hash"`
	MaxUses   int               `json:"max_uses"`
	Uses      int               `json:"uses"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedBy string            `json:"created_by"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt time.Time         `json:"expires_at"`
	// Nodes are those it admitted (node IDs), the newest last.
	Nodes []string `json:"nodes,omitempty"`
}

// Usable reports whether the token still admits a node at now.
func (t *Token) Usable(now time.Time) bool {
	return t.Uses < t.MaxUses && now.Before(t.ExpiresAt)
}

// Store is the Controller's enrollment tokens.
type Store struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	tokens map[string]*Token
}

// Open loads the tokens kept in dataDir.
func Open(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, file), now: time.Now, tokens: map[string]*Token{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Token
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for _, t := range list {
		s.tokens[t.ID] = t
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	list := make([]*Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		list = append(list, t)
	}
	slices.SortFunc(list, func(a, b *Token) int { return a.CreatedAt.Compare(b.CreatedAt) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(s.path+".tmp", s.path)
}

func clone(t *Token) Token {
	c := *t
	c.Labels = maps.Clone(t.Labels)
	c.Nodes = slices.Clone(t.Nodes)
	return c
}

// Create makes a token named name admitting maxUses nodes for days days,
// labelling them l, and returns it - the only time its secret exists.
func (s *Store) Create(name, createdBy string, maxUses, days int, l map[string]string) (string, Token, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", Token{}, errors.New("a name of 1 to 100 characters is required")
	}
	if maxUses < 1 || maxUses > MaxUses {
		return "", Token{}, fmt.Errorf("max_uses: 1 to %d", MaxUses)
	}
	if days < 1 || days > MaxDays {
		return "", Token{}, fmt.Errorf("expires_in_days: 1 to %d", MaxDays)
	}
	if err := labels.Check(l); err != nil {
		return "", Token{}, err
	}
	id, err := randomHex(6)
	if err != nil {
		return "", Token{}, err
	}
	secret, err := randomHex(32)
	if err != nil {
		return "", Token{}, err
	}
	token := Prefix + id + "_" + secret
	now := s.now().UTC()
	t := &Token{ID: id, Name: name, Hash: hash(token), MaxUses: maxUses, Labels: maps.Clone(l), CreatedBy: createdBy, CreatedAt: now, ExpiresAt: now.Add(time.Duration(days) * 24 * time.Hour)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[id] = t
	if err := s.saveLocked(); err != nil {
		delete(s.tokens, id)
		return "", Token{}, err
	}
	return token, clone(t), nil
}

// Is reports whether s looks like an enrollment token (and not a
// machine's registration token).
func Is(token string) bool { return strings.HasPrefix(token, Prefix) }

// Claim spends one use of token for a node about to be admitted - when
// it's known, unexpired, and has a use left - and answers its record
// (its labels). Record the node with Admitted once it is.
func (s *Store) Claim(token string) (Token, bool) {
	rest, ok := strings.CutPrefix(token, Prefix)
	if !ok {
		return Token{}, false
	}
	id, _, ok := strings.Cut(rest, "_")
	if !ok {
		return Token{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok || subtle.ConstantTimeCompare([]byte(hash(token)), []byte(t.Hash)) != 1 || !t.Usable(s.now()) {
		return Token{}, false
	}
	t.Uses++
	if err := s.saveLocked(); err != nil {
		t.Uses--
		return Token{}, false
	}
	return clone(t), true
}

// Admitted records the node a claimed use of token id admitted.
func (s *Store) Admitted(id, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tokens[id]; ok {
		t.Nodes = append(t.Nodes, nodeID)
		_ = s.saveLocked() // the use is spent either way
	}
}

// List is every token, the newest first.
func (s *Store) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, clone(t))
	}
	slices.SortFunc(out, func(a, b Token) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// Revoke forgets a token: it admits nobody any more.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return errors.New("no such enrollment token")
	}
	delete(s.tokens, id)
	if err := s.saveLocked(); err != nil {
		s.tokens[id] = t
		return err
	}
	return nil
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
