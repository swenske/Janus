// Package acme manages the letsencrypt extension (docs/letsencrypt.md):
// the node obtains its HAProxy certificates from an ACME CA and renews
// them by itself.
//
// janusd keeps everything - the configuration, the account key, the
// certificates - and runs janus-acme (cmd/janus-acme, the extension's
// ACME client) for each exchange with the CA, handing it what that one
// needs on its standard input (internal/acme/acmewire). Each certificate
// is a file HAProxy loads, /etc/haproxy/acme/<name>.pem: a self-signed
// stand-in until the CA's certificate arrives, so a configuration can
// reference it from the start; a renewed one is swapped into the running
// HAProxy without a reload.
//
// HTTP-01 challenges are answered by HAProxy, statelessly (HTTP01Rule):
// janusd hands it the account's thumbprint in its environment.
package acme

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme/acmewire"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
)

// Where things are - vars, so tests can move them.
var (
	// Binary is the extension's ACME client.
	Binary = "/usr/local/sbin/janus-acme"
	// Dir is STATE's config/: the configuration, the account key, the
	// state.
	Dir = "/etc/janus/config"
	// CertDir is where the certificates are, for HAProxy: STATE's
	// haproxy/acme/.
	CertDir = "/etc/haproxy/acme"
)

const (
	configFile = "letsencrypt.json"
	keyFile    = "acme/account.key"
	stateFile  = "acme/state.json"

	// ThumbprintEnv is the variable HAProxy finds the account's
	// thumbprint in.
	ThumbprintEnv = "JANUS_ACME_THUMBPRINT"
	// HTTP01Rule answers HTTP-01 challenges: the key authorization of a
	// token is "<token>.<thumbprint>" (RFC 8555 8.3).
	HTTP01Rule = `http-request return status 200 content-type text/plain lf-string "%[path,field(-1,/)].${` + ThumbprintEnv + `}" if { path_beg /.well-known/acme-challenge/ }`

	// LogID is the extension's log, among the services'.
	LogID = "letsencrypt"
)

// Timings.
var (
	idleWake      = time.Hour
	haproxyWait   = 30 * time.Second
	issueTimeout  = 15 * time.Minute
	queryTimeout  = 2 * time.Minute
	ariMinRecheck = time.Hour
	ariMaxRecheck = 24 * time.Hour
)

// backoff is how long to wait after the nth failure in a row: 5 min,
// doubling, up to a day - four attempts in the first hour at most: Let's
// Encrypt allows five failed validations per hostname and hour.
func backoff(n int) time.Duration {
	d := 5 * time.Minute
	for i := 1; i < n && d < 24*time.Hour; i++ {
		d *= 2
	}
	return min(d, 24*time.Hour)
}

// HAProxy is what the manager needs of janusd's HAProxy manager.
type HAProxy interface {
	ReplaceCertFile(path string, pemBundle []byte) error
	Reload() error
	StartedAt() time.Time
	Validate(cfg []byte) (bool, []string)
}

// Options are the manager's.
type Options struct {
	HAProxy HAProxy
	// HAProxyConfig is haproxy.cfg's path: a certificate the
	// configuration still uses can't be removed.
	HAProxyConfig string
	// Healthy reports whether HAProxy answers - it answers the HTTP-01
	// challenges.
	Healthy func() bool
	// Output gets janus-acme's log and the manager's.
	Output io.Writer
}

// Manager manages the extension.
type Manager struct {
	opts   Options
	logger *log.Logger
	kick   chan struct{}

	mu         sync.Mutex
	cfg        *janusv1alpha1.ACMEConfig // saved; nil: none
	key        crypto.Signer
	thumb      string
	st         *state
	inProgress string
	forced     map[string]bool
	waiting    bool // logged that HTTP-01 waits for HAProxy
}

type state struct {
	// Accounts are by directory URL and thumbprint.
	Accounts map[string]*accountState `json:"accounts,omitempty"`
	Certs    map[string]*certState    `json:"certificates,omitempty"`
}

