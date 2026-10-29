package consul

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A configuration as the nodes in front of a fleet of sites have them:
// a hosts.map read by the Host header, Host ACLs, an anycast address, a
// TLS passthrough by server name - and what has no URL.
const sitesConfig = `
global
    stats socket /run/janus/haproxy-admin.sock mode 600 level admin
defaults
    mode http
    timeout connect 5s
frontend fe_https
    bind 172.16.10.151:443 ssl crt /etc/haproxy/files/certs/ alpn h2,http/1.1
    bind [2001:db8::151]:443 ssl crt /etc/haproxy/files/certs/
    acl host_status hdr(host) -i status.example.com
    acl host_ip hdr(host) -i 192.0.2.1
    acl host_wild hdr(host) -m reg ^.*\.example\.org$
    acl host_beg req.hdr(host) -m beg beta.
    acl path_api path_beg /api
    use_backend be_status if host_status
    use_backend be_api if { req.hdr(host),lower -m dom api.example.com } path_api
    use_backend be_never unless host_status
    use_backend be_wild if host_wild || host_beg || host_ip
    use_backend %[req.hdr(host),lower,map(/etc/haproxy/files/hosts.map,be_default)]
    default_backend be_default
frontend fe_http
    bind :80
    http-request redirect scheme https code 301
frontend fe_vars
    bind :8080
    http-request set-var(txn.host) req.hdr(host),lower
    use_backend %[var(txn.host),map_dom(/etc/haproxy/files/vars.map)]
frontend fe_sni
    mode tcp
    bind :8443
    tcp-request inspect-delay 5s
    use_backend %[req.ssl_sni,lower,map(/etc/haproxy/files/sni.map)]
frontend fe_paths
    bind :8081
    use_backend %[path,map_beg(/etc/haproxy/files/paths.map)]
listen stats
    bind 127.0.0.1:8404
    stats enable
frontend fe_stats
    bind :8405
    bind :8406 ssl crt /etc/haproxy/files/stats.pem
    acl metrics path /metrics
    http-request use-service prometheus-exporter if metrics
    http-request return status 404
frontend fe_prom_tls
    bind 192.0.2.7:9443 ssl crt /etc/haproxy/files/stats.pem
    http-request use-service prometheus-exporter if { path_beg /prom }
listen raw
    mode tcp
    bind ipv4@:5432
    server db 10.0.0.9:5432
frontend sock
    bind unix@/run/haproxy.sock
    default_backend be_default
backend be_default
backend be_www
backend be_shop
`

var sitesMaps = map[string]map[string]string{
	"/etc/haproxy/files/hosts.map": {
		"www.example.com": "be_www", "shop.example.com": "be_shop", "*.example.net": "be_x",
		"10.0.0.1": "be_ip", "localhost": "be_local", "Old.Example.com": "be_old",
	},
	"/etc/haproxy/files/vars.map":  {"app.example.com": "be_app"},
	"/etc/haproxy/files/sni.map":   {"db.example.com": "be_db"},
	"/etc/haproxy/files/paths.map": {"/api": "be_api"},
}

func sitesSources() Sources {
	return Sources{
		Address: "192.0.2.5", Version: "v2026.10.29", APIPort: 9505,
		Exporter: &Endpoint{Port: 10056}, NodeExporter: &Endpoint{Host: "192.0.2.6", Port: 9100},
		HAProxyConfig: sitesConfig,
		Map: func(path string) (map[string]string, error) {
			m, ok := sitesMaps[path]
			if !ok {
				return nil, errors.New("unknown map")
			}
			return m, nil
		},
	}
}

