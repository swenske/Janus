package consul

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/swenske/Janus/internal/modcfg"
)

// The managed layer (docs/consul.md "From the Controller"): the part of
// the agent's configuration a Controller sets - what makes the node a
// client of its Consul cluster (datacenter, servers, gossip key, its ACL
// token, the servers' RPC CA, its metadata) and which of its own
// services janusd registers there (register.go). It is saved apart from
// the operator's configuration (consul.hcl, untouched), on STATE as
// ManagedFile - 0600: the token and the gossip key are in it - and
// rendered into janusd's fragment (janus.json), which the agent loads
// after consul.hcl: the Controller's keys win, lists (retry_join) are
// added to the operator's. With a managed layer and no operator
// configuration, the agent runs on the layer alone: an empty consul.hcl
// is put where the manifest waits for it.

// ManagedFile is the managed layer on STATE, under modcfg.Dir.
const ManagedFile = "consul-managed.json"

const (
	managedDir    = "managed" // under RunDir: the RPC CA
	managedCAFile = "ca.pem"
	// placeholder is the agent's consul.hcl when the operator has none
	// and a Controller manages a layer.
	placeholder = "# No configuration of the operator's: the agent runs on the Controller's\n# layer alone (janus.json, docs/consul.md \"From the Controller\").\n"
)

// Managed is the managed layer.
type Managed struct {
	ManagedBy     string            `json:"managed_by"`
	Datacenter    string            `json:"datacenter"`
	NodeName      string            `json:"node_name"`
	RetryJoin     []string          `json:"retry_join,omitempty"`
	AdvertiseAddr string            `json:"advertise_addr,omitempty"`
	Encrypt       string            `json:"encrypt,omitempty"`     // secret
	AgentToken    string            `json:"agent_token,omitempty"` // secret
	CAPEM         string            `json:"ca_pem,omitempty"`
	NodeMeta      map[string]string `json:"node_meta,omitempty"`
	Services      Toggles           `json:"services"`
}

// Toggles say which of its services the node registers.
type Toggles struct {
	Janus     bool `json:"janus"`
	Exporters bool `json:"exporters"`
	Frontends bool `json:"frontends"`
	URLs      bool `json:"urls"`
}

// IsSet reports whether m is a layer at all: a Controller set it.
func (m *Managed) IsSet() bool { return m != nil && m.ManagedBy != "" }

// Redacted is m without its secrets.
func (m Managed) Redacted() Managed {
	m.Encrypt, m.AgentToken = "", ""
	m.RetryJoin = slices.Clone(m.RetryJoin)
	if m.NodeMeta != nil {
		meta := make(map[string]string, len(m.NodeMeta))
		for k, v := range m.NodeMeta {
			meta[k] = v
		}
		m.NodeMeta = meta
	}
	return m
}

var (
	datacenterPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	nodeNamePattern   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62})$`)
	joinPattern       = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,255}$`)
	metaKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	tokenPattern      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

const (
	maxRetryJoin = 16
	maxNodeMeta  = 64 // Consul's own limits on node_meta
	maxMetaValue = 512
	maxCAPEM     = 64 << 10
)

