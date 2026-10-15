package logship

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/swenske/Janus/internal/tlstrust"
)

// Config is where the logs go.
type Config struct {
	Targets []Target `json:"targets,omitempty"`
	// Labels are added to every record (Elasticsearch's labels.*,
	// nothing in syslog): env=prod, site=paris...
	Labels map[string]string `json:"labels,omitempty"`
	// AccessLogsOnDisk keeps HAProxy's access logs on the LOGS partition
	// too: "always" - each target then gets them at least once, at the
	// cost of writing every request to the disk - or "never" (the
	// default): they wait in memory only, dropped when a target can't
	// keep up.
	AccessLogsOnDisk string `json:"access_logs_on_disk,omitempty"`
	// ManagedBy names the Controller that set this configuration (its
	// fleet profile); empty when set by hand.
	ManagedBy string `json:"managed_by,omitempty"`
}

// Target is one log server.
type Target struct {
	// Name identifies the target: what it has taken is kept under it.
	Name string `json:"name"`
	// Sources it takes (Sources); empty: DefaultSources.
	Sources []string `json:"sources,omitempty"`
	// Disabled keeps the target without sending to it.
	Disabled      bool                 `json:"disabled,omitempty"`
	Syslog        *SyslogTarget        `json:"syslog,omitempty"`
	Elasticsearch *ElasticsearchTarget `json:"elasticsearch,omitempty"`
	TLS           tlstrust.Trust       `json:"tls,omitzero"`
}

// SyslogTarget is a syslog server.
type SyslogTarget struct {
	// Address is host:port.
	Address string `json:"address"`
	// Transport: "udp", "tcp" or "tls" (the default port: 514, 514,
	// 6514).
	Transport string `json:"transport,omitempty"`
	// Format: "rfc5424" (the default) or "rfc3164" (BSD).
	Format string `json:"format,omitempty"`
	// Framing over TCP and TLS: "octet-counting" (RFC 6587, the default
	// with rfc5424) or "lf" (one message a line, the default with
	// rfc3164).
	Framing string `json:"framing,omitempty"`
	// Facility of the services' lines: local0 (the default) to local7,
	// daemon, user. The kernel's are kern, the audit's authpriv.
	Facility string `json:"facility,omitempty"`
}