func TestDesiredServices(t *testing.T) {
	all := Toggles{Janus: true, Exporters: true, Frontends: true, URLs: true}
	got, truncated, err := Desired(all, sitesSources())
	if err != nil || truncated {
		t.Fatal(truncated, err)
	}
	byID := map[string]Service{}
	var ids []string
	for _, s := range got {
		byID[s.ID] = s
		ids = append(ids, s.ID)
	}
	want := []string{
		"janus:janus", "janus:janus-exporter", "janus:node-exporter",
		"janus:haproxy:fe_https",
		"janus:url:fe_https:status.example.com", "janus:url:fe_https:api.example.com",
		"janus:url:fe_https:old.example.com", "janus:url:fe_https:shop.example.com", "janus:url:fe_https:www.example.com",
		"janus:haproxy:fe_http",
		"janus:haproxy:fe_vars", "janus:url:fe_vars:app.example.com",
		"janus:haproxy:fe_sni", "janus:url:fe_sni:db.example.com",
		"janus:haproxy:fe_paths",
		"janus:haproxy-exporter:fe_stats", "janus:haproxy:fe_stats",
		"janus:haproxy-exporter:fe_prom_tls", "janus:haproxy:fe_prom_tls",
		"janus:haproxy:raw",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("services:\n got %q\nwant %q", ids, want)
	}

	j := byID["janus:janus"]
	if j.Name != "janus" || j.Address != "192.0.2.5" || j.Port != 9505 || j.Check.TCP != "192.0.2.5:9505" || j.Meta["version"] != "v2026.10.29" {
		t.Errorf("janus: %+v %+v", j, j.Check)
	}
	// The exporters: where they listen, tag prometheus, metrics_path and
	// scheme, an HTTP check on their metrics.
	if ne := byID["janus:node-exporter"]; ne.Address != "192.0.2.6" || ne.Port != 9100 || !reflect.DeepEqual(ne.Meta, map[string]string{"metrics_path": "/metrics", "scheme": "http"}) ||
		ne.Check.HTTP != "http://192.0.2.6:9100/metrics" || ne.Check.TCP != "" || !reflect.DeepEqual(ne.Tags, []string{"janus", "prometheus", "scheme=http"}) {
		t.Errorf("node-exporter: %+v %+v", ne, ne.Check)
	}
	if je := byID["janus:janus-exporter"]; je.Address != "192.0.2.5" || je.Port != 10056 || je.Check.HTTP != "http://192.0.2.5:10056/metrics" {
		t.Errorf("janus-exporter: %+v %+v", je, je.Check)
	}
	he := byID["janus:haproxy-exporter:fe_stats"]
	if he.Name != "haproxy-exporter" || he.Address != "192.0.2.5" || he.Port != 8405 || !reflect.DeepEqual(he.Tags, []string{"haproxy", "prometheus", "scheme=http"}) ||
		!reflect.DeepEqual(he.Meta, map[string]string{"metrics_path": "/metrics", "scheme": "http", "frontend": "fe_stats"}) ||
		he.Check.TCP != "" || he.Check.TTL != "1m0s" || he.Check.CheckID != "service:janus:haproxy-exporter:fe_stats" || he.Check.proxy != "fe_stats" {
		t.Errorf("haproxy-exporter: %+v %+v", he, he.Check)
	}
	if he := byID["janus:haproxy-exporter:fe_prom_tls"]; he.Address != "192.0.2.7" || he.Port != 9443 || he.Meta["scheme"] != "https" || he.Meta["metrics_path"] != "/prom" ||
		!slices.Contains(he.Tags, "tls") || !slices.Contains(he.Tags, "scheme=https") {
		t.Errorf("haproxy-exporter over TLS: %+v", he)
	}
	fe := byID["janus:haproxy:fe_https"]
	if fe.Name != "haproxy-fe-https" || fe.Address != "172.16.10.151" || fe.Port != 443 || !reflect.DeepEqual(fe.Tags, []string{"haproxy", "http", "tls", "proto=http", "scheme=https"}) ||
		fe.Meta["maps"] != "/etc/haproxy/files/hosts.map" || fe.Check.TCP != "" || fe.Check.TTL != "1m0s" || fe.Check.proxy != "fe_https" ||
		fe.Check.CheckID != "service:janus:haproxy:fe_https" || fe.Check.Interval != "" || fe.Check.Status != "" {
		t.Errorf("fe_https: %+v %+v", fe, fe.Check)
	}
	if raw := byID["janus:haproxy:raw"]; raw.Name != "haproxy-raw" || raw.Port != 5432 || raw.Address != "192.0.2.5" || !reflect.DeepEqual(raw.Tags, []string{"haproxy", "tcp", "proto=tcp"}) {
		t.Errorf("raw: %+v", raw)
	}
	url := func(id string) map[string]string { return byID[id].Meta }
	if m := url("janus:url:fe_https:www.example.com"); !reflect.DeepEqual(m, map[string]string{
		"url": "https://www.example.com", "host": "www.example.com", "scheme": "https", "proto": "http",
		"frontend": "fe_https", "backend": "be_www", "source": "map:/etc/haproxy/files/hosts.map"}) {
		t.Errorf("www: %v", m)
	}
	if m := url("janus:url:fe_https:status.example.com"); m["backend"] != "be_status" || m["source"] != "acl" {
		t.Errorf("status: %v", m)
	}
	if m := url("janus:url:fe_https:api.example.com"); m["backend"] != "be_api" || m["source"] != "acl" {
		t.Errorf("api (anonymous ACL, as a domain): %v", m)
	}
	u := byID["janus:url:fe_https:www.example.com"]
	if u.Name != "haproxy-url" || u.Check != nil || u.Address != "172.16.10.151" || u.Port != 443 {
		t.Errorf("a URL's service: %+v", u)
	}
	if m := url("janus:url:fe_vars:app.example.com"); m["url"] != "http://app.example.com:8080" || m["backend"] != "be_app" {
		t.Errorf("through a variable: %v", m)
	}
	if m := url("janus:url:fe_sni:db.example.com"); m["url"] != "https://db.example.com:8443" || m["source"] != "map:/etc/haproxy/files/sni.map" || m["proto"] != "tcp" {
		t.Errorf("by the server name: %v", m)
	}

	// Toggles: each part alone.
	only, _, _ := Desired(Toggles{Frontends: true}, sitesSources())
	for _, s := range only {
		if !strings.HasPrefix(s.ID, "janus:haproxy:") {
			t.Errorf("frontends only: %s", s.ID)
		}
	}
	// An exporter that doesn't run: absent; the exporters off: none of
	// them, HAProxy's included.
	src0 := sitesSources()
	src0.NodeExporter = nil
	got0, _, _ := Desired(all, src0)
	for _, s := range got0 {
		if s.Name == "node-exporter" {
			t.Error("a node exporter that doesn't run is registered")
		}
	}
	noExp, _, _ := Desired(Toggles{Janus: true, Frontends: true}, sitesSources())
	for _, s := range noExp {
		if slices.Contains(s.Tags, PrometheusTag) {
			t.Errorf("exporters off: %s", s.ID)
		}
	}
	none, _, err := Desired(Toggles{}, Sources{Address: "192.0.2.5"})
	if err != nil || len(none) != 0 {
		t.Errorf("nothing chosen: %v %v", none, err)
	}
	// A map that can't be read: an error, not services gone.
	src := sitesSources()
	src.Map = func(string) (map[string]string, error) { return nil, errors.New("no stats socket") }
	if _, _, err := Desired(all, src); err == nil || !strings.Contains(err.Error(), "no stats socket") {
		t.Errorf("unreadable map: %v", err)
	}
	if _, _, err := Desired(all, Sources{}); err == nil {
		t.Error("no address accepted")
	}
}

