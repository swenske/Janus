package acme

import (
	"net/url"
	"sort"
)

// DNSProviderAPIs are the hosts each DNS provider's API answers on (port
// 443), from lego's clients: what the Controller's relay lets a node
// reach for DNS-01 (docs/guide/letsencrypt-controller.md). nil: the API
// is the one the provider's settings name (httpreq, pdns) - an admin
// allows it on the relay -, or no API at all (rfc2136 updates the zone's
// name server over DNS).
var DNSProviderAPIs = map[string][]string{
	"cloudflare":   {"api.cloudflare.com"},
	"desec":        {"desec.io"},
	"digitalocean": {"api.digitalocean.com"},
	"gandiv5":      {"api.gandi.net"},
	"hetzner":      {"api.hetzner.cloud", "dns.hetzner.com"},
	"httpreq":      nil,
	"infomaniak":   {"api.infomaniak.com"},
	"ionos":        {"api.hosting.ionos.com"},
	// OVH_ENDPOINT ovh-eu (the default), ovh-ca, ovh-us; the OAuth2
	// token endpoints beside them.
	"ovh":  {"eu.api.ovh.com", "ca.api.ovh.com", "api.us.ovhcloud.com", "www.ovh.com", "ca.ovh.com", "us.ovhcloud.com"},
	"pdns": nil,
	// The AWS SDK takes a proxy from the environment, never the relay's
	// client certificate: route53 can't go through the relay.
	"route53":  nil,
	"rfc2136":  nil,
	"scaleway": {"api.scaleway.com"},
}

// NotThroughRelay are the DNS providers whose API the relay can never
// carry, and why.
var NotThroughRelay = map[string]string{
	"route53": "the AWS SDK makes its own connections, without the node's certificate the relay asks for",
	"rfc2136": "it updates the zone's name server over DNS (port 53), not HTTPS",
}

// RelayDestinations are where the Controller's relay lets every node go
// on port 443, by what they're for: Let's Encrypt's directories, then
// each DNS provider's API.
func RelayDestinations() map[string][]string {
	out := map[string][]string{}
	for _, d := range []string{LetsEncrypt, LetsEncryptStaging} {
		if u, err := url.Parse(d); err == nil {
			out["Let's Encrypt"] = append(out["Let's Encrypt"], u.Hostname())
		}
	}
	for p, hosts := range DNSProviderAPIs {
		if len(hosts) > 0 {
			out[p] = append([]string(nil), hosts...)
		}
	}
	return out
}

// RelayHosts are RelayDestinations' hosts, sorted.
func RelayHosts() []string {
	var out []string
	for _, hosts := range RelayDestinations() {
		out = append(out, hosts...)
	}
	sort.Strings(out)
	return out
}
