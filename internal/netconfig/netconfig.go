// Package netconfig is the node's network configuration document -
// janus.v1alpha1.NetworkConfig (hostname, interfaces and VLANs, DNS,
// NTP) - as stored on the persistent STATE partition and given at node
// creation (Install, `janusctl image seed`, NoCloud user-data): parsing,
// validation, defaults and persistence. Pure logic and file I/O only;
// applying a configuration to the running system is internal/netmgr's
// job.
package netconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Dir is where rootfs/init bind-mounts STATE's network/ subdirectory - a
// var so tests can point it elsewhere, like internal/bootcommit.Dir.
var Dir = "/etc/janus/network"

// FileName is the configuration file in Dir, and in STATE's network/.
const FileName = "config.json"

const (
	MaxNTPServers = 2
	// MaxDNSServers matches resolv.conf's own limit (MAXNS): resolvers
	// ignore any nameserver line past the third.
	MaxDNSServers    = 3
	MaxSearchDomains = 6
	DefaultNTPServer = "pool.ntp.org"

	DefaultConfirmTimeout = 30 * time.Second
	MaxConfirmTimeout     = 300 * time.Second

	// maxHostname is Linux's HOST_NAME_MAX.
	maxHostname = 64
	// maxIfName is IFNAMSIZ minus the terminating NUL.
	maxIfName = 15
)

