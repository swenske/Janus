// Package exporter serves the node's own Prometheus metrics: what only
// Janus knows - certificate expiry, boot slot and pending upgrades,
// HAProxy as janusd manages it, extension services, time sync, SELinux -
// and nothing node_exporter or HAProxy's own exporter already provide.
// Plain HTTP on its own port (DefaultPort), on by default, on the node's
// main interface (Auto); enabling it, the port and the address are set
// through the API and persisted in Dir.
//
// The text format is written here rather than through
// prometheus/client_golang: a handful of gauges and counters computed at
// scrape time doesn't need the library, and janusd stays small.
package exporter

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultPort follows the last allocation in Prometheus's list of
// default exporter ports when this was written (github.com/prometheus/
// prometheus/wiki/Default-port-allocations - the 9100-9999 range is
// full, exporters now take 10000 and up; 10055 was the last one).
const DefaultPort = 10056

// Dir holds janusd's small persistent settings documents - bind-mounted
// from STATE's config/ by rootfs/init. A var so tests can point it
// elsewhere.
var Dir = "/etc/janus/config"

const configFile = "metrics.json"

// Auto, as the address, is the node's main interface: the addresses
// netmgr says (Server.Main) - the interface its IPv4 default route
// leaves by -, followed as the network changes (Refresh).
const Auto = "auto"

// Config is the exporter's persisted configuration.
type Config struct {
	Enabled bool   `json:"enabled"`
	Port    uint32 `json:"port"`
	// Address to listen on: Auto, empty for every address, or one IP. A
	// management address keeps the metrics off the networks HAProxy
	// serves.
	Address string `json:"address,omitempty"`
}

// DefaultConfig is what a node runs with until it's changed: on, on
// DefaultPort, on the main interface.
func DefaultConfig() Config { return Config{Enabled: true, Port: DefaultPort, Address: Auto} }

// Validate checks c. Port 0 means DefaultPort; the address, if any, is
// Auto or an IP address of the node's.
func (c *Config) Validate() error {
	c.Port = cmp.Or(c.Port, DefaultPort)
	if c.Port > 65535 {
		return fmt.Errorf("port %d is out of range", c.Port)
	}
	if c.Address != "" && c.Address != Auto && net.ParseIP(c.Address) == nil {
		return fmt.Errorf("%q isn't an IP address (or %q)", c.Address, Auto)
	}
	return nil
}

// Load reads the persisted configuration; isDefault reports that none
// was saved.
func Load() (cfg Config, isDefault bool, err error) {
	data, err := os.ReadFile(filepath.Join(Dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), true, nil
	}
	if err != nil {
		return DefaultConfig(), true, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultConfig(), true, fmt.Errorf("%s: %w", configFile, err)
	}
	if err := cfg.Validate(); err != nil {
		return DefaultConfig(), true, fmt.Errorf("%s: %w", configFile, err)
	}
	return cfg, false, nil
}