// A map's key with its port: the meta host is the name alone.
func TestDesiredHostWithPort(t *testing.T) {
	src := Sources{Address: "192.0.2.5", HAProxyConfig: "frontend f\n    mode http\n    bind :80\n    use_backend %[req.hdr(host),map(/m)]\n",
		Map: func(string) (map[string]string, error) { return map[string]string{"www.example.com:8443": "be"}, nil }}
	got, _, err := Desired(Toggles{URLs: true}, src)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	if m := got[0].Meta; m["host"] != "www.example.com" || m["url"] != "http://www.example.com:8443" || m["proto"] != "http" || m["scheme"] != "http" {
		t.Errorf("a host with its port: %v", m)
	}
}

func TestDesiredTruncates(t *testing.T) {
	m := map[string]string{}
	for i := range MaxURLs + 5 {
		m["h"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+".n"+strconv.Itoa(i)+".example.com"] = "be"
	}
	src := Sources{Address: "192.0.2.5", HAProxyConfig: "frontend f\n    mode http\n    bind :80\n    use_backend %[req.hdr(host),map(/m)]\n",
		Map: func(string) (map[string]string, error) { return m, nil }}
	got, truncated, err := Desired(Toggles{URLs: true}, src)
	if err != nil || !truncated || len(got) != MaxURLs {
		t.Fatalf("%d services, truncated %v, %v", len(got), truncated, err)
	}
}

