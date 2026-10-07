// Package exporter serves the node's own Prometheus metrics: what only
// Janus knows - certificate expiry, boot slot and pending upgrades,
// HAProxy as janusd manages it, extension services, time sync, SELinux -
// and nothing node_exporter or HAProxy's own exporter already provide.
// Plain HTTP on its own port (DefaultPort), on by default; enabling it
// and the port are set through the API and persisted in Dir.
//
// The text format is written here rather than through
// prometheus/client_golang: a handful of gauges and counters computed at
// scrape time doesn't need the library, and janusd stays small.
package exporter

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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

// Config is the exporter's persisted configuration.
type Config struct {
	Enabled bool   `json:"enabled"`
	Port    uint32 `json:"port"`
	// Address to listen on - empty for every address. A management
	// address keeps the metrics off the networks HAProxy serves.
	Address string `json:"address,omitempty"`
}

// DefaultConfig is what a node runs with until it's changed: on, on
// DefaultPort.
func DefaultConfig() Config { return Config{Enabled: true, Port: DefaultPort} }

// Validate checks c. Port 0 means DefaultPort; the address, if any, is
// an IP address of the node's.
func (c *Config) Validate() error {
	c.Port = cmp.Or(c.Port, DefaultPort)
	if c.Port > 65535 {
		return fmt.Errorf("port %d is out of range", c.Port)
	}
	if c.Address != "" && net.ParseIP(c.Address) == nil {
		return fmt.Errorf("%q isn't an IP address", c.Address)
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
	return os.Rename(tmp, filepath.Join(Dir, configFile))
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

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

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

	mu      sync.Mutex
	cfg     Config
	srv     *http.Server
	lastErr string
}

// New returns a Server that isn't listening yet; Apply starts it.
func New(collectors ...Collector) *Server {
	return &Server{collectors: collectors}
}

// Apply switches to cfg: stops the current listener and, if enabled,
// listens on the new address and port. The new ones are bound before
// the old listener closes, so if they can't be bound the previous one
// keeps running and the error is returned - except when only the
// address changes, on the same port: the old listener holds the port,
// so it closes first, and comes back if the new address can't be bound.
func (s *Server) Apply(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg == s.cfg && (s.srv != nil) == cfg.Enabled {
		return nil
	}
	if !cfg.Enabled {
		if s.srv != nil {
			_ = s.srv.Close()
			s.srv = nil
		}
		s.cfg, s.lastErr = cfg, ""
		log.Printf("exporter: disabled")
		return nil
	}
	previous := s.cfg
	samePort := s.srv != nil && previous.Port == cfg.Port
	if samePort {
		_ = s.srv.Close()
		s.srv = nil
	}
	listen := net.JoinHostPort(cfg.Address, strconv.Itoa(int(cfg.Port)))
	lis, err := net.Listen("tcp", listen)
	if err != nil {
		s.lastErr = err.Error()
		if samePort {
			if old, e := net.Listen("tcp", net.JoinHostPort(previous.Address, strconv.Itoa(int(previous.Port)))); e == nil {
				s.serve(old)
			} else {
				log.Printf("exporter: the previous listener couldn't come back either: %v", e)
			}
		}
		return fmt.Errorf("listen on %s: %w", listen, err)
	}
	if s.srv != nil {
		_ = s.srv.Close()
	}
	s.serve(lis)
	s.cfg, s.lastErr = cfg, ""
	log.Printf("exporter: metrics served on %s/metrics", listen)
	return nil
}

// serve starts an HTTP server on lis as s.srv. Called with mu held.
func (s *Server) serve(lis net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.serveMetrics)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>Janus exporter</title><h1>Janus exporter</h1><p><a href="/metrics">Metrics</a></p>`)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	s.srv = srv
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("exporter: %v", err)
		}
	}()
}

// Status reports the running configuration and whether it's listening;
// lastErr is the last Apply failure, if any.
func (s *Server) Status() (cfg Config, listening bool, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, s.srv != nil, s.lastErr
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