type accountState struct {
	URI      string    `json:"uri,omitempty"`
	TermsURL string    `json:"terms_url,omitempty"`
	Error    string    `json:"error,omitempty"`
	Checked  time.Time `json:"checked,omitzero"`
}

type certState struct {
	// What the file's certificate was obtained with.
	Directory  string `json:"directory,omitempty"`
	Thumbprint string `json:"thumbprint,omitempty"`
	Profile    string `json:"profile,omitempty"`
	// Created is when the file was first written: a file created after
	// HAProxy started isn't loaded in it.
	Created     time.Time `json:"created,omitzero"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// ACME Renewal Information for the certificate of serial ARISerial:
	// the time chosen in the CA's window, and when to ask again.
	ARISerial  string    `json:"ari_serial,omitempty"`
	ARIRenewAt time.Time `json:"ari_renew_at,omitzero"`
	ARICheck   time.Time `json:"ari_check,omitzero"`
}

func New(opts Options) *Manager {
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}
	return &Manager{
		opts:   opts,
		logger: log.New(out, "letsencrypt: ", log.LstdFlags),
		kick:   make(chan struct{}, 1),
		forced: map[string]bool{},
		st:     &state{},
	}
}

// Available reports whether the extension is in the image.
func (m *Manager) Available() bool {
	_, err := os.Stat(Binary)
	return err == nil
}

// Thumbprint is the account key's thumbprint ("" before Boot, or without
// the extension).
func (m *Manager) Thumbprint() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.thumb
}

// Env is HAProxy's part of the extension's environment.
func (m *Manager) Env() []string {
	if t := m.Thumbprint(); t != "" {
		return []string{ThumbprintEnv + "=" + t}
	}
	return nil
}

// Boot loads everything, creating the account key the first time, and
// puts a stand-in where a certificate isn't there yet - before HAProxy
// starts, so its configuration can reference them all.
func (m *Manager) Boot() error {
	if !m.Available() {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(Dir, "acme"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(CertDir, 0o700); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadKey(); err != nil {
		return err
	}
	if data, err := os.ReadFile(filepath.Join(Dir, stateFile)); err == nil {
		st := &state{}
		if err := json.Unmarshal(data, st); err != nil {
			m.logger.Printf("%s: %v - starting over", stateFile, err)
		} else {
			m.st = st
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg != nil {
		normalize(cfg)
	}
	m.cfg = cfg
	return m.writePlaceholders()
}

// loadKey loads the account key, or creates it. Called with mu held.
func (m *Manager) loadKey() error {
	path := filepath.Join(Dir, keyFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if data, err = newAccountKey(); err != nil {
			return err
		}
		if err := writeDurable(filepath.Dir(path), filepath.Base(path), data, 0o600); err != nil {
			return err
		}
		m.logger.Printf("created the account key")
	} else if err != nil {
		return err
	}
	key, err := parseAccountKey(data)
	if err != nil {
		return err
	}
	t, err := thumbprint(key)
	if err != nil {
		return err
	}
	m.key, m.thumb = key, t
	return nil
}

// writePlaceholders puts a stand-in where a configured certificate isn't
// there yet, and rewrites a stand-in whose names or key type changed.
// Called with mu held.
func (m *Manager) writePlaceholders() error {
	if m.cfg == nil {
		return nil
	}
	for _, c := range m.cfg.Certificates {
		path := certPath(c.Name)
		if data, err := os.ReadFile(path); err == nil {
			b, err := parseBundle(data)
			if err == nil && (!b.Placeholder || (sameNames(b.Leaf, c.Domains) && keyTypeOf(b.Leaf) == c.KeyType)) {
				continue
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		data, err := placeholder(c.Domains, c.KeyType)
		if err != nil {
			return err
		}
		if err := m.writeCert(c.Name, data); err != nil {
			return err
		}
	}
	return nil
}

// writeCert writes a certificate file durably, noting when it was first
// created. Called with mu held.
func (m *Manager) writeCert(name string, data []byte) error {
	path := certPath(name)
	_, err := os.Stat(path)
	existed := err == nil
	// The temporary file is outside CertDir: HAProxy loads whatever is in
	// a crt directory.
	if err := writeDurableVia(filepath.Dir(CertDir), CertDir, name+".pem", data, 0o600); err != nil {
		return err
	}
	cs := m.certState(name)
	if !existed || cs.Created.IsZero() {
		cs.Created = time.Now()
	}
	return m.saveState()
}

func certPath(name string) string { return filepath.Join(CertDir, name+".pem") }

// certState is name's state, created if needed. Called with mu held.
func (m *Manager) certState(name string) *certState {
	if m.st.Certs == nil {
		m.st.Certs = map[string]*certState{}
	}
	cs := m.st.Certs[name]
	if cs == nil {
		cs = &certState{}
		m.st.Certs[name] = cs
	}
	return cs
}

func (m *Manager) accountKey(dir, thumb string) string { return dir + " " + thumb }

// saveState writes the state durably. Called with mu held.
func (m *Manager) saveState() error {
	data, err := json.MarshalIndent(m.st, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(filepath.Join(Dir, "acme"), filepath.Base(stateFile), append(data, '\n'), 0o600)
}

// Config is the saved configuration without its secrets; nil if none.
func (m *Manager) Config() *janusv1alpha1.ACMEConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return nil
	}
	return Redacted(m.cfg)
}

// Apply checks cfg (and accountKey, PEM, if given), and unless
// validateOnly makes them the node's: saved, stand-ins written for the
// new certificates, the removed ones' files deleted - refused while
// haproxy.cfg still uses one. Certificates to obtain are then obtained in
// the background.
func (m *Manager) Apply(cfg *janusv1alpha1.ACMEConfig, accountKey string, validateOnly bool) ([]string, error) {
	if !m.Available() {
		return nil, errors.New("the letsencrypt extension isn't in this image")
	}
	cfg = proto.Clone(cfg).(*janusv1alpha1.ACMEConfig)
	normalize(cfg)
	m.mu.Lock()
	defer m.mu.Unlock()
	mergeSecrets(cfg, m.cfg)
	errs := Validate(cfg)
	var key crypto.Signer
	var thumb string
	if accountKey != "" {
		var err error
		if key, err = parseAccountKey([]byte(accountKey)); err == nil {
			thumb, err = thumbprint(key)
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errs, errors.New("invalid configuration")
	}

	var removed []string
	if m.cfg != nil {
		for _, c := range m.cfg.Certificates {
			if !slices.ContainsFunc(cfg.Certificates, func(n *janusv1alpha1.ACMECertificate) bool { return n.Name == c.Name }) {
				removed = append(removed, c.Name)
			}
		}
	}
	if errs := m.checkRemovable(removed); len(errs) > 0 {
		return errs, errors.New("certificates still in use")
	}
	if validateOnly {
		return nil, nil
	}

	reload := false
	started := m.opts.HAProxy.StartedAt()
	if key != nil && thumb != m.thumb {
		path := filepath.Join(Dir, keyFile)
		if err := writeDurable(filepath.Dir(path), filepath.Base(path), []byte(accountKey), 0o600); err != nil {
			return nil, err
		}
		m.key, m.thumb = key, thumb
		m.logger.Printf("account key replaced: thumbprint %s", thumb)
		reload = true // HAProxy's environment has the thumbprint
	}
	data, err := Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := writeDurable(Dir, configFile, data, 0o600); err != nil {
		return nil, err
	}
	m.cfg = cfg
	for _, name := range removed {
		if cs := m.st.Certs[name]; cs != nil && !started.IsZero() && cs.Created.Before(started) {
			reload = true // a crt directory may have loaded it
		}
		if err := os.Remove(certPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		delete(m.st.Certs, name)
		delete(m.forced, name)
		m.logger.Printf("certificate %s removed", name)
	}
	if err := m.writePlaceholders(); err != nil {
		return nil, err
	}
	if err := m.saveState(); err != nil {
		return nil, err
	}
	if reload && !started.IsZero() {
		go func() {
			if err := m.opts.HAProxy.Reload(); err != nil {
				m.logger.Printf("reload HAProxy: %v", err)
			}
		}()
	}
	m.wake()
	return nil, nil
}

// checkRemovable makes sure haproxy.cfg doesn't need the files of the
// removed certificates: it's checked without them. Called with mu held.
func (m *Manager) checkRemovable(names []string) []string {
	if len(names) == 0 || m.opts.HAProxyConfig == "" {
		return nil
	}
	cfg, err := os.ReadFile(m.opts.HAProxyConfig)
	if err != nil {
		return nil // no configuration: nothing uses them
	}
	aside := filepath.Join(filepath.Dir(CertDir), ".acme-removing")
	if err := os.MkdirAll(aside, 0o700); err != nil {
		return []string{err.Error()}
	}
	defer os.Remove(aside)
	var moved []string
	defer func() {
		for _, name := range moved {
			if err := os.Rename(filepath.Join(aside, name+".pem"), certPath(name)); err != nil {
				m.logger.Printf("put %s back: %v", certPath(name), err)
			}
		}
	}()
	for _, name := range names {
		if err := os.Rename(certPath(name), filepath.Join(aside, name+".pem")); err == nil {
			moved = append(moved, name)
		}
	}
	if ok, errs := m.opts.HAProxy.Validate(cfg); !ok {
		return append([]string{"haproxy.cfg doesn't load without the certificates removed here - take them out of it first:"}, errs...)
	}
	return nil
}

// Renew obtains names (every certificate if none) at the next pass,
// whether they're due or not.
func (m *Manager) Renew(names []string) ([]string, error) {
	if !m.Available() {
		return nil, errors.New("the letsencrypt extension isn't in this image")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil || len(m.cfg.Certificates) == 0 {
		return nil, errors.New("no certificate is configured")
	}
	var out []string
	for _, c := range m.cfg.Certificates {
		if len(names) == 0 || slices.Contains(names, c.Name) {
			out = append(out, c.Name)
		}
	}
	for _, n := range names {
		if !slices.Contains(out, n) {
			return nil, fmt.Errorf("no certificate is named %q", n)
		}
	}
	for _, n := range out {
		m.forced[n] = true
	}
	m.wake()
	return out, nil
}

// HAProxyConfigChanged retries the HTTP-01 certificates not obtained
// yet at once: the new HAProxy configuration may be what answers their
// challenges now.
func (m *Manager) HAProxyConfigChanged() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return
	}
	retry := false
	for _, c := range m.cfg.Certificates {
		cs := m.st.Certs[c.Name]
		if c.Challenge != acmewire.ChallengeHTTP01 || cs == nil || cs.Failures == 0 || !cs.LastSuccess.IsZero() {
			continue
		}
		cs.NextAttempt, retry = time.Time{}, true
	}
	if retry {
		m.logger.Printf("HAProxy's configuration changed - retrying the HTTP-01 certificates not obtained yet")
		m.wake()
	}
}

func (m *Manager) wake() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Run obtains and renews the certificates until stop closes.
func (m *Manager) Run(stop <-chan struct{}) {
	for {
		next := m.pass()
		t := time.NewTimer(time.Until(next))
		select {
		case <-m.kick:
		case <-t.C:
		case <-stop:
			t.Stop()
			return
		}
		t.Stop()
	}
}

// due is why a certificate must be obtained now ("" if it needn't), and
// when it will be due otherwise.
func (m *Manager) due(c *janusv1alpha1.ACMECertificate, cs *certState, dir string, now time.Time) (string, time.Time) {
	data, err := os.ReadFile(certPath(c.Name))
	if err != nil {
		return "no certificate yet", time.Time{}
	}
	b, err := parseBundle(data)
	switch {
	case err != nil:
		return "unreadable certificate file", time.Time{}
	case b.Placeholder:
		return "not obtained yet", time.Time{}
	case !sameNames(b.Leaf, c.Domains):
		return "the domains changed", time.Time{}
	case keyTypeOf(b.Leaf) != c.KeyType:
		return "the key type changed", time.Time{}
	case cs.Directory != dir:
		return "the CA changed", time.Time{}
	case cs.Profile != c.Profile:
		return "the profile changed", time.Time{}
	}
	at := renewAt(b.Leaf)
	if cs.ARISerial == serialHex(b.Leaf) && !cs.ARIRenewAt.IsZero() && cs.ARIRenewAt.Before(at) {
		at = cs.ARIRenewAt
	}
	if !now.Before(at) {
		if now.After(b.Leaf.NotAfter) {
			return "expired", at
		}
		return "renewal due", at
	}
	return "", at
}

// pass does what's due and returns when to look again.
func (m *Manager) pass() time.Time {
	now := time.Now()
	next := now.Add(idleWake)
	m.mu.Lock()
	if m.cfg == nil || len(m.cfg.Certificates) == 0 || m.key == nil {
		m.mu.Unlock()
		return next
	}
	cfg := proto.Clone(m.cfg).(*janusv1alpha1.ACMEConfig)
	dir := ResolveDirectory(cfg.Account.Directory)
	thumb := m.thumb
	acct := m.st.Accounts[m.accountKey(dir, thumb)]
	m.mu.Unlock()

	if acct == nil || (acct.URI == "" && now.Sub(acct.Checked) >= backoff(1)) {
		m.checkAccount(cfg, dir, thumb)
	}

	for _, c := range cfg.Certificates {
		m.mu.Lock()
		cs := m.certState(c.Name)
		forced := m.forced[c.Name]
		reason, at := m.due(c, cs, dir, now)
		nextAttempt := cs.NextAttempt
		m.mu.Unlock()
		if forced {
			reason = "renewal asked for"
		}
		if reason == "" {
			if !at.IsZero() && at.Before(next) {
				next = at
			}
			if t := m.checkRenewalInfo(cfg, c, dir, thumb, now); !t.IsZero() && t.Before(next) {
				next = t
			}
			continue
		}
		if !forced && now.Before(nextAttempt) {
			if nextAttempt.Before(next) {
				next = nextAttempt
			}
			continue
		}
		if c.Challenge == acmewire.ChallengeHTTP01 && m.opts.Healthy != nil && !m.opts.Healthy() {
			m.mu.Lock()
			if !m.waiting {
				m.logger.Printf("HTTP-01 certificates wait for HAProxy to answer")
				m.waiting = true
			}
			m.mu.Unlock()
			if t := now.Add(haproxyWait); t.Before(next) {
				next = t
			}
			continue
		}
		m.mu.Lock()
		m.waiting = false
		m.mu.Unlock()
		m.obtain(cfg, c, dir, thumb, reason)
		if t := time.Now().Add(time.Second); forced && t.Before(next) {
			next = t // another forced one may be queued meanwhile
		}
	}
	return next
}

// checkAccount finds or registers the account, for the status.
func (m *Manager) checkAccount(cfg *janusv1alpha1.ACMEConfig, dir, thumb string) {
	res, err := m.run(m.request(acmewire.ActionAccount, cfg, dir), queryTimeout)
	m.mu.Lock()
	defer m.mu.Unlock()
	a := &accountState{Checked: time.Now()}
	if res != nil {
		a.URI, a.TermsURL = res.AccountURI, res.TermsURL
	}
	if err != nil {
		a.Error = err.Error()
		m.logger.Printf("account: %v", err)
	} else {
		m.logger.Printf("account %s", a.URI)
	}
	if m.st.Accounts == nil {
		m.st.Accounts = map[string]*accountState{}
	}
	m.st.Accounts[m.accountKey(dir, thumb)] = a
	if err := m.saveState(); err != nil {
		m.logger.Printf("save the state: %v", err)
	}
}

// checkRenewalInfo asks the CA when the certificate should be renewed,
// once a day or as the CA says; it returns when to ask again.
func (m *Manager) checkRenewalInfo(cfg *janusv1alpha1.ACMEConfig, c *janusv1alpha1.ACMECertificate, dir, thumb string, now time.Time) time.Time {
	m.mu.Lock()
	cs := m.certState(c.Name)
	if cs.Directory != dir || now.Before(cs.ARICheck) {
		t := cs.ARICheck
		m.mu.Unlock()
		return t
	}
	m.mu.Unlock()
	data, err := os.ReadFile(certPath(c.Name))
	if err != nil {
		return time.Time{}
	}
	b, err := parseBundle(data)
	if err != nil || b.Placeholder {
		return time.Time{}
	}
	req := m.request(acmewire.ActionRenewalInfo, cfg, dir)
	req.Certificate = string(data)
	res, err := m.run(req, queryTimeout)

	m.mu.Lock()
	defer m.mu.Unlock()
	cs = m.certState(c.Name)
	recheck := now.Add(ariMaxRecheck)
	if err != nil {
		m.logger.Printf("certificate %s: renewal information: %v", c.Name, err)
	} else {
		if !res.RetryAfter.IsZero() {
			recheck = res.RetryAfter
			if lo := now.Add(ariMinRecheck); recheck.Before(lo) {
				recheck = lo
			}
			if hi := now.Add(ariMaxRecheck); recheck.After(hi) {
				recheck = hi
			}
		}
		serial := serialHex(b.Leaf)
		start, end := res.RenewalStart, res.RenewalEnd
		// A time chosen at random in the window (RFC 9773 4.2), kept
		// while the window stays the same.
		if !end.After(start) {
			end = start
		}
		if cs.ARISerial != serial || cs.ARIRenewAt.Before(start) || cs.ARIRenewAt.After(end) {
			at := start
			if span := end.Sub(start); span > 0 {
				at = start.Add(time.Duration(rand.Int64N(int64(span))))
			}
			cs.ARISerial, cs.ARIRenewAt = serial, at
		}
	}
	cs.ARICheck = recheck
	if err := m.saveState(); err != nil {
		m.logger.Printf("save the state: %v", err)
	}
	return recheck
}

// obtain gets a certificate from the CA, writes it and puts it into
// HAProxy.
func (m *Manager) obtain(cfg *janusv1alpha1.ACMEConfig, c *janusv1alpha1.ACMECertificate, dir, thumb, reason string) {
	req := m.request(acmewire.ActionIssue, cfg, dir)
	req.Domains, req.KeyType, req.Challenge, req.Profile = c.Domains, c.KeyType, c.Challenge, c.Profile
	if c.Challenge == acmewire.ChallengeDNS01 {
		for _, p := range cfg.DnsProviders {
			if p.Name == c.DnsProvider {
				req.DNSProvider, req.DNSEnv, req.DNSResolvers = p.Type, p.Settings, p.Resolvers
				req.DNSPropagationWait = time.Duration(p.PropagationWaitSeconds) * time.Second
			}
		}
	}
	m.mu.Lock()
	cs := m.certState(c.Name)
	if current, err := os.ReadFile(certPath(c.Name)); err == nil && cs.Directory == dir && cs.Thumbprint == thumb {
		if b, err := parseBundle(current); err == nil && !b.Placeholder {
			req.Certificate = string(current) // the order replaces it (ARI)
		}
	}
	m.inProgress = c.Name
	delete(m.forced, c.Name)
	m.mu.Unlock()

	m.logger.Printf("certificate %s: %s - ordering it (%s, %s)", c.Name, reason, c.Challenge, joinDomains(c.Domains))
	res, err := m.run(req, issueTimeout)
	var data []byte
	if err == nil {
		data, _, err = checkIssued(res.Certificate, res.PrivateKey, c.Domains)
	}

	m.mu.Lock()
	m.inProgress = ""
	cs = m.certState(c.Name)
	cs.LastAttempt = time.Now()
	if res != nil && res.AccountURI != "" {
		if m.st.Accounts == nil {
			m.st.Accounts = map[string]*accountState{}
		}
		m.st.Accounts[m.accountKey(dir, thumb)] = &accountState{URI: res.AccountURI, TermsURL: res.TermsURL, Checked: time.Now()}
	}
	if err == nil {
		err = m.writeCert(c.Name, data)
	}
	if err != nil {
		cs.Failures++
		cs.LastError = err.Error()
		cs.NextAttempt = time.Now().Add(backoff(cs.Failures))
		m.logger.Printf("certificate %s: %v - next attempt %s", c.Name, err, cs.NextAttempt.Format(time.RFC3339))
		if serr := m.saveState(); serr != nil {
			m.logger.Printf("save the state: %v", serr)
		}
		m.mu.Unlock()
		events.Publish("acme.failed", map[string]any{"name": c.Name, "error": err.Error(), "failures": cs.Failures})
		return
	}
	b, _ := parseBundle(data)
	cs.Directory, cs.Thumbprint, cs.Profile = dir, thumb, c.Profile
	cs.LastSuccess, cs.LastError, cs.Failures, cs.NextAttempt = time.Now(), "", 0, time.Time{}
	cs.ARISerial, cs.ARIRenewAt, cs.ARICheck = "", time.Time{}, time.Time{}
	created := cs.Created
	if err := m.saveState(); err != nil {
		m.logger.Printf("save the state: %v", err)
	}
	m.mu.Unlock()
	m.logger.Printf("certificate %s obtained: serial %s, valid until %s", c.Name, serialHex(b.Leaf), b.Leaf.NotAfter.Format(time.RFC3339))
	events.Publish("acme.issued", map[string]any{"name": c.Name, "serial": serialHex(b.Leaf), "not_after": b.Leaf.NotAfter.Unix()})
	m.deploy(c.Name, data, created)
}

// deploy puts a new certificate file's content into the running HAProxy:
// swapped in place if HAProxy loaded the file, by a reload if it couldn't
// have (the file is newer than the process - a crt directory picks it up
// then).
func (m *Manager) deploy(name string, data []byte, created time.Time) {
	started := m.opts.HAProxy.StartedAt()
	if started.IsZero() {
		return // it'll load the file when it starts
	}
	err := m.opts.HAProxy.ReplaceCertFile(certPath(name), data)
	switch {
	case err == nil:
		m.logger.Printf("certificate %s swapped into HAProxy", name)
		return
	case errors.Is(err, haproxy.ErrCertNotLoaded) && created.Before(started):
		return // HAProxy's configuration doesn't use it
	case errors.Is(err, haproxy.ErrCertNotLoaded):
		m.logger.Printf("certificate %s: new file - reloading HAProxy", name)
	default:
		m.logger.Printf("certificate %s: swap into HAProxy: %v - reloading it", name, err)
	}
	if err := m.opts.HAProxy.Reload(); err != nil {
		m.logger.Printf("reload HAProxy: %v", err)
	}
}

// request is a request with the account's part filled in.
func (m *Manager) request(action string, cfg *janusv1alpha1.ACMEConfig, dir string) *acmewire.Request {
	m.mu.Lock()
	keyPEM, _ := os.ReadFile(filepath.Join(Dir, keyFile))
	m.mu.Unlock()
	a := cfg.Account
	return &acmewire.Request{
		Action: action, Directory: dir, DirectoryCA: a.DirectoryCa,
		AccountKey: string(keyPEM), Email: a.Email, AcceptTerms: a.AcceptTerms,
		EABKeyID: a.EabKeyId, EABHMACKey: a.EabHmacKey,
	}
}

// run has janus-acme do req.
func (m *Manager) run(req *acmewire.Request, timeout time.Duration) (*acmewire.Result, error) {
	in, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, Binary)
	cmd.Env = []string{} // nothing of janusd's: the request has it all
	cmd.Stdin = bytes.NewReader(in)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = m.opts.Output
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	runErr := cmd.Run()
	res := &acmewire.Result{}
	if err := json.Unmarshal(out.Bytes(), res); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("janus-acme took over %s", timeout)
		}
		if runErr != nil {
			return nil, fmt.Errorf("janus-acme: %w", runErr)
		}
		return nil, fmt.Errorf("janus-acme's answer: %w", err)
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

func joinDomains(d []string) string {
	if len(d) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(d[:3], ", "), len(d)-3)
	}
	return strings.Join(d, ", ")
}