func TestParseBind(t *testing.T) {
	for b, want := range map[string]struct {
		addr string
		port int
		ok   bool
	}{
		":80": {"", 80, true}, "*:443": {"", 443, true}, "0.0.0.0:8080": {"", 8080, true}, ":::80": {"", 80, true},
		"[::]:443": {"", 443, true}, "192.0.2.1:443": {"192.0.2.1", 443, true}, "[2001:db8::1]:443": {"2001:db8::1", 443, true},
		"ipv4@:5432": {"", 5432, true}, ":8000-8010": {"", 8000, true}, "10.0.0.1:80,10.0.0.2:80": {"10.0.0.1", 80, true},
		"127.0.0.1:8404": {}, "localhost:80": {}, "unix@/run/x.sock": {}, "/run/x.sock": {}, "fd@3": {},
		"quic4@:443": {}, "abns@x": {}, ":${PORT}": {}, ":0": {},
	} {
		addr, port, ok := parseBind(b)
		if addr != want.addr || port != want.port || ok != want.ok {
			t.Errorf("parseBind(%q) = %q %d %v", b, addr, port, ok)
		}
	}
}

func TestServiceName(t *testing.T) {
	for in, want := range map[string]string{
		"fe_https": "haproxy-fe-https", "Web.Front": "haproxy-web-front", "__": "haproxy-frontend",
		strings.Repeat("a", 80): "haproxy-" + strings.Repeat("a", 55),
	} {
		if got := ServiceName(in); got != want {
			t.Errorf("ServiceName(%q) = %q", in, got)
		}
	}
}

func TestHostname(t *testing.T) {
	for in, want := range map[string]string{
		"WWW.Example.com": "www.example.com", "example.com:8443": "example.com:8443", "a.b": "a.b",
		"localhost": "", "10.0.0.1": "", "*.example.com": "", ".example.com": "", "^www$": "", "ex ample.com": "",
		"example.com:x": "", "-a.example.com": "",
	} {
		if got := hostname(in); got != want {
			t.Errorf("hostname(%q) = %q, want %q", in, got, want)
		}
	}
}

