// janus-acme is the letsencrypt extension's ACME client (docs/
// letsencrypt.md). janusd runs it for each thing to do - find or register
// the account, order a certificate, ask when one should be renewed -
// writing an acmewire.Request on its standard input and reading an
// acmewire.Result from its standard output; it logs to standard error.
// It keeps nothing: the account key, the DNS provider's credentials and
// the certificates come from janusd and go back to it.
//
// HTTP-01 challenges are answered by HAProxy itself, statelessly: the
// key authorization of a token is "<token>.<account thumbprint>", which a
// frontend's http-request return rule computes from the request path and
// the thumbprint janusd hands HAProxy (JANUS_ACME_THUMBPRINT). There's
// nothing to put in place for them, so janus-acme only asks the server to
// validate.
package main

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/acme"
	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	legolog "github.com/go-acme/lego/v4/log"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/providers/dns/desec"
	"github.com/go-acme/lego/v4/providers/dns/digitalocean"
	"github.com/go-acme/lego/v4/providers/dns/gandiv5"
	"github.com/go-acme/lego/v4/providers/dns/hetzner"
	"github.com/go-acme/lego/v4/providers/dns/httpreq"
	"github.com/go-acme/lego/v4/providers/dns/infomaniak"
	"github.com/go-acme/lego/v4/providers/dns/ionos"
	"github.com/go-acme/lego/v4/providers/dns/ovh"
	"github.com/go-acme/lego/v4/providers/dns/pdns"
	"github.com/go-acme/lego/v4/providers/dns/rfc2136"
	"github.com/go-acme/lego/v4/providers/dns/route53"
	"github.com/go-acme/lego/v4/providers/dns/scaleway"
	"github.com/go-acme/lego/v4/registration"

	"github.com/swenske/Janus/internal/acme/acmewire"
)

var version = "dev"

// dnsProviders builds a provider from its settings, which are in the
// environment by then.
var dnsProviders = map[string]func() (challenge.Provider, error){
	"cloudflare":   func() (challenge.Provider, error) { return cloudflare.NewDNSProvider() },
	"desec":        func() (challenge.Provider, error) { return desec.NewDNSProvider() },
	"digitalocean": func() (challenge.Provider, error) { return digitalocean.NewDNSProvider() },
	"gandiv5":      func() (challenge.Provider, error) { return gandiv5.NewDNSProvider() },
	"hetzner":      func() (challenge.Provider, error) { return hetzner.NewDNSProvider() },
	"httpreq":      func() (challenge.Provider, error) { return httpreq.NewDNSProvider() },
	"infomaniak":   func() (challenge.Provider, error) { return infomaniak.NewDNSProvider() },
	"ionos":        func() (challenge.Provider, error) { return ionos.NewDNSProvider() },
	"ovh":          func() (challenge.Provider, error) { return ovh.NewDNSProvider() },
	"pdns":         func() (challenge.Provider, error) { return pdns.NewDNSProvider() },
	"rfc2136":      func() (challenge.Provider, error) { return rfc2136.NewDNSProvider() },
	"route53":      func() (challenge.Provider, error) { return route53.NewDNSProvider() },
	"scaleway":     func() (challenge.Provider, error) { return scaleway.NewDNSProvider() },
}

var keyTypes = map[string]certcrypto.KeyType{
	"ec256": certcrypto.EC256, "ec384": certcrypto.EC384,
	"rsa2048": certcrypto.RSA2048, "rsa3072": certcrypto.RSA3072, "rsa4096": certcrypto.RSA4096,
}

func main() {
	logger := stdlog.New(os.Stderr, "", stdlog.LstdFlags)
	legolog.Logger = logger
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	var req acmewire.Request
	dec := json.NewDecoder(os.Stdin)
	dec.DisallowUnknownFields()
	res := &acmewire.Result{}
	if err := dec.Decode(&req); err != nil {
		res.Error = "read the request: " + err.Error()
	} else if err := run(&req, res); err != nil {
		res.Error = err.Error()
	}
	if res.Error != "" {
		logger.Printf("error: %s", res.Error)
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		os.Exit(1)
	}
}

type user struct {
	email string
	key   crypto.PrivateKey
	reg   *registration.Resource
}

func (u *user) GetEmail() string                        { return u.email }
func (u *user) GetRegistration() *registration.Resource { return u.reg }
func (u *user) GetPrivateKey() crypto.PrivateKey        { return u.key }

func run(req *acmewire.Request, res *acmewire.Result) error {
	key, err := certcrypto.ParsePEMPrivateKey([]byte(req.AccountKey))
	if err != nil {
		return fmt.Errorf("account key: %w", err)
	}
	u := &user{email: req.Email, key: key}
	cfg := lego.NewConfig(u)
	cfg.CADirURL = req.Directory
	cfg.UserAgent = "janus-acme/" + version
	if cfg.HTTPClient, err = httpClient(req.DirectoryCA); err != nil {
		return err
	}
	if req.Action == acmewire.ActionIssue {
		kt, ok := keyTypes[req.KeyType]
		if !ok {
			return fmt.Errorf("unknown key type %q", req.KeyType)
		}
		cfg.Certificate.KeyType = kt
	}
	client, err := lego.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("ACME directory %s: %w", req.Directory, err)
	}
	res.TermsURL = client.GetToSURL()
	if u.reg, err = account(client, req); err != nil {
		return err
	}
	res.AccountURI = u.reg.URI

	switch req.Action {
	case acmewire.ActionAccount:
		return nil
	case acmewire.ActionRenewalInfo:
		leaf, err := leafOf(req.Certificate)
		if err != nil {
			return err
		}
		info, err := client.Certificate.GetRenewalInfo(certificate.RenewalInfoRequest{Cert: leaf})
		if err != nil {
			return fmt.Errorf("renewal information: %w", err)
		}
		res.RenewalStart = info.SuggestedWindow.Start
		res.RenewalEnd = info.SuggestedWindow.End
		if info.RetryAfter > 0 {
			res.RetryAfter = time.Now().Add(info.RetryAfter)
		}
		return nil
	case acmewire.ActionIssue:
		return issue(client, req, res)
	}
	return fmt.Errorf("unknown action %q", req.Action)
}