// Load returns the persisted configuration, or an empty one and
// isDefault=true when none was ever saved.
func Load() (cfg *janusv1alpha1.NetworkConfig, isDefault bool, err error) {
	data, err := os.ReadFile(filepath.Join(Dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return &janusv1alpha1.NetworkConfig{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	cfg, err = Parse(data)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", filepath.Join(Dir, FileName), err)
	}
	return cfg, false, nil
}

// Exists reports whether a configuration was ever saved.
func Exists() bool {
	_, err := os.Stat(filepath.Join(Dir, FileName))
	return err == nil
}

// Save persists cfg atomically: written to a temporary file, synced,
// renamed over the previous one, and the directory synced - a power cut
// leaves either the old or the new configuration, never a torn one.
func Save(cfg *janusv1alpha1.NetworkConfig) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	data, err := Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(Dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(Dir, FileName)); err != nil {
		return err
	}
	d, err := os.Open(Dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Parse reads the JSON form of a NetworkConfig (protojson field names,
// either snake_case or lowerCamelCase) and validates it. Unknown fields
// are errors, so a typo isn't silently ignored.
func Parse(data []byte) (*janusv1alpha1.NetworkConfig, error) {
	cfg := &janusv1alpha1.NetworkConfig{}
	if err := protojson.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse network configuration: %w", err)
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Marshal is the stored and displayed JSON form: indented, snake_case.
// Indented by encoding/json rather than protojson's Multiline, whose
// whitespace is deliberately unstable (it varies between builds, to
// keep callers from depending on it) - this output is shown, diffed and
// grepped.
func Marshal(cfg *janusv1alpha1.NetworkConfig) ([]byte, error) {
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// Equal reports whether two configurations are the same.
func Equal(a, b *janusv1alpha1.NetworkConfig) bool {
	return proto.Equal(a, b)
}

// Validate checks everything that can be checked without the running
// system (which interfaces exist is internal/netmgr's to check). Errors
// name the offending field, for a caller-facing message.
func Validate(cfg *janusv1alpha1.NetworkConfig) error {
	if cfg == nil {
		return errors.New("network configuration is missing")
	}
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if h := cfg.GetHostname(); h != "" {
		if err := ValidateHostname(h); err != nil {
			fail("hostname: %v", err)
		}
	}

	names := map[string]bool{}
	macs := map[string]string{}
	vlans := map[string]string{}
	addrs := map[netip.Addr]string{}
	dhcp := ""
	listed := map[string]*janusv1alpha1.NetworkInterface{}
	for _, ifc := range cfg.GetInterfaces() {
		listed[ifc.GetName()] = ifc
	}
	for i, ifc := range cfg.GetInterfaces() {
		where := fmt.Sprintf("interfaces[%d]", i)
		name := ifc.GetName()
		if err := validateIfName(name); err != nil {
			fail("%s.name: %v", where, err)
		} else {
			where = fmt.Sprintf("interface %s", name)
			if names[name] {
				fail("%s: listed twice", where)
			}
			names[name] = true
		}

		if m := ifc.GetMac(); m != "" {
			hw, err := net.ParseMAC(m)
			if err != nil || len(hw) != 6 {
				fail("%s: mac %q isn't an Ethernet MAC address", where, m)
			} else if other, ok := macs[hw.String()]; ok {
				fail("%s: mac %s is also %s's", where, hw, other)
			} else {
				macs[hw.String()] = name
			}
		}

		if v := ifc.GetVlan(); v != nil {
			if ifc.GetMac() != "" {
				fail("%s: a VLAN interface can't be matched by mac", where)
			}
			if v.GetId() < 1 || v.GetId() > 4094 {
				fail("%s: VLAN id %d isn't in 1-4094", where, v.GetId())
			}
			switch p := v.GetParent(); {
			case p == "":
				fail("%s: VLAN parent is missing", where)
			case p == name:
				fail("%s: a VLAN can't be its own parent", where)
			case listed[p] != nil && listed[p].GetVlan() != nil:
				fail("%s: VLAN parent %s is itself a VLAN (stacked VLANs aren't supported)", where, p)
			case listed[p] != nil && listed[p].GetMode() == janusv1alpha1.AddressingMode_ADDRESSING_MODE_DISABLED:
				fail("%s: VLAN parent %s is disabled", where, p)
			default:
				key := fmt.Sprintf("%s/%d", p, v.GetId())
				if other, ok := vlans[key]; ok {
					fail("%s: VLAN %d on %s is already %s", where, v.GetId(), p, other)
				}
				vlans[key] = name
			}
		}

		if mtu := ifc.GetMtu(); mtu != 0 && (mtu < 68 || mtu > 65535) {
			fail("%s: mtu %d isn't in 68-65535", where, mtu)
		}

		if ifc.GetMode() == janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP {
			if dhcp != "" {
				fail("%s: only one interface can use DHCP (the kernel's boot-time DHCP configures a single interface), %s already does", where, dhcp)
			}
			dhcp = name
			if ifc.GetVlan() != nil {
				fail("%s: DHCP isn't available on a VLAN (only the kernel's boot-time DHCP exists for now)", where)
			}
		}

		static := ifc.GetMode() == janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC
		if !static {
			if len(ifc.GetAddresses()) > 0 || ifc.GetGateway() != "" || ifc.GetGateway6() != "" {
				fail("%s: addresses and gateways only apply in static mode", where)
			}
			continue
		}
		if len(ifc.GetAddresses()) == 0 {
			fail("%s: static mode needs at least one address", where)
		}
		var prefixes []netip.Prefix
		for _, a := range ifc.GetAddresses() {
			p, err := netip.ParsePrefix(a)
			switch {
			case err != nil:
				fail("%s: address %q isn't in CIDR form (e.g. 192.0.2.10/24)", where, a)
				continue
			case p.Bits() == 0:
				fail("%s: address %s has a zero-length prefix", where, a)
				continue
			case !p.Addr().IsGlobalUnicast(): // private ranges included
				fail("%s: address %s isn't a unicast address", where, a)
				continue
			}
			if other, ok := addrs[p.Addr()]; ok {
				fail("%s: address %s is also %s's", where, p.Addr(), other)
			}
			addrs[p.Addr()] = name
			prefixes = append(prefixes, p)
		}
		if gw := ifc.GetGateway(); gw != "" {
			g, err := netip.ParseAddr(gw)
			if err != nil || !g.Is4() {
				fail("%s: gateway %q isn't an IPv4 address", where, gw)
			} else if !onLink(g, prefixes) {
				fail("%s: gateway %s isn't in any of the interface's IPv4 subnets", where, gw)
			}
		}
		if gw := ifc.GetGateway6(); gw != "" {
			g, err := netip.ParseAddr(gw)
			if err != nil || !g.Is6() || g.Is4In6() {
				fail("%s: gateway6 %q isn't an IPv6 address", where, gw)
			} else if !g.IsLinkLocalUnicast() && !onLink(g, prefixes) {
				fail("%s: gateway6 %s is neither link-local nor in any of the interface's IPv6 subnets", where, gw)
			}
		}
	}

	dns := cfg.GetDns()
	if len(dns.GetServers()) > MaxDNSServers {
		fail("dns.servers: at most %d (resolv.conf ignores the rest)", MaxDNSServers)
	}
	for _, s := range dns.GetServers() {
		if _, err := netip.ParseAddr(s); err != nil {
			fail("dns.servers: %q isn't an IP address", s)
		}
	}
	if len(dns.GetSearch()) > MaxSearchDomains {
		fail("dns.search: at most %d domains", MaxSearchDomains)
	}
	for _, d := range dns.GetSearch() {
		if err := validateDomain(d); err != nil {
			fail("dns.search: %q: %v", d, err)
		}
	}

	if len(cfg.GetNtp().GetServers()) > MaxNTPServers {
		fail("ntp.servers: at most %d", MaxNTPServers)
	}
	for _, s := range cfg.GetNtp().GetServers() {
		if _, _, err := SplitNTPServer(s); err != nil {
			fail("ntp.servers: %q: %v", s, err)
		}
	}
	return errors.Join(errs...)
}

func onLink(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Masked().Contains(a) {
			return true
		}
	}
	return false
}

// validateIfName follows the kernel's own dev_valid_name().
func validateIfName(n string) error {
	switch {
	case n == "":
		return errors.New("is empty")
	case len(n) > maxIfName:
		return fmt.Errorf("%q is longer than %d characters", n, maxIfName)
	case n == "." || n == "..":
		return fmt.Errorf("%q isn't a valid interface name", n)
	case strings.ContainsAny(n, "/: \t\n\r"):
		return fmt.Errorf("%q contains '/', ':' or whitespace", n)
	}
	return nil
}

// ValidateHostname accepts an RFC 1123 host name (one label or a dotted
// FQDN) that fits the kernel's 64 bytes.
func ValidateHostname(h string) error {
	if len(h) > maxHostname {
		return fmt.Errorf("%q is longer than %d characters", h, maxHostname)
	}
	return validateDomain(h)
}

func validateDomain(d string) error {
	if d == "" {
		return errors.New("is empty")
	}
	if len(d) > 253 {
		return errors.New("is longer than 253 characters")
	}
	for _, label := range strings.Split(strings.TrimSuffix(d, "."), ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("label %q must be 1-63 characters", label)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("label %q can't start or end with '-'", label)
		}
		for _, c := range label {
			letterOrDigit := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
			if !letterOrDigit && c != '-' {
				return fmt.Errorf("label %q may only contain letters, digits and '-'", label)
			}
		}
	}
	return nil
}

// SplitNTPServer splits "host" or "host:port" (IPv6 literals in
// brackets), defaulting the port to 123.
func SplitNTPServer(s string) (host string, port int, err error) {
	host, portStr := s, "123"
	if strings.HasPrefix(s, "[") || strings.Count(s, ":") == 1 {
		if host, portStr, err = net.SplitHostPort(s); err != nil {
			return "", 0, err
		}
	}
	port, err = strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port %q isn't in 1-65535", portStr)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		if err := validateDomain(host); err != nil {
			return "", 0, fmt.Errorf("host: %v", err)
		}
	}
	return host, port, nil
}

// DefaultHostname is the hostname used when neither the configuration
// nor DHCP provide one: "janus-" and the MAC's last three bytes, stable
// across reboots and distinct between nodes.
func DefaultHostname(mac net.HardwareAddr) string {
	if len(mac) < 3 {
		return "janus"
	}
	return fmt.Sprintf("janus-%02x%02x%02x", mac[len(mac)-3], mac[len(mac)-2], mac[len(mac)-1])
}

// NTP sources, as TimeStatus.source reports them.
const (
	SourceConfigured = "configured"
	SourceDHCP       = "dhcp"
	SourceDefault    = "default"
)

// EffectiveNTP picks the servers to use: the configured ones, else those
// DHCP provides, else pool.ntp.org.
func EffectiveNTP(cfg *janusv1alpha1.NetworkConfig, dhcpServers []string) (servers []string, source string) {
	if s := cfg.GetNtp().GetServers(); len(s) > 0 {
		return append([]string(nil), s...), SourceConfigured
	}
	if len(dhcpServers) > 0 {
		return firstUnique(dhcpServers, MaxNTPServers), SourceDHCP
	}
	return []string{DefaultNTPServer}, SourceDefault
}

// EffectiveDNS picks the resolvers and search domains: configured ones
// where set, else what DHCP provides.
func EffectiveDNS(cfg *janusv1alpha1.NetworkConfig, dhcpServers, dhcpSearch []string) (servers, search []string) {
	servers = cfg.GetDns().GetServers()
	if len(servers) == 0 {
		servers = firstUnique(dhcpServers, MaxDNSServers)
	}
	search = cfg.GetDns().GetSearch()
	if len(search) == 0 {
		search = firstUnique(dhcpSearch, MaxSearchDomains)
	}
	return append([]string(nil), servers...), append([]string(nil), search...)
}

// ResolvConf renders /etc/resolv.conf.
func ResolvConf(servers, search []string) []byte {
	var b strings.Builder
	b.WriteString("# Written by janusd from the node's network configuration.\n")
	if len(search) > 0 {
		b.WriteString("search " + strings.Join(search, " ") + "\n")
	}
	for _, s := range servers {
		b.WriteString("nameserver " + s + "\n")
	}
	return []byte(b.String())
}

func firstUnique(in []string, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == max {
			break
		}
	}
	return out
}
