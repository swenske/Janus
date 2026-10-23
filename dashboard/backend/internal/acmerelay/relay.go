// Package acmerelay is the Controller's relay for the nodes that reach
// Let's Encrypt through it (docs/guide/letsencrypt-controller.md): an
// HTTPS proxy, CONNECT only, that knows a node by a certificate of the
// node's own CA - the one the Controller pins - and tunnels to the CA's
// and the DNS providers' API hosts only. It sees host names and byte
// counts: the TLS inside each tunnel is the node's, end to end.
package acmerelay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/ratelimit"
	"github.com/swenske/Janus/internal/acme"
)

// DefaultListen is where the relay listens unless told otherwise.
const DefaultListen = ":8444"

const (
	maxPerNode  = 8   // tunnels open at once, per node
	maxOpen     = 256 // tunnels open at once, every node
	maxAllowed  = 100 // destinations an admin adds
	idleTimeout = 2 * time.Minute
	maxDuration = 15 * time.Minute // janus-acme's own bound on an order
	dialTimeout = 10 * time.Second
	keepRecent  = 100
)

// Config is what an admin sets.
type Config struct {
	Enabled bool `json:"enabled"`
	// Listen is the address it listens on, DefaultListen when empty.
	Listen string `json:"listen,omitempty"`
	// Address is host:port the nodes reach it at - a name or address in
	// the Controller's identity; empty: the Controller's guess.
	Address string `json:"address,omitempty"`
	// Allowed are host:port destinations beyond the built-in ones: a
	// private ACME CA, the API an httpreq or pdns provider names.
	Allowed []string `json:"allowed,omitempty"`
}

// Normalized is c with its defaults, trimmed, its destinations lower
// case, sorted and once each.
func (c Config) Normalized() Config {
	c.Listen = strings.TrimSpace(c.Listen)
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	c.Address = strings.TrimSpace(c.Address)
	var allowed []string
	for _, a := range c.Allowed {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" && !slices.Contains(allowed, a) {
			allowed = append(allowed, a)
		}
	}
	slices.Sort(allowed)
	c.Allowed = allowed
	return c
}

// Validate says what's wrong with c (normalized).
func (c Config) Validate() []string {
	var errs []string
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || (!validPort(port) && port != "0") { // 0: any, for tests
		errs = append(errs, fmt.Sprintf("listen %q: [address]:port", c.Listen))
	}
	if c.Address != "" {
		if host, port, err := net.SplitHostPort(c.Address); err != nil || host == "" || !validPort(port) {
			errs = append(errs, fmt.Sprintf("address %q: host:port, what the nodes reach the relay at", c.Address))
		}
	}
	if len(c.Allowed) > maxAllowed {
		errs = append(errs, fmt.Sprintf("allowed: %d destinations at most", maxAllowed))
	}
	for _, a := range c.Allowed {
		host, port, err := net.SplitHostPort(a)
		if err != nil || host == "" || !validPort(port) || strings.ContainsAny(host, "/*@ ") {
			errs = append(errs, fmt.Sprintf("allowed %q: host:port (no scheme, no wildcard)", a))
		}
	}
	return errs
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 65536
}

// Node is a node the relay knows a certificate's issuer as.
type Node struct{ ID, Name string }

// Tunnel is a tunnel the relay opened, or refused.
type Tunnel struct {
	Node    string    `json:"node"`
	NodeID  string    `json:"node_id"`
	Target  string    `json:"target"`
	Start   time.Time `json:"start"`
	Seconds float64   `json:"seconds"`
	Up      int64     `json:"bytes_up"`   // node to target
	Down    int64     `json:"bytes_down"` // target to node
	Refused string    `json:"refused,omitempty"`
	Error   string    `json:"error,omitempty"` // the target couldn't be reached
}

// Status is the relay now.
type Status struct {
	Config Config `json:"config"`
	// Listening is the address it listens on; Error why it doesn't, when
	// it should.
	Listening string `json:"listening,omitempty"`
	Error     string `json:"error,omitempty"`
	// BuiltIn are the destinations every node may reach on port 443, by
	// what they're for.
	BuiltIn map[string][]string `json:"built_in"`
	Open    int                 `json:"open"`
	Recent  []Tunnel            `json:"recent"` // newest first
}