// Save persists cfg, atomically.
func Save(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(Dir, "."+configFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
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
	if err := os.Rename(tmp, filepath.Join(Dir, configFile)); err != nil {
		return err
	}
	d, err := os.Open(Dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Metric types.
const (
	Gauge   = "gauge"
	Counter = "counter"
)

// Family is one metric family: its samples share name, help and type.
type Family struct {
	Name, Help, Type string
	Samples          []Sample
}

// Sample is one value, with its labels in the order they're written.
type Sample struct {
	Labels []Label
	Value  float64
}

type Label struct{ Name, Value string }

// L builds a label list from name, value pairs.
func L(pairs ...string) []Label {
	out := make([]Label, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Label{pairs[i], pairs[i+1]})
	}
	return out
}

// Bool is 1 or 0.
func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collector returns families at scrape time. A collector with nothing to
// report returns none - a metric that doesn't apply is absent, not 0.
type Collector func() []Family

// Write renders families in the Prometheus text format (version 0.0.4).
// Families without samples are left out.
func Write(w io.Writer, families []Family) error {
	var b strings.Builder
	for _, f := range families {
		if len(f.Samples) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n", f.Name, escapeHelp(f.Help))
		fmt.Fprintf(&b, "# TYPE %s %s\n", f.Name, f.Type)
		for _, s := range f.Samples {
			b.WriteString(f.Name)
			if len(s.Labels) > 0 {
				b.WriteByte('{')
				for i, l := range s.Labels {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, "%s=\"%s\"", l.Name, escapeLabel(l.Value))
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(formatValue(s.Value))
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Built once: a Replacer prepares its lookup table on first use, and
// Write escapes every label value of every scrape.
var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func escapeHelp(s string) string { return helpEscaper.Replace(s) }

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Server serves the collectors' metrics per the current Config.
type Server struct {
	collectors []Collector
	// Main returns the addresses Auto listens on - netmgr's
	// MainAddresses; none (or no Main) is every address. Set before the
	// first Apply.
	Main func() []netip.Addr

	mu      sync.Mutex
	cfg     Config
	hosts   []string // what cfg resolved to: an address each, "" for every one
	srv     *http.Server
	lastErr string
}

// New returns a Server that isn't listening yet; Apply starts it.
func New(collectors ...Collector) *Server {
	return &Server{collectors: collectors}
}

// Apply switches to cfg: stops the current listeners and, if enabled,
// listens on the new addresses and port. The new ones are bound before
// the old listeners close, so if they can't be bound the previous ones
// keep running and the error is returned - except on the same port:
// the old listeners hold it, so they close first, and come back if the
// new addresses can't be bound.
func (s *Server) Apply(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(cfg)
}

// Refresh listens again on the main interface's addresses when they
// changed (netmgr's OnChange) - with Auto only, else nothing to do.
func (s *Server) Refresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.Enabled || s.cfg.Address != Auto {
		return nil
	}
	return s.applyLocked(s.cfg)
}

// resolve is the addresses cfg listens on: Auto's from Main.
func (s *Server) resolve(cfg Config) []string {
	if cfg.Address != Auto {
		return []string{cfg.Address}
	}
	var main []netip.Addr
	if s.Main != nil {
		main = s.Main()
	}
	if len(main) == 0 {
		return []string{""}
	}
	hosts := make([]string, len(main))
	for i, a := range main {
		hosts[i] = a.String()
	}
	return hosts
}

func (s *Server) applyLocked(cfg Config) error {
	hosts := s.resolve(cfg)
	if cfg == s.cfg && slices.Equal(hosts, s.hosts) && (s.srv != nil) == cfg.Enabled {
		return nil
	}
	if !cfg.Enabled {
		if s.srv != nil {
			_ = s.srv.Close()
			s.srv = nil
		}
		s.cfg, s.hosts, s.lastErr = cfg, nil, ""
		log.Printf("exporter: disabled")
		return nil
	}
	previous, previousHosts := s.cfg, s.hosts
	samePort := s.srv != nil && previous.Port == cfg.Port
	if samePort {
		_ = s.srv.Close()
		s.srv = nil
	}
	port := strconv.Itoa(int(cfg.Port))
	lis, err := listen(hosts, port, cfg.Address == Auto)
	if err != nil {
		s.lastErr = err.Error()
		if samePort {
			if old, e := listen(previousHosts, port, previous.Address == Auto); e == nil {
				s.serve(old)
			} else {
				log.Printf("exporter: the previous listener couldn't come back either: %v", e)
			}
		}
		return err
	}
	if s.srv != nil {
		_ = s.srv.Close()
	}
	s.serve(lis)
	s.cfg, s.hosts, s.lastErr = cfg, hosts, ""
	log.Printf("exporter: metrics served on :%s/metrics (%s)", port, where(hosts))
	return nil
}

// where says the addresses it listens on, for the log - after the port:
// the line "exporter: metrics served on :PORT/metrics" is what the log
// tests (and anyone grepping) look for, whatever the address.
func where(hosts []string) string {
	if len(hosts) == 1 && hosts[0] == "" {
		return "every address"
	}
	return strings.Join(hosts, ", ")
}

// listen binds every host on port, all or none. Auto's addresses are
// bound with IP_FREEBIND: they come from the configuration netmgr
// applied, and an IPv6 one is tentative for a second (duplicate address
// detection) - net.ipv6.ip_nonlocal_bind, 1 on a node, may be set to 0.
func listen(hosts []string, port string, freebind bool) ([]net.Listener, error) {
	var lc net.ListenConfig
	if freebind {
		lc.Control = func(network, _ string, c syscall.RawConn) error {
			var err error
			if cerr := c.Control(func(fd uintptr) {
				if network == "tcp6" {
					err = unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_FREEBIND, 1)
				} else {
					err = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1)
				}
			}); cerr != nil {
				return cerr
			}
			return err
		}
	}
	var out []net.Listener
	for _, h := range hosts {
		addr := net.JoinHostPort(h, port)
		l, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			for _, o := range out {
				_ = o.Close()
			}
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		out = append(out, l)
	}
	return out, nil
}

// serve starts an HTTP server on lis as s.srv. Called with mu held.
func (s *Server) serve(lis []net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.serveMetrics)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>Janus exporter</title><h1>Janus exporter</h1><p><a href="/metrics">Metrics</a></p>`)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	s.srv = srv
	for _, l := range lis {
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("exporter: %v", err)
			}
		}()
	}
}

// Status reports the running configuration and whether it's listening;
// lastErr is the last Apply failure, if any.
func (s *Server) Status() (cfg Config, listening bool, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, s.srv != nil, s.lastErr
}

// Addresses is what it listens on now, an address each - none for
// every address (or not listening).
func (s *Server) Addresses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return nil
	}
	var out []string
	for _, h := range s.hosts {
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

// Stop closes the listener.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		_ = s.srv.Close()
		s.srv = nil
	}
}

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = Write(w, s.Gather())
}

// Gather runs every collector once - what a scrape would return.
func (s *Server) Gather() []Family {
	var families []Family
	for _, c := range s.collectors {
		families = append(families, c()...)
	}
	return families
}
