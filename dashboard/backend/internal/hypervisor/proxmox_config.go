package hypervisor

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const KindProxmox Kind = "proxmox"

// ProxmoxConfig reaches a Proxmox VE node's API with an API token whose
// rights cover only the pool, storages and networks below
// (docs/hypervisors.md: preparing a Proxmox host). The token's secret
// isn't here: it's Hypervisor.TokenSecret, kept apart and never shown.
type ProxmoxConfig struct {
	// URL is the API: "https://host:8006" (the port defaults to 8006).
	URL string `json:"url"`
	// Node is the cluster node machines are created on.
	Node string `json:"node"`
	// TokenID is the API token: "user@realm!name".
	TokenID string `json:"token_id"`
	// Fingerprint pins the API's TLS certificate (SHA-256, as Proxmox
	// shows it), once the operator confirmed it. CACert, instead, is the
	// CA that signs it - for a certificate renewed now and then. Neither:
	// not trusted yet.
	Fingerprint string `json:"fingerprint,omitempty"`
	CACert      string `json:"ca_cert,omitempty"`
	// Pool is the resource pool the machines belong to - the token's
	// rights on virtual machines come from it.
	Pool string `json:"pool"`
	// Storage holds the machines' disks; ImageStorage (a directory
	// storage with the import and iso content types) the base images and
	// the machines' NoCloud volumes.
	Storage      string `json:"storage"`
	ImageStorage string `json:"image_storage"`
	// Networks the machines may use: a bridge ("vmbr0", untagged), a
	// VLAN on one ("vmbr0.20"), a range of VLANs ("vmbr0.100-199") or any
	// VLAN on it ("vmbr0.*"). A machine's interface names one network:
	// "vmbr0" or "vmbr0.150".
	Networks []string `json:"networks"`
	// NamePrefix starts every virtual machine's name. Empty:
	// DefaultNamePrefix.
	NamePrefix string `json:"name_prefix,omitempty"`
	// VMIDs is the range new machines' IDs come from, "first-last".
	// Empty: the cluster's next free ID.
	VMIDs string `json:"vmids,omitempty"`
}

// Prefix is the virtual machine name prefix to use.
func (c *ProxmoxConfig) Prefix() string {
	if c.NamePrefix != "" {
		return c.NamePrefix
	}
	return DefaultNamePrefix
}

// AllowsNetwork reports whether machines may be attached to network -
// "vmbr0" or "vmbr0.150" - one of Networks names or covers.
func (c *ProxmoxConfig) AllowsNetwork(network string) bool {
	bridge, vlan, err := ParseProxmoxNetwork(network)
	if err != nil {
		return false
	}
	for _, entry := range c.Networks {
		b, first, last, err := ParseProxmoxAllowed(entry)
		if err != nil || b != bridge {
			continue
		}
		if (vlan == 0 && first == 0) || (vlan > 0 && first > 0 && vlan >= first && vlan <= last) {
			return true
		}
	}
	return false
}

// MaxVLANRange is the widest range of VLANs a network may name: wider is
// "bridge.*" (the host's preparation gives each VLAN of a range its own
// right).
const MaxVLANRange = 256

// ParseProxmoxAllowed reads an allowed network: its bridge and VLANs -
// 0, 0 for the bridge itself (untagged), first to last otherwise
// ("vmbr0.20": 20, 20; "vmbr0.100-199"; "vmbr0.*": 1, 4094).
func ParseProxmoxAllowed(entry string) (bridge string, first, last int, err error) {
	m := pveAllowedRe.FindStringSubmatch(entry)
	if m == nil {
		return "", 0, 0, fmt.Errorf("network %q isn't a bridge, bridge.vlan, bridge.first-last or bridge.* (vmbr0, vmbr0.20, vmbr0.100-199, vmbr0.*)", entry)
	}
	switch {
	case m[2] == "*":
		return m[1], 1, 4094, nil
	case m[3] == "":
		return m[1], 0, 0, nil
	}
	first, _ = strconv.Atoi(m[3])
	last = first
	if m[4] != "" {
		last, _ = strconv.Atoi(m[4])
	}
	if first < 1 || last > 4094 || last < first {
		return "", 0, 0, fmt.Errorf("network %q: VLANs go from 1 to 4094, first to last", entry)
	}
	if last-first+1 > MaxVLANRange {
		return "", 0, 0, fmt.Errorf("network %q: at most %d VLANs in a range - %s.* allows them all", entry, MaxVLANRange, m[1])
	}
	return m[1], first, last, nil
}

// APIBase is the API's base URL, ".../api2/json".
func (c *ProxmoxConfig) APIBase() string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return c.URL
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "8006")
	}
	u.Path = "/api2/json"
	return u.String()
}

// Address is the API's host:port.
func (c *ProxmoxConfig) Address() string {
	u, err := url.Parse(c.APIBase())
	if err != nil {
		return ""
	}
	return u.Host
}

// VMIDRange is VMIDs as numbers; ok is false when it's unset.
func (c *ProxmoxConfig) VMIDRange() (first, last int, ok bool) {
	a, b, found := strings.Cut(c.VMIDs, "-")
	if !found {
		return 0, 0, false
	}
	first, err1 := strconv.Atoi(strings.TrimSpace(a))
	last, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return first, last, true
}

