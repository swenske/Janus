// Package birdexporter runs the prometheus-bird-exporter extension's
// bird_exporter (docs/bgp.md "bird_exporter"): BIRD's protocols as
// Prometheus metrics, read over BIRD's restricted control socket (bgp.
// ReadOnlySocketPath - show commands only), served by default on the
// node's main interface like node_exporter (internal/nodeexporter), so
// never on a VIP or an anycast address BIRD announces. Its settings -
// whether it runs, its address and port - are kept with janusd's
// (NetworkService.BGPExporterConfigGet/Set).
package birdexporter

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/swenske/Janus/internal/bgp"
)

// ServiceID is the extension's service.
const ServiceID = "prometheus-bird-exporter"

// DefaultPort is bird_exporter's own.
const DefaultPort = 9324

// Auto is the main interface's address, followed as the network changes.
const Auto = "auto"

// apiPort is janusd's: never bird_exporter's.
const apiPort = 9505

// Dir is where the settings are kept: janusd's -config-dir (STATE's
// config/ on a node). A var for tests.
var Dir = "/etc/janus/config"

const configFile = "bird-exporter.json"

// Config is bird_exporter's settings.
type Config struct {
	Enabled bool `json:"enabled"`
	// Address is Auto, "" for every address, or one IP.
	Address string `json:"address"`
	Port    uint32 `json:"port"`
}

// DefaultConfig is what a node runs until it's changed: on, the main
// interface, port 9324.
func DefaultConfig() Config { return Config{Enabled: true, Address: Auto, Port: DefaultPort} }

// Validate checks c and normalizes it: port 0 is DefaultPort.
func (c *Config) Validate() error {
	c.Port = cmp.Or(c.Port, DefaultPort)
	if c.Port > 65535 {
		return fmt.Errorf("port %d is out of range", c.Port)
	}
	if c.Port == apiPort {
		return fmt.Errorf("port %d is janusd's API", apiPort)
	}
	if c.Address != "" && c.Address != Auto && net.ParseIP(c.Address) == nil {
		return fmt.Errorf("%q isn't an IP address (or %q)", c.Address, Auto)
	}
	return nil
}

// Host is where c listens: Auto's is the main interface's IPv4 address,
// else its IPv6 one, else - no main address known yet - every address
// until netmgr knows it; "" is every address.
func Host(c Config, main []netip.Addr) string {
	if c.Address != Auto {
		return c.Address
	}
	host := ""
	for _, a := range main {
		if a.Is4() {
			return a.String()
		}
		if host == "" {
			host = a.String()
		}
	}
	return host
}

// Args is bird_exporter's command line for c (validated): one listen
// address (bird_exporter takes one), BIRD 2's format (-bird.v2), the
// generic metric names (-format.new), every protocol but OSPF:
// bird_exporter 1.7.1 can't read an OSPF protocol's channel from BIRD
// 2.20's reply (its "Channel ipv4" line comes first, behind a reply code
// the parser doesn't expect) and then fails the whole scrape with HTTP
// 500 - measured on a node; janus_bgp_* covers OSPF's state meanwhile.
func Args(c Config, main []netip.Addr) []string {
	return []string{
		"-web.listen-address=" + net.JoinHostPort(Host(c, main), strconv.Itoa(int(c.Port))),
		"-bird.socket=" + bgp.ReadOnlySocketPath(),
		"-bird.v2",
		"-format.new",
		"-proto.ospf=false",
	}
}

// Supervisor is what runs bird_exporter: janusd's extension manager.
type Supervisor interface {
	// Configure sets its arguments and whether it runs (restarting it).
	Configure(id string, args []string, enabled bool) error
	// SetArgs changes its arguments, restarting it only if it runs.
	SetArgs(id string, args []string) error
}

// mu orders Apply, Boot and Follow: a network change followed while new
// settings are being applied mustn't run the old ones.
var mu sync.Mutex

// Boot runs bird_exporter as its saved settings say (janusd's start). A
// broken settings file runs the defaults, and says so.
func Boot(s Supervisor, main []netip.Addr) error {
	mu.Lock()
	defer mu.Unlock()
	c, _, err := Load()
	if cerr := s.Configure(ServiceID, Args(c, main), c.Enabled); cerr != nil {
		return cerr
	}
	return err
}

// Apply saves c (validated) and runs bird_exporter with it
// (BGPExporterConfigSet) - saved first: what runs is always what a
// reboot would run.
func Apply(s Supervisor, c Config, main []netip.Addr) error {
	mu.Lock()
	defer mu.Unlock()
	if err := Save(c); err != nil {
		return fmt.Errorf("save the settings: %w", err)
	}
	if err := s.Configure(ServiceID, Args(c, main), c.Enabled); err != nil {
		return fmt.Errorf("apply the settings: %w", err)
	}
	return nil
}

// Follow gives bird_exporter the main interface's new address when it
// listens there (netmgr's OnChange) - restarted only if it changed and it
// runs; one stopped on purpose stays stopped.
func Follow(s Supervisor, main []netip.Addr) error {
	mu.Lock()
	defer mu.Unlock()
	c, _, err := Load()
	if err != nil || !c.Enabled || c.Address != Auto {
		return err
	}
	return s.SetArgs(ServiceID, Args(c, main))
}

// Load reads the saved settings; isDefault reports that none were.
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

// Save writes cfg durably: a temporary file synced, renamed over the old
// one, and the directory synced.
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