// A TCP frontend on :443 splitting TLS by server name and passing it,
// in PROXY v2, to HTTP frontends that listen only on abstract sockets -
// their URLs are the :443's; a two-hop chain through a Unix socket; a
// frontend on the loopback behind a TLS one; two sockets sending to each
// other that nothing reaches.
const chainedConfig = `
defaults
    mode http
frontend fe_https
    bind abns@lb-https name https accept-proxy ssl crt /etc/haproxy/acme/
    http-request redirect prefix https://www.example.net code 301 if { req.hdr(host),lower,word(1,:) -m str example.net }
    http-request set-var(txn.backend) req.hdr(host),lower,word(1,:),map(/etc/haproxy/files/maps/hosts.map,be_landing)
    use_backend %[var(txn.backend)]
    default_backend be_landing
frontend fe_tunnel
    bind abns@lb-tunnel accept-proxy ssl crt /etc/haproxy/acme/
    acl tunnel_host req.hdr(host),lower,word(1,:) -m str static.example.net
    use_backend be_tunnel if tunnel_host
    default_backend be_landing
frontend fe_plain
    bind :8080
    default_backend be_to_mid
backend be_to_mid
    server mid unix@/run/haproxy/mid.sock
frontend fe_mid
    bind /run/haproxy/mid.sock
    default_backend be_to_deep
backend be_to_deep
    server deep abns@deep
frontend fe_deep
    bind abns@deep
    acl app hdr(host) -i app.example.net
    use_backend be_app if app
frontend fe_loop_a
    bind abns@loop-a
    acl a hdr(host) -i loop.example.net
    use_backend be_to_b if a
backend be_to_b
    server b abns@loop-b
frontend fe_loop_b
    bind abns@loop-b
    default_backend be_to_a
backend be_to_a
    server a abns@loop-a
frontend fe_front
    bind :8443 ssl crt /etc/haproxy/files/certs/
    default_backend be_local
backend be_local
    server local 127.0.0.1:8081
frontend fe_local
    bind 127.0.0.1:8081
    acl adm hdr(host) -i admin.example.net
    use_backend be_admin if adm
backend be_landing
backend be_tunnel
defaults tcp
    mode tcp
frontend fe_tls from tcp
    bind 0.0.0.0:443 name https-sni
    bind :::443 v6only name https-sni6
    tcp-request content accept if { req.ssl_hello_type 1 }
    use_backend be_tls_tunnel if { req.ssl_sni -i static.example.net }
    use_backend be_tls_media if { req.ssl_sni -i media.example.net }
    default_backend be_tls_https
frontend fe_edge from tcp
    bind 192.0.2.151:4443 name edge
    default_backend be_tls_https
backend be_tls_https from tcp
    server fe_https abns@lb-https send-proxy-v2 no-check
backend be_tls_tunnel from tcp
    server fe_tunnel abns@lb-tunnel send-proxy-v2 no-check
backend be_tls_media from tcp
    server media 10.0.0.20:443
`