// Server is the relay. Identity and NodeOf must be set; Logf and Dial
// have defaults.
type Server struct {
	// Identity is the certificate it serves: the Controller's own, the
	// one the nodes are given.
	Identity func() *tls.Certificate
	// NodeOf is the approved node whose CA issued chain[0].
	NodeOf func(chain []*x509.Certificate) (Node, bool)
	Logf   func(format string, args ...any)
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)

	mu      sync.Mutex
	cfg     Config
	ln      net.Listener
	srv     *http.Server
	err     string
	open    int
	perNode map[string]int
	recent  []Tunnel
	limiter *ratelimit.Limiter
	builtIn map[string]bool
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Boot starts the relay as saved; what goes wrong is in the status.
func (s *Server) Boot(cfg Config) {
	if err := s.Apply(cfg); err != nil {
		s.mu.Lock()
		s.cfg, s.err = cfg.Normalized(), err.Error()
		s.mu.Unlock()
		s.logf("acme relay: %v", err)
	}
}

// Apply makes cfg the relay's: listening on its address when enabled,
// nothing changed if that fails.
func (s *Server) Apply(cfg Config) error {
	cfg = cfg.Normalized()
	if errs := cfg.Validate(); len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limiter == nil {
		s.limiter = ratelimit.New(60, time.Second)
		s.perNode = map[string]int{}
		s.builtIn = map[string]bool{}
		for _, h := range acme.RelayHosts() {
			s.builtIn[net.JoinHostPort(h, "443")] = true
		}
	}
	running := s.ln != nil
	switch {
	case !cfg.Enabled:
		s.stopLocked()
		if running {
			s.logf("acme relay: off")
		}
	case running && cfg.Listen == s.cfg.Listen:
	default:
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
		}
		s.stopLocked()
		s.serveLocked(ln)
		s.logf("acme relay: listening on %s", ln.Addr())
	}
	s.cfg, s.err = cfg, ""
	return nil
}

func (s *Server) stopLocked() {
	if s.srv != nil {
		_ = s.srv.Close() // tunnels open go on until they end
	}
	if s.ln != nil {
		_ = s.ln.Close() // now, even if Serve hasn't started with it yet
	}
	s.ln, s.srv = nil, nil
}

func (s *Server) serveLocked(ln net.Listener) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
		ClientAuth: tls.RequireAnyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.Identity(), nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if _, ok := s.NodeOf(cs.PeerCertificates); !ok {
				return errors.New("not a certificate of an approved node's CA")
			}
			return nil
		},
	}
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(logWriter{s}, "", 0),
	}
	s.ln, s.srv = ln, srv
	go func() { _ = srv.Serve(tls.NewListener(ln, tlsConfig)) }()
}

// logWriter has the HTTP server's own errors (a handshake refused) in
// the relay's log.
type logWriter struct{ s *Server }

func (w logWriter) Write(p []byte) (int, error) {
	w.s.logf("acme relay: %s", strings.TrimSpace(string(p)))
	return len(p), nil
}

// Close stops listening.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

// Status is the relay now.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Config: s.cfg.Normalized(), Error: s.err, BuiltIn: acme.RelayDestinations(), Open: s.open}
	if s.ln != nil {
		st.Listening = s.ln.Addr().String()
	}
	st.Recent = make([]Tunnel, len(s.recent))
	for i, t := range s.recent {
		st.Recent[len(s.recent)-1-i] = t
	}
	return st
}

func (s *Server) allowed(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builtIn[target] || slices.Contains(s.cfg.Allowed, target)
}

// take counts a tunnel for node, if there's room.
func (s *Server) take(node string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open >= maxOpen || s.perNode[node] >= maxPerNode {
		return false
	}
	s.open++
	s.perNode[node]++
	return true
}

func (s *Server) release(node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open--
	if s.perNode[node]--; s.perNode[node] <= 0 {
		delete(s.perNode, node)
	}
}