// Validate says what's wrong with m, as Consul would refuse it or worse
// - a token or a key the agent can't use.
func (m Managed) Validate() []string {
	var errs []string
	if strings.TrimSpace(m.ManagedBy) == "" || len(m.ManagedBy) > 200 {
		errs = append(errs, "managed_by: who sets the layer, at most 200 characters")
	}
	if !datacenterPattern.MatchString(m.Datacenter) {
		errs = append(errs, fmt.Sprintf("datacenter %q: letters, digits, '-', '_'", m.Datacenter))
	}
	if !nodeNamePattern.MatchString(m.NodeName) {
		errs = append(errs, fmt.Sprintf("node_name %q: letters, digits, '.', '-', '_', at most 63", m.NodeName))
	}
	if len(m.RetryJoin) == 0 {
		errs = append(errs, "retry_join: no server to join")
	}
	if len(m.RetryJoin) > maxRetryJoin {
		errs = append(errs, fmt.Sprintf("retry_join: over %d servers", maxRetryJoin))
	}
	for _, j := range m.RetryJoin {
		if !joinPattern.MatchString(j) {
			errs = append(errs, fmt.Sprintf("retry_join %q: a host or host:port", j))
		}
	}
	if m.AdvertiseAddr != "" && net.ParseIP(m.AdvertiseAddr) == nil {
		errs = append(errs, fmt.Sprintf("advertise_addr %q isn't an IP address", m.AdvertiseAddr))
	}
	if m.Encrypt != "" {
		if k, err := base64.StdEncoding.DecodeString(m.Encrypt); err != nil || (len(k) != 16 && len(k) != 24 && len(k) != 32) {
			errs = append(errs, "encrypt: a base64 key of 16, 24 or 32 bytes (consul keygen)")
		}
	}
	if m.AgentToken == "" {
		errs = append(errs, "agent_token: none - the agent can't do anything under ACLs without one")
	} else if !tokenPattern.MatchString(m.AgentToken) {
		errs = append(errs, "agent_token: a token's secret ID (a UUID)")
	}
	if m.CAPEM != "" {
		if err := checkCAPEM(m.CAPEM); err != nil {
			errs = append(errs, "ca_pem: "+err.Error())
		}
	}
	if len(m.NodeMeta) > maxNodeMeta {
		errs = append(errs, fmt.Sprintf("node_meta: over %d keys", maxNodeMeta))
	}
	for _, k := range sortedKeys(m.NodeMeta) {
		switch {
		case !metaKeyPattern.MatchString(k):
			errs = append(errs, fmt.Sprintf("node_meta key %q: letters, digits, '-', '_', at most 128", k))
		case strings.HasPrefix(k, "consul-"):
			errs = append(errs, fmt.Sprintf("node_meta key %q: consul- is Consul's", k))
		case len(m.NodeMeta[k]) > maxMetaValue:
			errs = append(errs, fmt.Sprintf("node_meta %s: over %d characters", k, maxMetaValue))
		}
	}
	return errs
}

func checkCAPEM(s string) error {
	if len(s) > maxCAPEM {
		return fmt.Errorf("over %d bytes", maxCAPEM)
	}
	rest, n := []byte(s), 0
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return fmt.Errorf("a %s block: certificates only", b.Type)
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return err
		}
		n++
	}
	if n == 0 || len(bytes.TrimSpace(rest)) > 0 {
		return errors.New("not PEM certificates")
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// LoadManaged reads the saved layer; nil: none.
func LoadManaged() (*Managed, error) {
	data, err := os.ReadFile(filepath.Join(modcfg.Dir, ManagedFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Managed
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManagedFile, err)
	}
	if !m.IsSet() {
		return nil, nil
	}
	return &m, nil
}

func saveManaged(m *Managed) error {
	path := filepath.Join(modcfg.Dir, ManagedFile)
	if m == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(modcfg.Dir, 0o755); err != nil {
		return err
	}
	return writeSynced(path, append(data, '\n'), 0o600)
}

// managedCAPath is where the agent reads the servers' RPC CA.
func managedCAPath() string { return filepath.Join(RunDir, managedDir, managedCAFile) }

// fragment is janusd's part of the agent's configuration: the data
// directory, the node ID, and the managed layer m (nil: none) - main is
// the node's main IPv4 address, advertised when m names none.
func fragment(m *Managed, main string) ([]byte, error) {
	id, err := nodeID()
	if err != nil {
		return nil, err
	}
	f := map[string]any{
		"data_dir": filepath.Join(RunDir, "data"),
		"node_id":  id,
	}
	if m.IsSet() {
		f["datacenter"] = m.Datacenter
		f["node_name"] = m.NodeName
		f["retry_join"] = m.RetryJoin
		if addr := cmpOr(m.AdvertiseAddr, main); addr != "" {
			f["bind_addr"], f["advertise_addr"] = addr, addr
		}
		if m.Encrypt != "" {
			f["encrypt"] = m.Encrypt
		}
		f["acl"] = map[string]any{
			"enabled":        true,
			"default_policy": "deny",
			"down_policy":    "extend-cache",
			"tokens":         map[string]any{"agent": m.AgentToken},
		}
		if m.CAPEM != "" {
			f["tls"] = map[string]any{
				"defaults":     map[string]any{"ca_file": managedCAPath(), "verify_outgoing": true},
				"internal_rpc": map[string]any{"verify_server_hostname": true},
			}
			f["auto_encrypt"] = map[string]any{"tls": true}
		}
		if len(m.NodeMeta) > 0 {
			f["node_meta"] = m.NodeMeta
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
