package acme

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme/acmewire"
)

// The well-known directories, and the names they can be given by.
const (
	LetsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// ResolveDirectory is the URL of a configured directory.
func ResolveDirectory(d string) string {
	switch d {
	case "", "letsencrypt":
		return LetsEncrypt
	case "letsencrypt-staging":
		return LetsEncryptStaging
	}
	return d
}

const (
	defaultKeyType   = "ec256"
	defaultChallenge = acmewire.ChallengeHTTP01
	maxCertificates  = 100
	maxDomains       = 100
)

var (
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	settingPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	profilePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	labelPattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Marshal is a configuration's JSON form, as saved.
func Marshal(cfg *janusv1alpha1.ACMEConfig) ([]byte, error) {
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

// loadConfig reads the saved configuration; nil if there's none.
func loadConfig() (*janusv1alpha1.ACMEConfig, error) {
	data, err := os.ReadFile(filepath.Join(Dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := &janusv1alpha1.ACMEConfig{}
	if err := protojson.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", configFile, err)
	}
	return cfg, nil
}

// normalize fills the defaults in, and lowercases the domains.
func normalize(cfg *janusv1alpha1.ACMEConfig) {
	if cfg.Account == nil {
		cfg.Account = &janusv1alpha1.ACMEAccount{}
	}
	cfg.Account.Directory = strings.TrimSpace(cfg.Account.Directory)
	cfg.Account.Email = strings.TrimSpace(cfg.Account.Email)
	for _, c := range cfg.Certificates {
		if c.KeyType == "" {
			c.KeyType = defaultKeyType
		}
		if c.Challenge == "" {
			c.Challenge = defaultChallenge
		}
		for i, d := range c.Domains {
			c.Domains[i] = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
		}
	}
}

// mergeSecrets puts the saved secrets back where cfg leaves them empty:
// a DNS provider's settings (by provider name) and the EAB key.
func mergeSecrets(cfg, saved *janusv1alpha1.ACMEConfig) {
	if saved == nil {
		return
	}
	if cfg.Account.EabHmacKey == "" && cfg.Account.EabKeyId != "" && saved.GetAccount().GetEabKeyId() == cfg.Account.EabKeyId {
		cfg.Account.EabHmacKey = saved.GetAccount().GetEabHmacKey()
	}
	for _, p := range cfg.DnsProviders {
		var old *janusv1alpha1.ACMEDNSProvider
		for _, sp := range saved.DnsProviders {
			if sp.Name == p.Name && sp.Type == p.Type {
				old = sp
			}
		}
		for k, v := range p.Settings {
			if v == "" && old != nil {
				p.Settings[k] = old.Settings[k]
			}
		}
	}
}

// Redacted is cfg without its secrets: the values are emptied.
func Redacted(cfg *janusv1alpha1.ACMEConfig) *janusv1alpha1.ACMEConfig {
	out := proto.Clone(cfg).(*janusv1alpha1.ACMEConfig)
	if out.Account != nil {
		out.Account.EabHmacKey = ""
	}
	for _, p := range out.DnsProviders {
		for k := range p.Settings {
			p.Settings[k] = ""
		}
	}
	return out
}

// Validate checks a (normalized, secrets merged) configuration.
func Validate(cfg *janusv1alpha1.ACMEConfig) []string {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	acc := cfg.GetAccount()
	dir := ResolveDirectory(acc.GetDirectory())
	if u, err := url.Parse(dir); err != nil || u.Scheme != "https" || u.Host == "" {
		add("account.directory %q: an https:// URL, \"letsencrypt\" or \"letsencrypt-staging\"", acc.GetDirectory())
	}
	if acc.GetDirectoryCa() != "" {
		if n := countCerts(acc.GetDirectoryCa()); n == 0 {
			add("account.directory_ca: no PEM certificate in it")
		}
	}
	if e := acc.GetEmail(); e != "" && (strings.ContainsAny(e, " \t,<>") || !strings.Contains(e, "@")) {
		add("account.email %q isn't an address", e)
	}
	if (acc.GetEabKeyId() == "") != (acc.GetEabHmacKey() == "") {
		add("account: eab_key_id and eab_hmac_key go together")
	}

	providers := map[string]*janusv1alpha1.ACMEDNSProvider{}
	for i, p := range cfg.DnsProviders {
		where := fmt.Sprintf("dns_providers[%d]", i)
		if !namePattern.MatchString(p.Name) {
			add("%s: name %q: letters, digits, '.', '-', '_'", where, p.Name)
		} else if providers[p.Name] != nil {
			add("%s: name %q is used twice", where, p.Name)
		}
		providers[p.Name] = p
		prefixes, ok := acmewire.DNSProviders[p.Type]
		if !ok {
			add("%s: unknown type %q (one of %s)", where, p.Type, strings.Join(providerTypes(), ", "))
			continue
		}
		if len(p.Settings) == 0 {
			add("%s (%s): no settings", where, p.Type)
		}
		for _, k := range sortedKeys(p.Settings) {
			own := false
			for _, pre := range prefixes {
				own = own || strings.HasPrefix(k, pre)
			}
			switch {
			case !settingPattern.MatchString(k) || !own:
				add("%s (%s): %s isn't one of its settings (they start with %s)", where, p.Type, k, strings.Join(prefixes, " or "))
			case strings.HasSuffix(k, "_FILE"):
				add("%s (%s): %s: file settings aren't supported, give the value itself", where, p.Type, k)
			case p.Settings[k] == "":
				add("%s (%s): %s has no value", where, p.Type, k)
			}
		}
		if p.PropagationWaitSeconds > 3600 {
			add("%s: propagation_wait_seconds: an hour at most", where)
		}
		for _, r := range p.Resolvers {
			if !validResolver(r) {
				add("%s: resolver %q: host or host:port", where, r)
			}
		}
	}

	if len(cfg.Certificates) > maxCertificates {
		add("over %d certificates", maxCertificates)
	}
	seen := map[string]bool{}
	for i, c := range cfg.Certificates {
		where := fmt.Sprintf("certificates[%d]", i)
		if !namePattern.MatchString(c.Name) {
			add("%s: name %q: letters, digits, '.', '-', '_' (its file is /etc/haproxy/acme/<name>.pem)", where, c.Name)
		} else if seen[c.Name] {
			add("%s: name %q is used twice", where, c.Name)
		}
		seen[c.Name] = true
		where = fmt.Sprintf("certificate %q", c.Name)
		if len(c.Domains) == 0 || len(c.Domains) > maxDomains {
			add("%s: 1 to %d domains", where, maxDomains)
		}
		dup := map[string]bool{}
		for _, d := range c.Domains {
			if dup[d] {
				add("%s: %s is listed twice", where, d)
			}
			dup[d] = true
			wildcard, err := checkDomain(d)
			if err != nil {
				add("%s: %v", where, err)
			}
			if wildcard && c.Challenge != acmewire.ChallengeDNS01 {
				add("%s: %s is a wildcard: it needs the dns-01 challenge", where, d)
			}
		}
		if !slices.Contains(acmewire.KeyTypes, c.KeyType) {
			add("%s: key_type %q (one of %s)", where, c.KeyType, strings.Join(acmewire.KeyTypes, ", "))
		}
		switch c.Challenge {
		case acmewire.ChallengeHTTP01:
			if c.DnsProvider != "" {
				add("%s: dns_provider is for the dns-01 challenge", where)
			}
		case acmewire.ChallengeDNS01:
			if providers[c.DnsProvider] == nil {
				add("%s: dns_provider %q isn't one of dns_providers", where, c.DnsProvider)
			}
		default:
			add("%s: challenge %q (http-01 or dns-01)", where, c.Challenge)
		}
		if c.Profile != "" && !profilePattern.MatchString(c.Profile) {
			add("%s: profile %q", where, c.Profile)
		}
	}
	return errs
}

// checkDomain checks a lowercase DNS name, "*." allowed in front.
func checkDomain(d string) (wildcard bool, err error) {
	name := d
	if strings.HasPrefix(name, "*.") {
		wildcard, name = true, name[2:]
	}
	if net.ParseIP(name) != nil {
		return wildcard, fmt.Errorf("%s: an IP address, not a name", d)
	}
	labels := strings.Split(name, ".")
	if len(name) > 253 || len(labels) < 2 {
		return wildcard, fmt.Errorf("%s isn't a domain name", d)
	}
	for _, l := range labels {
		if !labelPattern.MatchString(l) {
			return wildcard, fmt.Errorf("%s isn't a domain name", d)
		}
	}
	return wildcard, nil
}

func validResolver(r string) bool {
	host, port, err := net.SplitHostPort(r)
	if err != nil {
		host, port = r, ""
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return host != "" && !strings.ContainsAny(host, " /")
}

func countCerts(data string) int {
	n := 0
	rest := []byte(data)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return n
		}
		if block.Type == "CERTIFICATE" {
			if _, err := x509.ParseCertificate(block.Bytes); err == nil {
				n++
			}
		}
	}
}

func providerTypes() []string {
	types := make([]string, 0, len(acmewire.DNSProviders))
	for t := range acmewire.DNSProviders {
		types = append(types, t)
	}
	slices.Sort(types)
	return types
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
