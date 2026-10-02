# Let's Encrypt: the letsencrypt extension

Nodes built with the **letsencrypt** [extension](image-factory.md) obtain
their HAProxy certificates from Let's Encrypt - or any ACME CA - and
renew them by themselves. Each certificate is a file HAProxy loads,
`/etc/haproxy/acme/<name>.pem`, and a renewed one is swapped into the
running HAProxy without a reload. It's managed through the API,
`janusctl haproxy acme` and the Controller's **Apps › Let's Encrypt** page.

The extension is `janus-acme`, an ACME client built on
[lego](https://go-acme.github.io/lego/). janusd keeps everything - the
configuration, the account key, the certificates, all on the node's
STATE partition - and runs janus-acme for each exchange with the CA,
handing it only what that exchange needs. janus-acme reads no file of the
node's and keeps nothing.

## A configuration

```json
{
  "account": {
    "directory": "letsencrypt",
    "email": "ops@example.com",
    "accept_terms": true
  },
  "certificates": [
    {"name": "example.com", "domains": ["example.com", "www.example.com", "shop.example.com"]},
    {"name": "wildcard", "domains": ["*.example.com"], "challenge": "dns-01", "dns_provider": "gandi"},
    {"name": "example.fr", "domains": ["example.fr", "www.example.fr"], "challenge": "dns-01", "dns_provider": "ovh"}
  ],
  "dns_providers": [
    {"name": "gandi", "type": "gandiv5", "settings": {"GANDIV5_PERSONAL_ACCESS_TOKEN": "..."}},
    {"name": "ovh", "type": "ovh", "settings": {
      "OVH_ENDPOINT": "ovh-eu", "OVH_APPLICATION_KEY": "...", "OVH_APPLICATION_SECRET": "...", "OVH_CONSUMER_KEY": "..."}}
  ]
}
```

- **account.directory**: `letsencrypt` (the default), `letsencrypt-staging`
  (untrusted test certificates, much higher rate limits - use it while
  setting things up), or another CA's directory URL. For a private CA,
  `directory_ca` is the only CA certificate (PEM) trusted for its HTTPS;
  otherwise the node's trust store is. `eab_key_id`/`eab_hmac_key` are for
  CAs that require external account binding (ZeroSSL, Google...).
- **accept_terms**: registering an account means accepting the CA's terms
  of service. The node registers its account with the first certificate.
- **certificates**: `name` makes the file name; `domains` are the names
  it covers, the first one its subject. `key_type` is `ec256` (the
  default), `ec384`, `rsa2048`, `rsa3072` or `rsa4096`. `challenge` is
  `http-01` (the default) or `dns-01` (with `dns_provider`), which a
  wildcard needs. `profile` picks one of the CA's certificate profiles,
  if it has several (Let's Encrypt's `shortlived`, for 6-day certificates).

Change a certificate's domains, key type or profile, or the CA, and the
node obtains it again. Adding a name to a certificate is a configuration
change, nothing more.

## HTTP-01: HAProxy answers

An HTTP-01 challenge asks `http://<domain>/.well-known/acme-challenge/<token>`
for `<token>.<account thumbprint>`. HAProxy answers it itself, for every
domain and every token, with one rule - nothing to put in place per
challenge:

```
frontend http
    bind :80
    http-request return status 200 content-type text/plain lf-string "%[path,field(-1,/)].${JANUS_ACME_THUMBPRINT}" if { path_beg /.well-known/acme-challenge/ }
    http-request redirect scheme https code 301

frontend https
    bind :443 ssl crt /etc/haproxy/acme/ strict-sni
    ...
```

janusd starts HAProxy with the account's thumbprint in
`JANUS_ACME_THUMBPRINT`; `janusctl haproxy acme status` shows it and the
rule. The rule goes before any redirect, in each frontend on port 80 that
receives the challenges. HAProxy must be reachable on port 80 from the
internet for these names - otherwise use DNS-01.

## The certificate files

Every configured certificate is `/etc/haproxy/acme/<name>.pem` (chain and
key). Until the CA's certificate is there, the file holds a self-signed
stand-in for the same names, so a configuration that references it loads
from the start. The usual order is: save the letsencrypt configuration,
then the HAProxy configuration that uses the files and answers the
challenges. Applying a HAProxy configuration makes the node retry at once
the HTTP-01 certificates it couldn't obtain yet.

Reference a file (`crt /etc/haproxy/acme/example.com.pem`), or the whole
directory (`crt /etc/haproxy/acme/`), possibly after a default
certificate. A renewed certificate is swapped into the running HAProxy
through its runtime API, without a reload. A certificate obtained for the
first time after HAProxy started can't be swapped into a directory
HAProxy has already read: janusd reloads HAProxy then (seamlessly).

Removing a certificate from the configuration removes its file - refused
while haproxy.cfg doesn't load without it: take it out of the HAProxy
configuration first.

## Renewal

- A certificate is renewed when a third of its lifetime is left (30 days
  of a 90-day certificate), or earlier when the CA's ACME Renewal
  Information (RFC 9773) says so - Let's Encrypt uses it to have
  certificates renewed early, e.g. before a mass revocation. The node asks
  once a day, and renews at a random time in the window the CA gives.
- After a failure the node waits 5 minutes, then twice as long after each
  new failure, up to a day: Let's Encrypt allows 5 failed validations per
  name and hour.
- `janusctl haproxy acme renew [NAME...]` (and **Renew** in the
  Controller) obtains certificates now, due or not.

## DNS-01: DNS providers

For wildcards, or names HAProxy doesn't receive on port 80, janus-acme
creates the `_acme-challenge` TXT record through the DNS provider's API.
`settings` are lego's own variables for that provider - see lego's page
for each (go-acme.github.io/lego/dns/&lt;type&gt;):

| type | usual settings |
|---|---|
| `gandiv5` | `GANDIV5_PERSONAL_ACCESS_TOKEN` |
| `ovh` | `OVH_ENDPOINT` (`ovh-eu`), `OVH_APPLICATION_KEY`, `OVH_APPLICATION_SECRET`, `OVH_CONSUMER_KEY` |
| `cloudflare` | `CLOUDFLARE_DNS_API_TOKEN` |
| `route53` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `AWS_HOSTED_ZONE_ID` |
| `pdns` (PowerDNS) | `PDNS_API_URL`, `PDNS_API_KEY` |
| `rfc2136` (dynamic DNS updates: BIND, Knot...) | `RFC2136_NAMESERVER`, `RFC2136_TSIG_KEY`, `RFC2136_TSIG_SECRET`, `RFC2136_TSIG_ALGORITHM` |
| `hetzner` | `HETZNER_API_TOKEN` |
| `digitalocean` | `DO_AUTH_TOKEN` |
| `scaleway` | `SCALEWAY_API_TOKEN`, `SCALEWAY_PROJECT_ID` |
| `ionos` | `IONOS_API_KEY` |
| `infomaniak` | `INFOMANIAK_ACCESS_TOKEN` |
| `desec` | `DESEC_TOKEN` |
| `httpreq` (your own webhook) | `HTTPREQ_ENDPOINT`, `HTTPREQ_USERNAME`, `HTTPREQ_PASSWORD` |

Only a provider's own variables are accepted, and never a `*_FILE` one
(there's no file to read on the node).

Before asking the CA to check the record, janus-acme checks that it's
visible on the zone's authoritative servers. With split-horizon DNS -
the node's resolvers serve an internal view of the zone - give public
resolvers for that check (`"resolvers": ["1.1.1.1", "9.9.9.9"]`), or, if
the node can't query them, wait a fixed time instead
(`"propagation_wait_seconds": 60`).

## Secrets

The DNS providers' settings and the EAB key are secrets: the node keeps
them (on STATE, mode 0600) and never gives them back. `janusctl haproxy
acme get` and the Controller show them empty; an empty value in an
applied configuration keeps the saved one. To change one, give the new
value; to remove one, leave it out.

## The account key

The node creates its account key (ECDSA P-256) the first time. To keep
using an existing account - its rate limits, and the thumbprint an
existing HAProxy rule may already use - give its private key (PEM, EC or
RSA):

```sh
janusctl haproxy acme apply -account-key account.key letsencrypt.json
```

acme.sh keeps it in `<acme.sh home>/ca/acme-v02.api.letsencrypt.org/directory/account.key`
(`acme-staging-v02...` for the staging account). HAProxy is reloaded, for
its `JANUS_ACME_THUMBPRINT` to follow.

## Several nodes

Behind VRRP or anycast, the challenge may reach any of the nodes. With
HTTP-01, give them all the same account key: they then answer every
challenge alike. Each node obtains its own certificates: mind the CA's
limits (Let's Encrypt: 5 certificates for the exact same set of names per
week) - or use DNS-01.

## Applying

```sh
janusctl haproxy acme check letsencrypt.json    # checked, nothing changes
janusctl haproxy acme apply letsencrypt.json    # saved; certificates are obtained in the background
janusctl haproxy acme status                    # the account, each certificate's state, expiry, last error
janusctl haproxy acme get > letsencrypt.json    # the configuration, secrets empty
janusctl haproxy acme renew example.com         # now
```

The log is the `letsencrypt` service log (`janusctl system logs
letsencrypt`, Logs in the Controller).

## Monitoring

The [Janus exporter](metrics.md) reports `janus_acme_certificate_state`
(`pending`, `valid`, `due`, `expired`), `janus_acme_certificate_failures`
(in a row), `janus_acme_certificate_renew_timestamp_seconds`,
`janus_acme_certificate_last_success_timestamp_seconds` and
`janus_acme_account_registered`; the certificates' expiry is in
`janus_certificate_expiry_timestamp_seconds{source="haproxy"}` like every
certificate HAProxy loads. Alert on failures that last, not on one.

## Moving from acme.sh

A HAProxy that used acme.sh in stateless mode (`setenv ACCOUNT_THUMBPRINT`
and the same `http-request return` rule) moves as is:

1. Import acme.sh's account key (above): the thumbprint stays the same.
2. One certificate per acme.sh certificate, with the same names (`acme.sh
   --list`, `acme.sh --info -d <domain>`).
3. In haproxy.cfg, `${ACCOUNT_THUMBPRINT}` can stay (with its `setenv`)
   or become `${JANUS_ACME_THUMBPRINT}`; point the `crt` lines at
   `/etc/haproxy/acme/`.
4. A wildcard issued elsewhere (DNS-01 by hand) becomes a `dns-01`
   certificate with the zone's DNS provider: renewed by the node from then
   on.

## Other certificates

A certificate from another CA (an internal one, a commercial one) goes in
[HAProxy's files](haproxy-files.md) and is referenced from there.

## Not supported

The TLS-ALPN-01 challenge, revoking a certificate, OCSP (Let's Encrypt
has stopped it), and DNS providers that need a program or a file (lego's
`exec`, `manual`).
