// Package auth holds the Janus Controller's accounts: users with a role -
// reader, operator or admin -, their sessions, the session policy, and
// API tokens (tokens.go). The first account is made on first run (forced
// setup); an admin makes the others. Passwords are bcrypt hashes in
// <data-dir>/users.json (0600), with the session policy.
//
// The main port is HTTPS-only (see dashboard/backend/main.go's own doc
// comment) - the session cookie is marked Secure accordingly, so a
// browser refuses to ever send it in the clear.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	usersFile = "users.json"
	// legacyFile is the single admin password of Controllers before
	// accounts: read once, as the account "admin". Left in place - a
	// Controller rolled back to such a version still has its password
	// rather than offering its setup screen to whoever comes first.
	legacyFile = "auth.json"

	// LegacyAdmin is the account the single admin password became.
	LegacyAdmin = "admin"

	minPasswordLen = 8
	// bcrypt only reads the first 72 bytes: a longer password would
	// silently be checked on its beginning only.
	maxPasswordLen = 72
)

// Role is what an account may do on the Controller, and on nodes through
// it.
type Role string

const (
	// Reader sees everything the Controller shows, changes nothing.
	Reader Role = "reader"
	// Operator runs nodes - HAProxy, services, reboots - and powers
	// machines, without changing how they're set up.
	Operator Role = "operator"
	// Admin does everything: accounts, the fleet, hypervisors, machines,
	// approvals, updates.
	Admin Role = "admin"
)

var roleRank = map[Role]int{Reader: 1, Operator: 2, Admin: 3}

// ParseRole checks a role's name.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if roleRank[r] == 0 {
		return "", fmt.Errorf("unknown role %q (reader, operator or admin)", s)
	}
	return r, nil
}

// AtLeast reports whether r grants everything min does.
func (r Role) AtLeast(min Role) bool { return roleRank[r] >= roleRank[min] && roleRank[r] > 0 }

// Lower is the lesser of two roles.
func Lower(a, b Role) Role {
	if roleRank[a] <= roleRank[b] {
		return a
	}
	return b
}

// NamePattern is what an account's name may be: it's also who a node
// logs as having acted (nodeproxy's impersonation), and an e-mail
// address fits for a single sign-on later.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{0,63}$`)