func (s *Server) record(t Tunnel) {
	t.Seconds = time.Since(t.Start).Seconds()
	switch {
	case t.Refused != "":
		s.logf("acme relay: %s -> %s refused: %s", t.Node, t.Target, t.Refused)
	case t.Error != "":
		s.logf("acme relay: %s -> %s: %s", t.Node, t.Target, t.Error)
	default:
		s.logf("acme relay: %s -> %s: %d bytes up, %d down, %.1f s", t.Node, t.Target, t.Up, t.Down, t.Seconds)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append(s.recent, t)
	if len(s.recent) > keepRecent {
		s.recent = s.recent[len(s.recent)-keepRecent:]
	}
}

// ServeHTTP tunnels a node's CONNECT to an allowed destination.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var node Node
	ok := r.TLS != nil
	if ok {
		node, ok = s.NodeOf(r.TLS.PeerCertificates) // removed since its handshake?
	}
	if !ok {
		http.Error(w, "not an approved node", http.StatusForbidden)
		return
	}
	t := Tunnel{Node: node.Name, NodeID: node.ID, Target: strings.ToLower(r.Host), Start: time.Now()}
	if r.Method != http.MethodConnect {
		// A plain-HTTP request (a DNS provider's http:// API): journaled,
		// so the tab says why it never went through.
		if r.URL.Host != "" {
			t.Target = strings.ToLower(r.URL.Host)
		}
		t.Refused = "the relay only tunnels HTTPS (CONNECT) - an http:// API can't go through it"
		s.record(t)
		http.Error(w, t.Refused, http.StatusMethodNotAllowed)
		return
	}
	refuse := func(code int, why string) {
		t.Refused = why
		s.record(t)
		http.Error(w, why, code)
	}
	if !s.allowed(t.Target) {
		refuse(http.StatusForbidden, "not a destination the relay allows - an admin adds it on the Controller's Let's Encrypt tab")
		return
	}
	if ok, _ := s.limiter.Allow(node.ID); !ok {
		refuse(http.StatusTooManyRequests, "too many tunnels opened - wait a moment")
		return
	}
	if !s.take(node.ID) {
		refuse(http.StatusTooManyRequests, "too many tunnels open at once")
		return
	}
	defer s.release(node.ID)

	dial := s.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	up, err := dial(ctx, "tcp", t.Target)
	cancel()
	if err != nil {
		t.Error = err.Error()
		s.record(t)
		http.Error(w, "the relay couldn't reach "+t.Target+": "+err.Error(), http.StatusBadGateway)
		return
	}
	// Hijacked before answering, and read through its buffer: the server
	// may read the client's first bytes ahead once the answer is out.
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		up.Close()
		conn.Close()
		return
	}
	t.Up, t.Down = pipe(conn, buf, up)
	s.record(t)
}

// pipe copies both ways until either side ends, idles for idleTimeout,
// or maxDuration has passed; the bytes copied each way.
func pipe(client net.Conn, clientBuf io.Reader, target net.Conn) (up, down int64) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			client.Close()
			target.Close()
		})
	}
	timer := time.AfterFunc(maxDuration, closeBoth)
	defer timer.Stop()
	var upN, downN atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer closeBoth()
		copyIdle(target, clientBuf, client, &upN)
	}()
	go func() {
		defer wg.Done()
		defer closeBoth()
		copyIdle(client, target, target, &downN)
	}()
	wg.Wait()
	return upN.Load(), downN.Load()
}

// copyIdle copies src to dst until an error, or src (whose deadline is
// on conn) idles for idleTimeout.
func copyIdle(dst io.Writer, src io.Reader, conn net.Conn, n *atomic.Int64) {
	b := make([]byte, 32*1024)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		k, err := src.Read(b)
		if k > 0 {
			if _, werr := dst.Write(b[:k]); werr != nil {
				return
			}
			n.Add(int64(k))
		}
		if err != nil {
			return
		}
	}
}

// Load reads the configuration saved at path; the zero one (off) if
// there's none.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Save writes c at path durably: tmp, fsync, rename, the directory's
// fsync.
func Save(path string, c Config) error {
	data, err := json.MarshalIndent(c.Normalized(), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
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
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