// account finds the key's account, or registers it.
func account(client *lego.Client, req *acmewire.Request) (*registration.Resource, error) {
	reg, err := client.Registration.ResolveAccountByKey()
	if err == nil {
		return reg, nil
	}
	var problem *acme.ProblemDetails
	if !errors.As(err, &problem) || !strings.HasSuffix(problem.Type, ":accountDoesNotExist") {
		return nil, fmt.Errorf("find the account: %w", err)
	}
	if !req.AcceptTerms {
		return nil, fmt.Errorf("the account isn't registered yet, and registering it means accepting the CA's terms of service (%s): set accept_terms", client.GetToSURL())
	}
	if req.EABKeyID != "" {
		reg, err = client.Registration.RegisterWithExternalAccountBinding(registration.RegisterEABOptions{
			TermsOfServiceAgreed: true, Kid: req.EABKeyID, HmacEncoded: req.EABHMACKey})
	} else {
		if client.GetExternalAccountRequired() {
			return nil, errors.New("this CA requires an external account binding (eab_key_id, eab_hmac_key)")
		}
		reg, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	}
	if err != nil {
		return nil, fmt.Errorf("register the account: %w", err)
	}
	return reg, nil
}

func issue(client *lego.Client, req *acmewire.Request, res *acmewire.Result) error {
	if len(req.Domains) == 0 {
		return errors.New("no domains")
	}
	switch req.Challenge {
	case acmewire.ChallengeHTTP01:
		if err := client.Challenge.SetHTTP01Provider(stateless{}); err != nil {
			return err
		}
	case acmewire.ChallengeDNS01:
		provider, err := dnsProvider(req.DNSProvider, req.DNSEnv)
		if err != nil {
			return err
		}
		var opts []dns01.ChallengeOption
		if len(req.DNSResolvers) > 0 {
			opts = append(opts, dns01.AddRecursiveNameservers(dns01.ParseNameservers(req.DNSResolvers)))
		}
		if req.DNSPropagationWait > 0 {
			opts = append(opts, dns01.PropagationWait(req.DNSPropagationWait, true))
		}
		if err := client.Challenge.SetDNS01Provider(provider, opts...); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown challenge %q", req.Challenge)
	}

	obtain := certificate.ObtainRequest{Domains: req.Domains, Bundle: true, Profile: req.Profile}
	if req.Certificate != "" {
		if leaf, err := leafOf(req.Certificate); err == nil {
			if id, err := certificate.MakeARICertID(leaf); err == nil {
				obtain.ReplacesCertID = id
			}
		}
	}
	cert, err := client.Certificate.Obtain(obtain)
	if err != nil && obtain.ReplacesCertID != "" {
		// The CA may not know the certificate this one replaces (issued
		// elsewhere, or already replaced): the order itself doesn't need it.
		stdlog.New(os.Stderr, "", stdlog.LstdFlags).Printf("order replacing the current certificate failed (%v), ordering a new one", err)
		obtain.ReplacesCertID = ""
		cert, err = client.Certificate.Obtain(obtain)
	}
	if err != nil {
		return err
	}
	res.Certificate = string(cert.Certificate)
	res.PrivateKey = string(cert.PrivateKey)
	return nil
}

// dnsProvider sets the provider's settings in the environment - only
// under the provider's own names, and never a *_FILE name, which would
// make the provider read a file - and builds it.
func dnsProvider(name string, settings map[string]string) (challenge.Provider, error) {
	build, ok := dnsProviders[name]
	prefixes := acmewire.DNSProviders[name]
	if !ok || len(prefixes) == 0 {
		return nil, fmt.Errorf("unknown DNS provider %q", name)
	}
	for k, v := range settings {
		allowed := false
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				allowed = true
			}
		}
		if !allowed || strings.HasSuffix(k, "_FILE") {
			return nil, fmt.Errorf("DNS provider %s: setting %s isn't one of its own", name, k)
		}
		if err := os.Setenv(k, v); err != nil {
			return nil, err
		}
	}
	p, err := build()
	if err != nil {
		return nil, fmt.Errorf("DNS provider %s: %w", name, err)
	}
	return p, nil
}

// stateless is the HTTP-01 provider: HAProxy answers the challenges.
type stateless struct{}

func (stateless) Present(domain, token, keyAuth string) error { return nil }
func (stateless) CleanUp(domain, token, keyAuth string) error { return nil }

// httpClient is the client for the ACME server: the system's trust
// store, or only directoryCA when it's set.
func httpClient(directoryCA string) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if directoryCA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(directoryCA)) {
			return nil, errors.New("directory_ca: no PEM certificate in it")
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: time.Minute,
			TLSClientConfig:       tlsConfig,
		},
	}, nil
}

func leafOf(chain string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(chain))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