// User is an account. PasswordHash never leaves the package's callers.
type User struct {
	Name         string `json:"name"`
	Role         Role   `json:"role"`
	PasswordHash []byte `json:"password_hash"`
	// MustChangePassword: someone else set the password (an admin, a
	// reset from the host) - the account's sessions can only change it.
	MustChangePassword bool `json:"must_change_password,omitempty"`
	Disabled           bool `json:"disabled,omitempty"`
	// Epoch moves with every password change, reset and disablement: a
	// session started before it is over.
	Epoch       int        `json:"epoch"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// Settings is the session policy.
type Settings struct {
	// SessionIdleMinutes ends a session nobody used for that long.
	SessionIdleMinutes int `json:"session_idle_minutes"`
	// SessionMaxHours ends a session that long after its sign-in,
	// used or not.
	SessionMaxHours int `json:"session_max_hours"`
}

// DefaultSettings: half an hour away ends a session, and none outlives a
// working day.
var DefaultSettings = Settings{SessionIdleMinutes: 30, SessionMaxHours: 12}

// Check bounds the policy: a session from five minutes to a day idle, an
// hour to thirty days long.
func (s Settings) Check() error {
	if s.SessionIdleMinutes < 5 || s.SessionIdleMinutes > 24*60 {
		return errors.New("session_idle_minutes: 5 to 1440")
	}
	if s.SessionMaxHours < 1 || s.SessionMaxHours > 30*24 {
		return errors.New("session_max_hours: 1 to 720")
	}
	if s.SessionIdleMinutes > s.SessionMaxHours*60 {
		return errors.New("a session can't stay idle longer than it lasts")
	}
	return nil
}

// Idle and Max are the policy as durations.
func (s Settings) Idle() time.Duration { return time.Duration(s.SessionIdleMinutes) * time.Minute }
func (s Settings) Max() time.Duration  { return time.Duration(s.SessionMaxHours) * time.Hour }

// Session is one sign-in.
type Session struct {
	User     string
	Epoch    int
	Created  time.Time
	LastSeen time.Time
}

// Expires is when the session ends if nothing uses it again.
func (s Session) Expires(p Settings) time.Time {
	idle, max := s.LastSeen.Add(p.Idle()), s.Created.Add(p.Max())
	if idle.Before(max) {
		return idle
	}
	return max
}

var (
	// ErrInvalid is a wrong name or password - never which of the two.
	ErrInvalid = errors.New("invalid credentials")
	// ErrLastAdmin refuses what would leave no enabled admin.
	ErrLastAdmin = errors.New("this is the last enabled admin: make another admin first")
	ErrNoUser    = errors.New("no such account")
)

// Store holds the accounts, the policy and the sessions. Sessions are
// deliberately not persisted - a restart requires signing in again.
type Store struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	users    map[string]*User
	settings Settings
	sessions map[string]*Session
	// stamp is users.json as last read or written: a change made from
	// outside (dashboardd reset-user, on the host) is read again.
	stamp fileStamp
}

type fileStamp struct {
	mod  time.Time
	size int64
}

type usersFileContents struct {
	Settings Settings `json:"settings"`
	Users    []*User  `json:"users"`
}

// Open loads the accounts from dataDir - the single admin password of an
// older Controller becoming the account "admin" -, or leaves the store
// empty (setup required).
func Open(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, usersFile), now: time.Now, sessions: map[string]*Session{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	if len(s.users) == 0 {
		if err := s.migrate(filepath.Join(dataDir, legacyFile)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) load() error {
	s.users, s.settings = map[string]*User{}, DefaultSettings
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.stamp = fileStamp{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	var c usersFileContents
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	if c.Settings.Check() == nil {
		s.settings = c.Settings
	}
	for _, u := range c.Users {
		s.users[u.Name] = u
	}
	s.stamp = stampOf(s.path)
	return nil
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{fi.ModTime(), fi.Size()}
}

// fresh reads users.json again if something else wrote it. Called with
// s.mu held.
func (s *Store) fresh() {
	if stampOf(s.path) == s.stamp {
		return
	}
	sessions := s.sessions
	if err := s.load(); err != nil {
		// Half-written or broken from outside: keep what we had, the
		// next save rewrites it whole.
		return
	}
	s.sessions = sessions
}

func (s *Store) migrate(legacy string) error {
	data, err := os.ReadFile(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", legacy, err)
	}
	var c struct {
		PasswordHash []byte `json:"password_hash"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("parse %s: %w", legacy, err)
	}
	if c.PasswordHash == nil {
		return nil
	}
	s.users[LegacyAdmin] = &User{Name: LegacyAdmin, Role: Admin, PasswordHash: c.PasswordHash, CreatedAt: s.now().UTC()}
	return s.save()
}

// save writes users.json whole: tmp, fsync, rename. Called with s.mu held.
func (s *Store) save() error {
	c := usersFileContents{Settings: s.settings}
	for _, u := range s.users {
		c.Users = append(c.Users, u)
	}
	sort.Slice(c.Users, func(i, j int) bool { return c.Users[i].Name < c.Users[j].Name })
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileDurably(s.path, append(data, '\n')); err != nil {
		return err
	}
	s.stamp = stampOf(s.path)
	return nil
}

func writeFileDurably(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func checkPassword(password string) error {
	if len(password) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	if len(password) > maxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordLen)
	}
	return nil
}

