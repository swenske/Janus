package consul

import (
	"errors"
	"reflect"
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
		"janus:haproxy:raw",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("services:\n got %q\nwant %q", ids, want)
	}

	j := byID["janus:janus"]
	if j.Name != "janus" || j.Address != "192.0.2.5" || j.Port != 9505 || j.Check.TCP != "192.0.2.5:9505" || j.Meta["version"] != "v2026.10.29" {
		t.Errorf("janus: %+v %+v", j, j.Check)
	}
	if ne := byID["janus:node-exporter"]; ne.Address != "192.0.2.6" || ne.Port != 9100 || ne.Meta["metrics_path"] != "/metrics" || ne.Check.TCP != "192.0.2.6:9100" {
		t.Errorf("node-exporter: %+v", ne)
	}
	fe := byID["janus:haproxy:fe_https"]
	if fe.Name != "haproxy-fe-https" || fe.Address != "172.16.10.151" || fe.Port != 443 || !reflect.DeepEqual(fe.Tags, []string{"haproxy", "http", "tls"}) ||
		fe.Meta["maps"] != "/etc/haproxy/files/hosts.map" || fe.Check.TCP != "172.16.10.151:443" {
		t.Errorf("fe_https: %+v %+v", fe, fe.Check)
	}
	if raw := byID["janus:haproxy:raw"]; raw.Name != "haproxy-raw" || raw.Port != 5432 || raw.Address != "192.0.2.5" || raw.Tags[1] != "tcp" {
		t.Errorf("raw: %+v", raw)
	}
	url := func(id string) map[string]string { return byID[id].Meta }
	if m := url("janus:url:fe_https:www.example.com"); !reflect.DeepEqual(m, map[string]string{
		"url": "https://www.example.com", "frontend": "fe_https", "backend": "be_www", "source": "map:/etc/haproxy/files/hosts.map"}) {
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
	if m := url("janus:url:fe_sni:db.example.com"); m["url"] != "https://db.example.com:8443" || m["source"] != "map:/etc/haproxy/files/sni.map" {
		t.Errorf("by the server name: %v", m)
	}

	// Toggles: each part alone.
	only, _, _ := Desired(Toggles{Frontends: true}, sitesSources())
	for _, s := range only {
		if !strings.HasPrefix(s.ID, "janus:haproxy:") {
			t.Errorf("frontends only: %s", s.ID)
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