// ParseProxmoxNetwork splits "vmbr0.20" into its bridge and VLAN tag (0:
// none).
func ParseProxmoxNetwork(network string) (bridge string, tag int, err error) {
	m := pveNetworkRe.FindStringSubmatch(network)
	if m == nil {
		return "", 0, fmt.Errorf("network %q isn't a bridge or bridge.vlan (vmbr0, vmbr0.20)", network)
	}
	if m[2] != "" {
		tag, _ = strconv.Atoi(m[2])
		if tag < 1 || tag > 4094 {
			return "", 0, fmt.Errorf("network %q: VLAN %d isn't between 1 and 4094", network, tag)
		}
	}
	return m[1], tag, nil
}

var (
	pveNetworkRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]{0,14})(?:\.([0-9]{1,4}))?$`)
	pveAllowedRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]{0,14})(?:\.(?:(\*)|([0-9]{1,4})(?:-([0-9]{1,4}))?))?$`)
	pveIDRe      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,62}$`)
	pveTokenRe   = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+![A-Za-z][A-Za-z0-9._-]*$`)
	pveVMIDsRe   = regexp.MustCompile(`^[0-9]{3,9}-[0-9]{3,9}$`)
	// A Proxmox virtual machine's name is a DNS name.
	pvePrefixRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,31}$`)
	hexColonsRe = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){31}$`)
)

// NormalizeFingerprint is a SHA-256 certificate fingerprint as Proxmox
// shows it ("8D:B4:..."), whatever its case.
func NormalizeFingerprint(fp string) string {
	return strings.ToUpper(strings.TrimSpace(fp))
}

func (c *ProxmoxConfig) validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("API URL %q isn't https://host[:port]", c.URL)
	}
	if !pveIDRe.MatchString(c.Node) {
		return fmt.Errorf("node %q isn't a valid node name", c.Node)
	}
	if !pveTokenRe.MatchString(c.TokenID) {
		return fmt.Errorf("API token %q isn't user@realm!name", c.TokenID)
	}
	for what, v := range map[string]string{"pool": c.Pool, "storage": c.Storage, "image storage": c.ImageStorage} {
		if !pveIDRe.MatchString(v) {
			return fmt.Errorf("%s %q isn't a valid name", what, v)
		}
	}
	if len(c.Networks) == 0 {
		return errors.New("at least one network is required")
	}
	for _, n := range c.Networks {
		if _, _, _, err := ParseProxmoxAllowed(n); err != nil {
			return err
		}
	}
	if c.NamePrefix != "" && !pvePrefixRe.MatchString(c.NamePrefix) {
		return fmt.Errorf("name prefix %q: letters, digits, '.' and '-' only - a Proxmox virtual machine's name is a DNS name", c.NamePrefix)
	}
	if c.VMIDs != "" {
		first, last, ok := c.VMIDRange()
		if !pveVMIDsRe.MatchString(c.VMIDs) || !ok || first < 100 || last < first {
			return fmt.Errorf("VM IDs %q isn't a range first-last, from 100 up", c.VMIDs)
		}
	}
	if c.Fingerprint != "" && !hexColonsRe.MatchString(c.Fingerprint) {
		return fmt.Errorf("fingerprint %q isn't a SHA-256 fingerprint (AA:BB:...)", c.Fingerprint)
	}
	if c.CACert != "" {
		if _, err := ParseCACert(c.CACert); err != nil {
			return err
		}
	}
	return nil
}

// ParseCACert reads a PEM CA certificate (the first one).
func ParseCACert(s string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("CA certificate: not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("CA certificate: %w", err)
	}
	return cert, nil
}

// --- what every kind answers alike ---

// Prefix is the name prefix of the hypervisor's virtual machines.
func (h *Hypervisor) Prefix() string {
	switch {
	case h.Libvirt != nil:
		return h.Libvirt.Prefix()
	case h.Proxmox != nil:
		return h.Proxmox.Prefix()
	}
	return DefaultNamePrefix
}

// Networks are the networks its machines may use.
func (h *Hypervisor) Networks() []string {
	switch {
	case h.Libvirt != nil:
		return h.Libvirt.Networks
	case h.Proxmox != nil:
		return h.Proxmox.Networks
	}
	return nil
}

// AllowsNetwork reports whether its machines may be attached to network.
func (h *Hypervisor) AllowsNetwork(network string) bool {
	switch {
	case h.Libvirt != nil:
		return h.Libvirt.AllowsNetwork(network)
	case h.Proxmox != nil:
		return h.Proxmox.AllowsNetwork(network)
	}
	return false
}

// Trusted reports whether the operator vouched for the host: a pinned
// SSH host key (libvirt), a pinned certificate or a CA (Proxmox).
func (h *Hypervisor) Trusted() bool {
	switch {
	case h.Libvirt != nil:
		return h.Libvirt.HostKey != ""
	case h.Proxmox != nil:
		return h.Proxmox.Fingerprint != "" || h.Proxmox.CACert != ""
	}
	return false
}