func hash(password string) ([]byte, error) {
	if err := checkPassword(password); err != nil {
		return nil, err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	return h, nil
}

func checkName(name string) error {
	if !NamePattern.MatchString(name) {
		return errors.New("a name is 1 to 64 lowercase letters, digits and . _ @ -, starting with a letter or a digit")
	}
	return nil
}

// view is a copy of u without its hash.
func view(u *User) User {
	c := *u
	c.PasswordHash = nil
	return c
}

// SetupRequired reports whether no account exists yet.
func (s *Store) SetupRequired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	return len(s.users) == 0
}

// Setup makes the first account, an admin - refused once any exists.
func (s *Store) Setup(name, password string) (User, error) {
	if err := checkName(name); err != nil {
		return User{}, err
	}
	h, err := hash(password)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	if len(s.users) > 0 {
		return User{}, errors.New("the Controller is already set up")
	}
	u := &User{Name: name, Role: Admin, PasswordHash: h, CreatedAt: s.now().UTC()}
	s.users[name] = u
	if err := s.save(); err != nil {
		delete(s.users, name)
		return User{}, err
	}
	return view(u), nil
}

// dummyHash is compared against when the name is unknown, so a wrong name
// takes as long to refuse as a wrong password.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("janus-no-such-account"), bcrypt.DefaultCost)