// ElasticsearchTarget is an Elasticsearch cluster.
type ElasticsearchTarget struct {
	// URL is the cluster's, e.g. https://elastic.example:9200.
	URL string `json:"url"`
	// Namespace of the data streams, logs-janus.<source>-<namespace>
	// ("default" by default).
	Namespace string `json:"namespace,omitempty"`
	// APIKey (secret) - as Kibana shows it encoded, or "id:key" - or
	// Username and Password (secret).
	APIKey   string `json:"api_key,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Gzip compresses what's sent: a fifth of the bytes, some CPU.
	Gzip bool `json:"gzip,omitempty"`
}

// Kind is the target's kind: "syslog" or "elasticsearch".
func (t Target) Kind() string {
	if t.Elasticsearch != nil {
		return "elasticsearch"
	}
	return "syslog"
}

// Takes says whether the target takes source.
func (t Target) Takes(source string) bool {
	if len(t.Sources) == 0 {
		return slices.Contains(DefaultSources, source)
	}
	return slices.Contains(t.Sources, source)
}

var (
	targetName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	labelKey   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	namespace  = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
)

var facilities = map[string]int{
	"kern": 0, "user": 1, "daemon": 3, "auth": 4, "authpriv": 10,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19, "local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

// Validate checks c without connecting anywhere, and fills the
// defaults in.
func (c *Config) Validate() error {
	if len(c.Targets) > 8 {
		return errors.New("8 targets at most")
	}
	switch c.AccessLogsOnDisk {
	case "", "never", "always":
	default:
		return fmt.Errorf("access_logs_on_disk %q: never or always", c.AccessLogsOnDisk)
	}
	if len(c.Labels) > 16 {
		return errors.New("16 labels at most")
	}
	for k, v := range c.Labels {
		if !labelKey.MatchString(k) || len(v) > 128 {
			return fmt.Errorf("label %q: a lowercase name (a-z, 0-9, _) and a value of 128 bytes at most", k)
		}
	}
	seen := map[string]bool{}
	for i := range c.Targets {
		t := &c.Targets[i]
		if !targetName.MatchString(t.Name) {
			return fmt.Errorf("target %q: a name of lowercase letters, digits, '.', '_', '-'", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("two targets named %q", t.Name)
		}
		seen[t.Name] = true
		if err := t.validate(); err != nil {
			return fmt.Errorf("target %s: %w", t.Name, err)
		}
	}
	return nil
}

func (t *Target) validate() error {
	for _, s := range t.Sources {
		if !slices.Contains(Sources, s) && s != SourceController {
			return fmt.Errorf("unknown source %q (%s)", s, strings.Join(Sources, ", "))
		}
	}
	if (t.Syslog == nil) == (t.Elasticsearch == nil) {
		return errors.New("a syslog server or an Elasticsearch cluster - one of them")
	}
	if err := t.TLS.Validate(); err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	if s := t.Syslog; s != nil {
		s.Transport = strings.ToLower(cmpOr(s.Transport, "udp"))
		s.Format = strings.ToLower(cmpOr(s.Format, "rfc5424"))
		switch s.Transport {
		case "udp", "tcp", "tls":
		default:
			return fmt.Errorf("transport %q: udp, tcp or tls", s.Transport)
		}
		switch s.Format {
		case "rfc5424", "rfc3164":
		default:
			return fmt.Errorf("format %q: rfc5424 or rfc3164", s.Format)
		}
		if s.Framing == "" {
			s.Framing = "octet-counting"
			if s.Format == "rfc3164" {
				s.Framing = "lf"
			}
		}
		if s.Framing != "octet-counting" && s.Framing != "lf" {
			return fmt.Errorf("framing %q: octet-counting or lf", s.Framing)
		}
		s.Facility = cmpOr(s.Facility, "local0")
		if f, ok := facilities[s.Facility]; !ok || f == 0 || f == 10 {
			return fmt.Errorf("facility %q: local0 to local7, daemon, user, auth", s.Facility)
		}
		port := "514"
		if s.Transport == "tls" {
			port = "6514"
		}
		addr, err := withPort(s.Address, port)
		if err != nil {
			return err
		}
		s.Address = addr
		if s.Transport != "tls" && (t.TLS != tlstrust.Trust{}) {
			return errors.New("tls settings, but the transport isn't tls")
		}
	}
	if e := t.Elasticsearch; e != nil {
		u, err := url.Parse(e.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("url %q: https://host[:port][/path]", e.URL)
		}
		e.URL = strings.TrimRight(e.URL, "/")
		e.Namespace = cmpOr(e.Namespace, "default")
		if !namespace.MatchString(e.Namespace) {
			return fmt.Errorf("namespace %q: lowercase letters, digits and _", e.Namespace)
		}
		if e.APIKey != "" && (e.Username != "" || e.Password != "") {
			return errors.New("an API key or a username and password - one of them")
		}
		if (e.Username == "") != (e.Password == "") {
			return errors.New("a username needs its password")
		}
		if u.Scheme == "http" && (t.TLS != tlstrust.Trust{}) {
			return errors.New("tls settings for an http:// URL")
		}
	}
	return nil
}

// withPort gives host:port, def when address has no port.
func withPort(address, def string) (string, error) {
	if address == "" {
		return "", errors.New("no address")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host, port = strings.Trim(address, "[]"), def
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 || host == "" || strings.ContainsAny(host, "/ ") {
		return "", fmt.Errorf("address %q: host[:port]", address)
	}
	return net.JoinHostPort(host, port), nil
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Redacted is c without its secrets: what's shown, never what's kept.
func (c Config) Redacted() Config {
	out := c
	out.Targets = slices.Clone(c.Targets)
	for i := range out.Targets {
		t := &out.Targets[i]
		t.TLS.ClientKeyPEM = ""
		if t.Elasticsearch != nil {
			e := *t.Elasticsearch
			e.APIKey, e.Password = "", ""
			t.Elasticsearch = &e
		}
	}
	return out
}

// HasSecret says which of a target's secrets are set, by name.
func (t Target) HasSecret() map[string]bool {
	out := map[string]bool{}
	if t.TLS.ClientKeyPEM != "" {
		out["client_key"] = true
	}
	if e := t.Elasticsearch; e != nil {
		if e.APIKey != "" {
			out["api_key"] = true
		}
		if e.Password != "" {
			out["password"] = true
		}
	}
	return out
}

// MergeSecrets gives each target of c left without a secret the one
// saved for the target of the same name - a configuration read back
// (Redacted) and changed keeps its secrets.
func (c *Config) MergeSecrets(saved Config) {
	for i := range c.Targets {
		t := &c.Targets[i]
		idx := slices.IndexFunc(saved.Targets, func(s Target) bool { return s.Name == t.Name })
		if idx < 0 {
			continue
		}
		s := saved.Targets[idx]
		if t.TLS.ClientKeyPEM == "" && t.TLS.ClientCertPEM != "" && t.TLS.ClientCertPEM == s.TLS.ClientCertPEM {
			t.TLS.ClientKeyPEM = s.TLS.ClientKeyPEM
		}
		if e, se := t.Elasticsearch, s.Elasticsearch; e != nil && se != nil && e.URL == se.URL {
			if e.APIKey == "" && e.Username == "" && se.APIKey != "" {
				e.APIKey = se.APIKey
			}
			if e.Password == "" && e.Username != "" && e.Username == se.Username {
				e.Password = se.Password
			}
		}
	}
}