func TestDesiredThroughSockets(t *testing.T) {
	src := Sources{Address: "192.0.2.5", HAProxyConfig: chainedConfig, Map: func(path string) (map[string]string, error) {
		if path != "/etc/haproxy/files/maps/hosts.map" {
			return nil, errors.New("unknown map")
		}
		return map[string]string{"www.example.net": "be_www", "shop.example.net": "be_shop", "*.example.org": "be_x"}, nil
	}}
	got, _, err := Desired(Toggles{Frontends: true, URLs: true}, src)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Service{}
	var ids []string
	for _, s := range got {
		byID[s.ID] = s
		ids = append(ids, s.ID)
	}
	want := []string{
		"janus:url:fe_https:shop.example.net", "janus:url:fe_https:www.example.net",
		"janus:url:fe_tunnel:static.example.net",
		"janus:haproxy:fe_plain",
		"janus:url:fe_deep:app.example.net",
		"janus:haproxy:fe_front",
		"janus:url:fe_local:admin.example.net",
		// static.example.net by its server name is fe_tunnel's: not twice.
		"janus:haproxy:fe_tls", "janus:url:fe_tls:media.example.net",
		"janus:haproxy:fe_edge",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("services:\n got %q\nwant %q", ids, want)
	}
	for id, w := range map[string]struct {
		url, via, backend string
		port              int
	}{
		"janus:url:fe_https:www.example.net":     {"https://www.example.net", "fe_tls", "be_www", 443},
		"janus:url:fe_tunnel:static.example.net": {"https://static.example.net", "fe_tls", "be_tunnel", 443},
		"janus:url:fe_deep:app.example.net":      {"http://app.example.net:8080", "fe_plain", "be_app", 8080},
		"janus:url:fe_local:admin.example.net":   {"https://admin.example.net:8443", "fe_front", "be_admin", 8443},
		"janus:url:fe_tls:media.example.net":     {"https://media.example.net", "", "be_tls_media", 443},
	} {
		s := byID[id]
		if s.Meta["url"] != w.url || s.Meta["via"] != w.via || s.Meta["backend"] != w.backend || s.Port != w.port || s.Address != "192.0.2.5" {
			t.Errorf("%s: %+v", id, s)
		}
	}
	if s := byID["janus:url:fe_https:www.example.net"]; s.Meta["source"] != "map:/etc/haproxy/files/maps/hosts.map" || s.Meta["frontend"] != "fe_https" ||
		!reflect.DeepEqual(s.Tags, []string{"haproxy", "url", "https", "scheme=https", "proto=http"}) {
		t.Errorf("through a map: %+v", s)
	}
	// The tags a Prometheus job filters on (key=value), and the same in
	// the meta: by the Host header behind the socket, by the server name
	// passed through on the TCP frontend.
	for id, want := range map[string][]string{
		"janus:url:fe_tunnel:static.example.net": {"haproxy", "url", "https", "scheme=https", "proto=http"},
		"janus:url:fe_deep:app.example.net":      {"haproxy", "url", "http", "scheme=http", "proto=http"},
		"janus:url:fe_tls:media.example.net":     {"haproxy", "url", "https", "scheme=https", "proto=tcp"},
		"janus:haproxy:fe_tls":                   {"haproxy", "tcp", "proto=tcp"},
		"janus:haproxy:fe_plain":                 {"haproxy", "http", "proto=http", "scheme=http"},
		"janus:haproxy:fe_front":                 {"haproxy", "http", "tls", "proto=http", "scheme=https"},
	} {
		if got := byID[id].Tags; !reflect.DeepEqual(got, want) {
			t.Errorf("%s's tags: %q, want %q", id, got, want)
		}
	}
	if m := byID["janus:url:fe_tls:media.example.net"].Meta; m["proto"] != "tcp" || m["scheme"] != "https" || m["host"] != "media.example.net" {
		t.Errorf("by the server name: %v", m)
	}
	if m := byID["janus:url:fe_deep:app.example.net"].Meta; m["proto"] != "http" || m["scheme"] != "http" || m["host"] != "app.example.net" {
		t.Errorf("two hops behind sockets: %v", m)
	}

	// The relay only through a map's value: found the same way.
	src.HAProxyConfig = `
frontend fe_sni
    mode tcp
    bind :443
    use_backend %[req.ssl_sni,lower,map(/etc/haproxy/files/sni.map)]
backend be_tls_api
    mode tcp
    server api abns@api send-proxy-v2
frontend fe_api
    mode http
    bind abns@api accept-proxy ssl crt /etc/haproxy/acme/
    acl api hdr(host) -i api.example.net
    use_backend be_api if api
`
	src.Map = func(path string) (map[string]string, error) {
		return map[string]string{"api.example.net": "be_tls_api"}, nil
	}
	got, _, err = Desired(Toggles{URLs: true}, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "janus:url:fe_api:api.example.net" || got[0].Meta["via"] != "fe_sni" || got[0].Meta["url"] != "https://api.example.net" {
		t.Errorf("through an SNI map: %+v", got)
	}
}

func TestSocketKey(t *testing.T) {
	for in, want := range map[string]string{
		"abns@lb-https": "abns@lb-https", "abnsz@x": "abnsz@x", "unix@/run/a.sock": "unix@/run/a.sock", "/run/a.sock": "unix@/run/a.sock",
		"127.0.0.1:8081": "lo:8081", "ipv4@127.0.0.1:08081": "lo:8081", "[::1]:80": "lo:80", "localhost:80": "lo:80",
		"10.0.0.20:443": "", ":443": "", "fd@3": "", "127.0.0.1": "", "sockpair@4": "",
	} {
		if got := socketKey(in); got != want {
			t.Errorf("socketKey(%q) = %q, want %q", in, got, want)
		}
	}
}
