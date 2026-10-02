package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// API tokens let a program - a Terraform provider, a script - call the
// Controller's API with "Authorization: Bearer <token>" instead of the
// admin's session. A token is shown once, when it's created: only its
// SHA-256 is kept (<data-dir>/api-tokens.json, 0600). Its form,
// "janus_<id>_<secret>", names the record to check without a search.

// TokenPrefix starts every token, so one found in a file or a log is
// recognizable.
const TokenPrefix = "janus_"

// Token is a token's record - never its secret.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hash       string     `json:"hash"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// Expired reports whether the token can no longer be used.
func (t *Token) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

// TokenStore keeps the tokens.
type TokenStore struct {
	path string

	mu       sync.Mutex
	tokens   map[string]*Token
	lastSave time.Time
}

// usageSaveEvery bounds how often a token's last use is written down: a
// Terraform run makes dozens of calls a second.
const usageSaveEvery = time.Minute

func OpenTokens(dataDir string) (*TokenStore, error) {
	s := &TokenStore{path: filepath.Join(dataDir, "api-tokens.json"), tokens: map[string]*Token{}}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Token
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	for _, t := range list {
		s.tokens[t.ID] = t
	}
	return s, nil
}

// Create makes a token named name, valid for ttl (0: until revoked), and
// returns it - the only time its secret exists outside the caller.
func (s *TokenStore) Create(name string, ttl time.Duration) (string, *Token, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", nil, errors.New("a name of 1 to 100 characters is required")
	}
	if ttl < 0 {
		return "", nil, errors.New("the validity can't be negative")
	}
	id, err := randomHex(6)
	if err != nil {
		return "", nil, err
	}
	secret, err := randomHex(32)
	if err != nil {
		return "", nil, err
	}
	token := TokenPrefix + id + "_" + secret
	now := time.Now().UTC()
	t := &Token{ID: id, Name: name, Hash: hashToken(token), CreatedAt: now}
	if ttl > 0 {
		exp := now.Add(ttl)
		t.ExpiresAt = &exp
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[id] = t
	if err := s.saveLocked(); err != nil {
		delete(s.tokens, id)
		return "", nil, err
	}
	c := *t
	return token, &c, nil
}

// Verify returns the token's record if token is valid: known, matching,
// not expired.
func (s *TokenStore) Verify(token string) (*Token, bool) {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok {
		return nil, false
	}
	id, _, ok := strings.Cut(rest, "_")
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok || subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(t.Hash)) != 1 {
		return nil, false
	}
	now := time.Now().UTC()
	if t.Expired(now) {
		return nil, false
	}
	t.LastUsedAt = &now
	if now.Sub(s.lastSave) >= usageSaveEvery {
		_ = s.saveLocked() // the last use is informational: never fails a call
	}
	c := *t
	return &c, true
}

// List returns every token's record, newest first.
func (s *TokenStore) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Revoke deletes a token: it stops working at once.
func (s *TokenStore) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return fmt.Errorf("no token %q", id)
	}
	delete(s.tokens, id)
	if err := s.saveLocked(); err != nil {
		s.tokens[id] = t
		return err
	}
	return nil
}

func (s *TokenStore) saveLocked() error {
	list := make([]*Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.lastSave = time.Now()
	return nil
}

func hashToken(token string) string {
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
