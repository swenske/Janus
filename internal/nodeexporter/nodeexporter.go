// Package nodeexporter holds the settings of the prometheus-node-exporter
// extension: whether node_exporter runs, where it listens and which
// collectors it runs - saved on STATE, turned into node_exporter's
// command line for janusd's extension supervisor (docs/metrics.md).
//
// The collectors are a fixed list, not free-form flags: each one is
// known to work on a Janus node - what it reads is in the kernel, and
// allowed by the SELinux policy (hack/qemu-extensions-test.sh turns them
// all on and requires every one to succeed).
package nodeexporter

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// ServiceID is the extension's service in janusd's supervisor.
const ServiceID = "prometheus-node-exporter"

// DefaultPort is node_exporter's own default.
const DefaultPort = 9100

// apiPort is janusd's gRPC API, which node_exporter must not take.
const apiPort = 9505

// Dir holds the saved settings (STATE's config/ on a node) - a var for
// tests and janusd's -config-dir.
var Dir = "/etc/janus/config"

const configFile = "node-exporter.json"

// Collector is one node_exporter collector the settings can turn on.
type Collector struct {
	Name        string
	Description string
	// Default collectors run unless the settings say otherwise.
	Default bool
}

// Collectors lists every collector the settings accept, by name.
var Collectors = []Collector{
	{"arp", "ARP table entries per interface", false},
	{"conntrack", "connection tracking table usage", false},
	{"cpu", "CPU time per mode and core", true},
	{"cpufreq", "CPU frequencies", false},
	{"diskstats", "disk I/O", true},
	{"dmi", "hardware identity (vendor, product, BIOS)", false},
	{"entropy", "kernel entropy pool", false},
	{"filefd", "open file descriptors", true},
	{"filesystem", "filesystem sizes and use", true},
	{"interrupts", "interrupts per CPU and device", false},
	{"loadavg", "load average", true},
	{"meminfo", "memory", true},
	{"netclass", "network interfaces: link speed, carrier, MTU", false},
	{"netdev", "network traffic per interface", true},
	{"netstat", "network protocol counters (TCP, UDP, IP)", true},
	{"nvme", "NVMe disks: model, firmware, state", false},
	{"os", "OS release", true},
	{"pressure", "CPU, memory and I/O pressure (PSI)", true},
	{"sockstat", "sockets in use", true},
	{"softirqs", "soft interrupts per CPU", false},
	{"softnet", "packets processed and dropped per CPU", false},
	{"stat", "boot time, context switches, forks", true},
	{"thermal_zone", "thermal zones (bare metal)", false},
	{"time", "system time", true},
	{"timex", "clock synchronization (adjtimex)", true},
	{"udp_queues", "UDP socket queues", false},
	{"uname", "kernel version", true},
	{"vmstat", "virtual memory statistics", true},
}

// DefaultCollectors is the collectors run by default, sorted.
func DefaultCollectors() []string {
	var names []string
	for _, c := range Collectors {
		if c.Default {
			names = append(names, c.Name)
		}
	}
	return names
}

// Config is the extension's settings.
type Config struct {
	Enabled bool `json:"enabled"`
	// Address to listen on - empty for every address.
	Address    string   `json:"address"`
	Port       uint32   `json:"port"`
	Collectors []string `json:"collectors"`
}

// DefaultConfig is what a node runs until it's changed: on, every
// address, port 9100, the default collectors.
func DefaultConfig() Config {
	return Config{Enabled: true, Port: DefaultPort, Collectors: DefaultCollectors()}
}

// Validate checks c and normalizes it: port 0 is DefaultPort, no
// collectors the default ones, collectors sorted without duplicates.
func (c *Config) Validate() error {
	c.Port = cmp.Or(c.Port, DefaultPort)
	if c.Port > 65535 {
		return fmt.Errorf("port %d is out of range", c.Port)
	}
	if c.Port == apiPort {
		return fmt.Errorf("port %d is janusd's API", apiPort)
	}
	if c.Address != "" && net.ParseIP(c.Address) == nil {
		return fmt.Errorf("%q isn't an IP address", c.Address)
	}
	if len(c.Collectors) == 0 {
		c.Collectors = DefaultCollectors()
	}
	for _, name := range c.Collectors {
		if !slices.ContainsFunc(Collectors, func(k Collector) bool { return k.Name == name }) {
			return fmt.Errorf("unknown collector %q", name)
		}
	}
	c.Collectors = slices.Compact(slices.Sorted(slices.Values(c.Collectors)))
	return nil
}

// Args is node_exporter's command line for c (validated).
func Args(c Config) []string {
	args := []string{"--web.listen-address=" + net.JoinHostPort(c.Address, strconv.Itoa(int(c.Port))), "--collector.disable-defaults"}
	for _, name := range c.Collectors {
		args = append(args, "--collector."+name)
	}
	return args
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