// Authenticate checks name's password: ErrInvalid for an unknown name, a
// wrong password or a disabled account alike.
func (s *Store) Authenticate(name, password string) (User, error) {
	s.mu.Lock()
	s.fresh()
	u, ok := s.users[name]
	var h []byte
	if ok {
		h = u.PasswordHash
	}
	s.mu.Unlock()
	if !ok {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return User{}, ErrInvalid
	}
	if bcrypt.CompareHashAndPassword(h, []byte(password)) != nil {
		return User{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok = s.users[name]
	if !ok || u.Disabled {
		return User{}, ErrInvalid
	}
	now := s.now().UTC()
	u.LastLoginAt = &now
	_ = s.save() // informational: never fails a sign-in
	return view(u), nil
}

// NewSession signs user in.
func (s *Store) NewSession(user string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[user]
	if !ok {
		return "", ErrNoUser
	}
	now := s.now()
	// Sessions nobody presents again would stay until a restart: swept
	// here, which bounds the map by the sign-ins of one session's life.
	for t, ss := range s.sessions {
		if !s.liveLocked(ss, now) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = &Session{User: user, Epoch: u.Epoch, Created: now, LastSeen: now}
	return token, nil
}

func (s *Store) liveLocked(ss *Session, now time.Time) bool {
	u, ok := s.users[ss.User]
	return ok && !u.Disabled && u.Epoch == ss.Epoch && now.Before(ss.Expires(s.settings))
}

// Session returns the account and the session token is, if it's live -
// touch: a request the user made, which keeps it alive, rather than one
// the page makes by itself (a periodic refresh).
func (s *Store) Session(token string, touch bool) (User, Session, bool) {
	if token == "" {
		return User{}, Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	ss, ok := s.sessions[token]
	if !ok {
		return User{}, Session{}, false
	}
	now := s.now()
	if !s.liveLocked(ss, now) {
		delete(s.sessions, token)
		return User{}, Session{}, false
	}
	if touch {
		ss.LastSeen = now
	}
	return view(s.users[ss.User]), *ss, true
}

// Revoke ends one session (sign out).
func (s *Store) Revoke(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// Settings is the session policy now.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	return s.settings
}

// SetSettings changes the session policy - for the live sessions too.
func (s *Store) SetSettings(p Settings) error {
	if err := p.Check(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	old := s.settings
	s.settings = p
	if err := s.save(); err != nil {
		s.settings = old
		return err
	}
	return nil
}

// Users returns every account, by name.
func (s *Store) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, view(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// User returns one account.
func (s *Store) User(name string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[name]
	if !ok {
		return User{}, false
	}
	return view(u), true
}

// CreateUser makes an account whose password its owner changes at the
// first sign-in.
func (s *Store) CreateUser(name string, role Role, password string) (User, error) {
	if err := checkName(name); err != nil {
		return User{}, err
	}
	if _, err := ParseRole(string(role)); err != nil {
		return User{}, err
	}
	h, err := hash(password)
	if err != nil {
		return User{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	if _, ok := s.users[name]; ok {
		return User{}, fmt.Errorf("an account %q already exists", name)
	}
	u := &User{Name: name, Role: role, PasswordHash: h, MustChangePassword: true, CreatedAt: s.now().UTC()}
	s.users[name] = u
	if err := s.save(); err != nil {
		delete(s.users, name)
		return User{}, err
	}
	return view(u), nil
}

// Change is what UpdateUser changes - nil fields are left alone.
type Change struct {
	Role     *Role
	Disabled *bool
	// Password is set by an admin: its owner changes it at the next
	// sign-in, and the account's sessions are over.
	Password *string
}

// UpdateUser changes an account - never leaving no enabled admin.
func (s *Store) UpdateUser(name string, c Change) (User, error) {
	var h []byte
	if c.Password != nil {
		var err error
		if h, err = hash(*c.Password); err != nil {
			return User{}, err
		}
	}
	if c.Role != nil {
		if _, err := ParseRole(string(*c.Role)); err != nil {
			return User{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[name]
	if !ok {
		return User{}, ErrNoUser
	}
	next := *u
	if c.Role != nil {
		next.Role = *c.Role
	}
	if c.Disabled != nil && *c.Disabled != next.Disabled {
		next.Disabled = *c.Disabled
		next.Epoch++
	}
	if h != nil {
		next.PasswordHash, next.MustChangePassword = h, true
		next.Epoch++
	}
	if u.Role == Admin && !u.Disabled && (next.Role != Admin || next.Disabled) && s.enabledAdminsLocked() == 1 {
		return User{}, ErrLastAdmin
	}
	s.users[name] = &next
	if err := s.save(); err != nil {
		s.users[name] = u
		return User{}, err
	}
	return view(&next), nil
}

func (s *Store) enabledAdminsLocked() int {
	n := 0
	for _, u := range s.users {
		if u.Role == Admin && !u.Disabled {
			n++
		}
	}
	return n
}

// DeleteUser removes an account - its sessions end with it.
func (s *Store) DeleteUser(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	u, ok := s.users[name]
	if !ok {
		return ErrNoUser
	}
	if u.Role == Admin && !u.Disabled && s.enabledAdminsLocked() == 1 {
		return ErrLastAdmin
	}
	delete(s.users, name)
	if err := s.save(); err != nil {
		s.users[name] = u
		return err
	}
	return nil
}

// ChangePassword is an account changing its own password: every session
// of the account ends, the caller starts a new one.
func (s *Store) ChangePassword(name, current, next string) error {
	if _, err := s.Authenticate(name, current); err != nil {
		return errors.New("the current password is wrong")
	}
	if current == next {
		return errors.New("the new password must differ from the current one")
	}
	h, err := hash(next)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		return ErrNoUser
	}
	old := *u
	u.PasswordHash, u.MustChangePassword = h, false
	u.Epoch++
	if err := s.save(); err != nil {
		*u = old
		return err
	}
	return nil
}

// ResetFromHost gives name a new random password to change at its next
// sign-in, and enables it - making it an admin if it doesn't exist: the
// way back in when no admin can sign in (dashboardd reset-user, run on
// the Controller's host). Its sessions end.
func ResetFromHost(dataDir, name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	s, err := Open(dataDir)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 15)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	h, err := hash(password)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[name]
	if !ok {
		u = &User{Name: name, Role: Admin, CreatedAt: s.now().UTC()}
		s.users[name] = u
	}
	u.PasswordHash, u.MustChangePassword, u.Disabled = h, true, false
	u.Epoch++
	return password, s.save()
}
