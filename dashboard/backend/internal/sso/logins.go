package sso

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"
	"sync"
	"time"
)

// Intents: what a sign-in through a provider is for.
const (
	// IntentLogin signs in - the account the identity is bound to, or
	// one the provider's rules make.
	IntentLogin = "login"
	// IntentLink binds the identity to the account signed in already.
	IntentLink = "link"
)

const (
	// loginTTL is how long the provider has to send the browser back;
	// doneTTL how long the page then has to finish.
	loginTTL = 10 * time.Minute
	doneTTL  = 2 * time.Minute
	// maxLogins bounds the sign-ins under way - someone starting them
	// without end gets ErrBusy, not the Controller's memory.
	maxLogins = 10000
)

// ErrBusy refuses a sign-in while too many are under way.
var ErrBusy = errors.New("too many sign-ins under way: try again in a few minutes")

// Login is a sign-in sent to a provider, waiting for it to send the
// browser back.
type Login struct {
	Provider string
	Version  int
	State    string
	Nonce    string
	Verifier string
	// RedirectURI is the one sent to the provider, sent again with the
	// code.
	RedirectURI string
	// Return is where the page goes once signed in: a route of its own
	// (ValidReturn).
	Return string
	Intent string
	// Session is the SHA-256 of the session that asked to link (hex).
	Session string
	Created time.Time
}

// Done is a sign-in the provider sent the browser back from: who it is,
// or why it failed - waiting for the page to finish it.
type Done struct {
	Login   Login
	Result  *Result
	Err     *Error
	Created time.Time
}

// Logins are the sign-ins under way, each under a random key (the
// browser's cookie): in memory only, each taken once.
type Logins struct {
	now func() time.Time

	mu      sync.Mutex
	pending map[string]*Login
	done    map[string]*Done
}

// NewLogins is an empty set.
func NewLogins() *Logins {
	return &Logins{now: time.Now, pending: map[string]*Login{}, done: map[string]*Done{}}
}

// Random is n random bytes, base64url: a state, a nonce, a key.
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// sweepLocked forgets the sign-ins nobody finished in time. Called with
// l.mu held.
func (l *Logins) sweepLocked(now time.Time) {
	for k, p := range l.pending {
		if now.Sub(p.Created) >= loginTTL {
			delete(l.pending, k)
		}
	}
	for k, d := range l.done {
		if now.Sub(d.Created) >= doneTTL {
			delete(l.done, k)
		}
	}
}

// Start keeps login, answering its key.
func (l *Logins) Start(login Login) (string, error) {
	key := Random(32)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	if len(l.pending)+len(l.done) >= maxLogins {
		return "", ErrBusy
	}
	login.Created = now
	l.pending[key] = &login
	return key, nil
}

// Take is key's sign-in under way - once: it's forgotten.
func (l *Logins) Take(key string) (Login, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.pending[key]
	delete(l.pending, key)
	if !ok || l.now().Sub(p.Created) >= loginTTL {
		return Login{}, false
	}
	return *p, true
}

// Finish keeps what a sign-in came to, under its key, for the page.
func (l *Logins) Finish(key string, d Done) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d.Created = l.now()
	l.done[key] = &d
}

// TakeDone is what key's sign-in came to - once.
func (l *Logins) TakeDone(key string) (Done, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.done[key]
	delete(l.done, key)
	if !ok || l.now().Sub(d.Created) >= doneTTL {
		return Done{}, false
	}
	return *d, true
}

// returnPattern is a route of the page: a fragment, never a URL - the
// page goes there with history.replaceState, on its own origin.
var returnPattern = regexp.MustCompile(`^#/[A-Za-z0-9._~!$&'()*+,;=:@/?%-]{0,1000}$`)

// ValidReturn reports whether s is a route the page may go back to.
func ValidReturn(s string) bool { return returnPattern.MatchString(s) }
