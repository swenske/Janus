// Package acmewire is what janusd and janus-acme (the letsencrypt
// extension's ACME client) exchange: janusd writes one Request as JSON on
// janus-acme's standard input and reads one Result from its standard
// output; janus-acme logs to its standard error. janus-acme keeps nothing
// on disk - account key, credentials and certificates all come from, and
// go back to, janusd.
package acmewire

import "time"

// Actions.
const (
	// ActionAccount finds the account of AccountKey on the ACME server, or
	// registers it.
	ActionAccount = "account"
	// ActionIssue orders a certificate for Domains.
	ActionIssue = "issue"
	// ActionRenewalInfo asks the server when Certificate should be renewed
	// (ACME Renewal Information, RFC 9773).
	ActionRenewalInfo = "renewal-info"
)

// Challenges.
const (
	ChallengeHTTP01 = "http-01"
	ChallengeDNS01  = "dns-01"
)

// Request is one thing for janus-acme to do.
type Request struct {
	Action string `json:"action"`

	// Directory is the ACME server's directory URL.
	Directory string `json:"directory"`
	// DirectoryCA, if set, is the only CA (PEM) trusted for the ACME
	// server's HTTPS certificate - a private ACME server's.
	DirectoryCA string `json:"directory_ca,omitempty"`

	// AccountKey is the account's private key (PEM).
	AccountKey  string `json:"account_key"`
	Email       string `json:"email,omitempty"`
	AcceptTerms bool   `json:"accept_terms"`
	// External account binding, for the CAs that require one.
	EABKeyID   string `json:"eab_key_id,omitempty"`
	EABHMACKey string `json:"eab_hmac_key,omitempty"`

	// ActionIssue.
	Domains   []string `json:"domains,omitempty"`
	KeyType   string   `json:"key_type,omitempty"`
	Profile   string   `json:"profile,omitempty"`
	Challenge string   `json:"challenge,omitempty"`
	// DNSProvider is a provider name (DNSProviders); DNSEnv its settings,
	// under the provider's own variable names.
	DNSProvider  string            `json:"dns_provider,omitempty"`
	DNSEnv       map[string]string `json:"dns_env,omitempty"`
	DNSResolvers []string          `json:"dns_resolvers,omitempty"`
	// DNSPropagationWait, if set, replaces the propagation check by a
	// wait.
	DNSPropagationWait time.Duration `json:"dns_propagation_wait,omitempty"`

	// Certificate (PEM) is the current certificate: the one ActionIssue
	// replaces, or the one ActionRenewalInfo asks about.
	Certificate string `json:"certificate,omitempty"`
}

// Result is what janus-acme did.
type Result struct {
	Error string `json:"error,omitempty"`

	// The account (every action resolves it first).
	AccountURI string `json:"account_uri,omitempty"`
	TermsURL   string `json:"terms_url,omitempty"`

	// ActionIssue: the certificate chain and its private key (PEM).
	Certificate string `json:"certificate,omitempty"`
	PrivateKey  string `json:"private_key,omitempty"`

	// ActionRenewalInfo: the window the server suggests renewing in.
	RenewalStart time.Time `json:"renewal_start,omitzero"`
	RenewalEnd   time.Time `json:"renewal_end,omitzero"`
	// RetryAfter is when to ask again.
	RetryAfter time.Time `json:"retry_after,omitzero"`
}

// DNSProviders are the DNS-01 providers janus-acme has, and the prefix
// of their settings' names (lego's).
var DNSProviders = map[string][]string{
	"cloudflare":   {"CLOUDFLARE_", "CF_"},
	"desec":        {"DESEC_"},
	"digitalocean": {"DO_"},
	"gandiv5":      {"GANDIV5_"},
	"hetzner":      {"HETZNER_"},
	"httpreq":      {"HTTPREQ_"},
	"infomaniak":   {"INFOMANIAK_"},
	"ionos":        {"IONOS_"},
	"ovh":          {"OVH_"},
	"pdns":         {"PDNS_"},
	"rfc2136":      {"RFC2136_", "DNSUPDATE_"},
	"route53":      {"AWS_"},
	"scaleway":     {"SCALEWAY_"},
}

// KeyTypes are the certificate key types.
var KeyTypes = []string{"ec256", "ec384", "rsa2048", "rsa3072", "rsa4096"}
