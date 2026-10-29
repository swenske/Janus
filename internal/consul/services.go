package consul

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/swenske/Janus/internal/haproxyflow"
)

// The services a node registers in its agent (docs/consul.md "The
// node's services"), as the managed layer's toggles choose them - all
// with IDs starting IDPrefix, the only ones janusd ever touches:
//
//   - janus: janusd's API, a TCP check;
//   - the exporters, each where it really listens, while it runs:
//     janus-exporter, node-exporter (the extension's), haproxy-exporter
//     (each frontend that has HAProxy's own prometheus-exporter serve
//     a request) - tags prometheus and scheme=<it>, meta metrics_path and
//     scheme, for one Prometheus job to find them all;
//   - haproxy-<name>: each frontend and listen section, on the port of
//     its first bind (its address when it names one - an anycast VIP -,
//     else the node's), tags haproxy, its mode and tls, proto=<mode> and
//     scheme=<https|http> (none for TCP without ssl), a TCP check;
//   - haproxy-url: one instance per URL the configuration routes, for a
//     probe (Prometheus's blackbox exporter) to find - the hosts in the
//     maps a use_backend reads by the Host header (or the TLS server
//     name, in TCP mode), and the literal hosts of the Host ACLs a
//     use_backend's condition names; tags haproxy, url, its scheme,
//     scheme=<it> and proto=<http|tcp> (by the Host header, or by the TLS
//     server name passed through), meta url, host, scheme, proto,
//     frontend, backend, source. No check: what answers is the probe's to say. A frontend
//     only on a socket (abns@...) has its URLs at the bind of the one
//     relaying to it - meta via.

// IDPrefix starts the IDs of the services janusd registers.
const IDPrefix = "janus:"

// MaxURLs bounds the haproxy-url instances a node registers.
const MaxURLs = 1000

// Service is a service as the agent registers it.
type Service struct {
	ID      string            `json:"ID"`
	Name    string            `json:"Name"`
	Tags    []string          `json:"Tags,omitempty"`
	Address string            `json:"Address,omitempty"`
	Port    int               `json:"Port,omitempty"`
	Meta    map[string]string `json:"Meta,omitempty"`
	Check   *Check            `json:"Check,omitempty"`
}

// Check is a service's check: TCP or HTTP.
type Check struct {
	Name     string `json:"Name,omitempty"`
	TCP      string `json:"TCP,omitempty"`
	HTTP     string `json:"HTTP,omitempty"`
	Interval string `json:"Interval"`
	Timeout  string `json:"Timeout"`
}

// Endpoint is where an exporter listens: Host "" is the node's address.
type Endpoint struct {
	Host string
	Port int
}

// The exporters' services: the tag a Prometheus job finds them by.
const (
	PrometheusTag      = "prometheus"
	defaultMetricsPath = "/metrics"
)

// Sources are what the node's services are read from.
type Sources struct {
	// Address is the node's: its services', and the frontends' that
	// bind every address.
	Address string
	Version string // janusd's
	APIPort int    // janusd's, 9505
	// The exporters, nil when they don't run.
	Exporter     *Endpoint
	NodeExporter *Endpoint
	// HAProxyConfig is the configuration HAProxy runs; Map reads a map's
	// entries as HAProxy has them now (the runtime's, changes made
	// without a reload included).
	HAProxyConfig string
	Map           func(path string) (map[string]string, error)
}

// Desired is the services the node registers. truncated: there were
// more URLs than MaxURLs.
func Desired(t Toggles, src Sources) (services []Service, truncated bool, err error) {
	if src.Address == "" {
		return nil, false, fmt.Errorf("the node has no address to register its services with")
	}
	check := func(name, host string, port int) *Check {
		return &Check{Name: name, TCP: net.JoinHostPort(host, strconv.Itoa(port)), Interval: "10s", Timeout: "3s"}
	}
	if t.Janus {
		port := src.APIPort
		if port == 0 {
			port = 9505
		}
		meta := map[string]string{}
		if src.Version != "" {
			meta["version"] = src.Version
		}
		services = append(services, Service{ID: IDPrefix + "janus", Name: "janus", Tags: []string{"janus", "grpc"},
			Address: src.Address, Port: port, Meta: meta, Check: check("janus API", src.Address, port)})
	}
	if t.Exporters {
		for _, e := range []struct {
			name string
			ep   *Endpoint
		}{{"janus-exporter", src.Exporter}, {"node-exporter", src.NodeExporter}} {
			if e.ep == nil || e.ep.Port == 0 {
				continue
			}
			host := cmpOr(e.ep.Host, src.Address)
			services = append(services, Service{ID: IDPrefix + e.name, Name: e.name, Tags: []string{"janus", PrometheusTag, "scheme=http"},
				Address: host, Port: e.ep.Port, Meta: map[string]string{"metrics_path": defaultMetricsPath, "scheme": "http"},
				Check: &Check{Name: e.name, HTTP: "http://" + net.JoinHostPort(host, strconv.Itoa(e.ep.Port)) + defaultMetricsPath, Interval: "30s", Timeout: "5s"}})
		}
	}
	if !t.Frontends && !t.URLs && !t.Exporters {
		return services, false, nil
	}
	top := haproxyflow.Parse(src.HAProxyConfig)
	maps := map[string]map[string]string{}
	readMap := func(path string) (map[string]string, error) {
		if m, ok := maps[path]; ok {
			return m, nil
		}
		if src.Map == nil {
			return nil, fmt.Errorf("map %s: can't be read", path)
		}
		m, err := src.Map(path)
		if err != nil {
			return nil, fmt.Errorf("map %s: %w", path, err)
		}
		maps[path] = m
		return m, nil
	}
	// A frontend only on a socket is reached through the one relaying to
	// it (fe_tls :443 -> server abns@x -> fe_https): its URLs are at that
	// one's bind, and the relay's own for the same URL is left out.
	entries, relayTo := relays(top, readMap)
	inner := map[string]map[string]bool{} // socket-only frontend: its URLs
	if t.URLs {
		for _, f := range top.Frontends {
			r, ok := entries[f.Name]
			if !ok {
				continue
			}
			refs, err := hostsOf(f, readMap)
			if err != nil {
				return nil, false, err
			}
			inner[f.Name] = map[string]bool{}
			for _, ref := range refs {
				_, u := urlOf(ref, r.scheme(), r.port)
				inner[f.Name][u] = true
			}
		}
	}
	urls := 0
	addURLs := func(f haproxyflow.Frontend, scheme, uaddr string, uport int, entry string) error {
		refs, err := hostsOf(f, readMap)
		if err != nil {
			return err
		}
		uhost := cmpOr(uaddr, src.Address)
		// How HAProxy routes the host: by the Host header (an HTTP
		// frontend), or by the TLS server name, passed through (TCP).
		proto := cmpOr(f.Mode, "tcp")
	next:
		for _, r := range refs {
			if urls >= MaxURLs {
				truncated = true
				break
			}
			sch, u := urlOf(r, scheme, uport)
			for _, s := range relayTo[r.backend] {
				if inner[s][u] {
					continue next
				}
			}
			host, _, _ := strings.Cut(r.host, ":")
			meta := map[string]string{"url": u, "host": host, "scheme": sch, "proto": proto, "frontend": f.Name, "backend": r.backend, "source": r.source}
			if entry != "" {
				meta["via"] = entry
			}
			services = append(services, Service{ID: IDPrefix + "url:" + f.Name + ":" + r.host, Name: "haproxy-url",
				Tags: []string{"haproxy", "url", sch, "scheme=" + sch, "proto=" + proto}, Address: uhost, Port: uport, Meta: meta})
			urls++
		}
		return nil
	}
	for _, f := range top.Frontends {
		raw, addr, port, ok := firstBind(f.Binds)
		if !ok {
			// Its own service has nothing a check reaches - the entry's
			// is registered, at the same address and port.
			if r, found := entries[f.Name]; found && t.URLs {
				if err := addURLs(f, r.scheme(), r.addr, r.port, r.entry); err != nil {
					return nil, false, err
				}
			}
			continue
		}
		host := cmpOr(addr, src.Address)
		if t.Exporters {
			if path, ok := prometheusExporter(f); ok {
				scheme, tags := "http", []string{"haproxy", PrometheusTag}
				if slices.Contains(f.TLSBinds, raw) {
					scheme, tags = "https", append(tags, "tls")
				}
				tags = append(tags, "scheme="+scheme)
				services = append(services, Service{ID: IDPrefix + "haproxy-exporter:" + f.Name, Name: "haproxy-exporter", Tags: tags,
					Address: host, Port: port, Meta: map[string]string{"metrics_path": path, "scheme": scheme, "frontend": f.Name},
					Check: check("haproxy-exporter "+f.Name, host, port)})
			}
		}
		if t.Frontends {
			mode := cmpOr(f.Mode, "tcp")
			tags := []string{"haproxy", mode}
			if len(f.TLSBinds) > 0 {
				tags = append(tags, "tls")
			}
			tags = append(tags, "proto="+mode)
			switch {
			case len(f.TLSBinds) > 0:
				tags = append(tags, "scheme=https")
			case mode == "http":
				tags = append(tags, "scheme=http")
			}
			meta := map[string]string{"frontend": f.Name}
			if m := frontendMaps(f); m != "" {
				meta["maps"] = m
			}
			services = append(services, Service{ID: IDPrefix + "haproxy:" + f.Name, Name: ServiceName(f.Name), Tags: tags,
				Address: host, Port: port, Meta: meta, Check: check("haproxy "+f.Name, host, port)})
		}
		if !t.URLs {
			continue
		}
		scheme, uaddr, uport := "http", addr, port
		if _, a, p, ok := firstBind(f.TLSBinds); ok {
			scheme, uaddr, uport = "https", a, p
		}
		if err := addURLs(f, scheme, uaddr, uport, ""); err != nil {
			return nil, false, err
		}
	}
	return services, truncated, nil
}

// urlOf is the URL of a host a frontend routes, its scheme (https by a
// TLS server name too) and the port when it isn't the scheme's own.
func urlOf(r hostRef, scheme string, port int) (sch, u string) {
	if r.sni {
		scheme = "https"
	}
	u = scheme + "://" + r.host
	if !strings.Contains(r.host, ":") && port != defaultPort(scheme) {
		u += ":" + strconv.Itoa(port)
	}
	return scheme, u
}

// relay is how clients reach a frontend that listens only on a socket:
// through entry, at its bind's address ("" every address) and port,
// with TLS on the way (the entry's bind, or the socket's) or not.
type relay struct {
	entry string
	addr  string
	port  int
	tls   bool
}

func (r relay) scheme() string {
	if r.tls {
		return "https"
	}
	return "http"
}

// maxRelayHops bounds a chain of frontends behind sockets.
const maxRelayHops = 4

// relays finds, for each frontend listening only on sockets (abns@,
// unix@, the loopback), the frontend clients reach it through: one with
// a bind a check reaches whose backend - use_backend, default_backend,
// a listen's own, a dynamic one's literals, map values and default - has
// a server on one of its sockets, followed through other socket-only
// frontends a few hops; the first in the configuration's order. relayTo
// is, for each backend sending to such a frontend, the frontends it
// reaches.
func relays(top haproxyflow.Topology, readMap func(string) (map[string]string, error)) (entries map[string]relay, relayTo map[string][]string) {
	type hop struct {
		front string
		tls   bool // the socket's bind has ssl
	}
	byName := map[string]haproxyflow.Frontend{}
	sockets := map[string]hop{}
	for _, f := range top.Frontends {
		byName[f.Name] = f
		if _, _, _, ok := firstBind(f.Binds); ok {
			continue
		}
		for _, b := range f.Binds {
			if k := socketKey(b); k != "" {
				sockets[k] = hop{f.Name, slices.Contains(f.TLSBinds, b)}
			}
		}
	}
	if len(sockets) == 0 {
		return nil, nil
	}
	to := map[string][]hop{}
	for be, servers := range top.Servers {
		for _, s := range servers {
			if h, ok := sockets[socketKey(s)]; ok && !slices.Contains(to[be], h) {
				to[be] = append(to[be], h)
			}
		}
	}
	from := map[string][]hop{} // socket-only frontend: who sends to it
	for _, f := range top.Frontends {
		for _, be := range backendsOf(f, readMap) {
			for _, h := range to[be] {
				from[h.front] = append(from[h.front], hop{f.Name, h.tls})
			}
		}
	}
	var resolve func(name string, seen map[string]bool) (relay, bool)
	resolve = func(name string, seen map[string]bool) (relay, bool) {
		if seen[name] || len(seen) >= maxRelayHops {
			return relay{}, false
		}
		seen[name] = true
		defer delete(seen, name)
		for _, h := range from[name] {
			f := byName[h.front]
			if _, a, p, ok := firstBind(f.TLSBinds); ok {
				return relay{entry: f.Name, addr: a, port: p, tls: true}, true
			}
			if _, a, p, ok := firstBind(f.Binds); ok {
				return relay{entry: f.Name, addr: a, port: p, tls: h.tls}, true
			}
			if r, ok := resolve(h.front, seen); ok {
				r.tls = r.tls || h.tls
				return r, true
			}
		}
		return relay{}, false
	}
	entries = map[string]relay{}
	for name := range from {
		if r, ok := resolve(name, map[string]bool{}); ok {
			entries[name] = r
		}
	}
	relayTo = map[string][]string{}
	for be, hs := range to {
		for _, h := range hs {
			if _, ok := entries[h.front]; ok {
				relayTo[be] = append(relayTo[be], h.front)
			}
		}
	}
	return entries, relayTo
}

// backendsOf are the backends f may send to. A map it can't read adds
// none: hostsOf says so, when its keys are hosts.
func backendsOf(f haproxyflow.Frontend, readMap func(string) (map[string]string, error)) []string {
	var out []string
	for _, r := range f.Rules {
		switch r.Type {
		case "use", "default", "self":
			out = append(out, r.Backend)
		case "dynamic":
			for _, l := range r.Literals {
				out = append(out, r.Prefix+l+r.Suffix)
			}
			for _, mc := range r.MapCalls {
				if mc.Default != "" {
					out = append(out, r.Prefix+mc.Default+r.Suffix)
				}
				m, err := readMap(mc.Path)
				if err != nil {
					continue
				}
				for _, v := range m {
					out = append(out, r.Prefix+v+r.Suffix)
				}
			}
		}
	}
	return out
}

// socketKey is a bind's or a server's address when it is a socket on
// the node itself - abns@name, unix@path (a bare path too), a loopback
// port - written the same way for both; "" otherwise.
func socketKey(a string) string {
	a, _, _ = strings.Cut(a, ",")
	if fam, rest, found := strings.Cut(a, "@"); found {
		switch fam {
		case "abns", "abnsz":
			return a
		case "unix":
			return "unix@" + rest
		case "ipv4", "ipv6", "tcp", "tcp4", "tcp6":
			a = rest
		default:
			return ""
		}
	}
	if strings.HasPrefix(a, "/") {
		return "unix@" + a
	}
	i := strings.LastIndex(a, ":")
	if i < 0 {
		return ""
	}
	host := strings.Trim(a[:i], "[]")
	p, err := strconv.Atoi(a[i+1:])
	if err != nil || p <= 0 || p > 65535 {
		return ""
	}
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
		return "lo:" + strconv.Itoa(p)
	}
	return ""
}

func defaultPort(scheme string) int {
	if scheme == "https" {
		return 443
	}
	return 80
}

var notLabel = regexp.MustCompile(`[^a-z0-9]+`)

// ServiceName is a frontend's service name: haproxy-<its name>, lower
// case, what isn't a letter or a digit a dash - a DNS label.
func ServiceName(frontend string) string {
	n := strings.Trim(notLabel.ReplaceAllString(strings.ToLower(frontend), "-"), "-")
	if n == "" {
		n = "frontend"
	}
	n = "haproxy-" + n
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// frontendMaps are the maps a frontend's rules read, comma-separated.
func frontendMaps(f haproxyflow.Frontend) string {
	seen := map[string]bool{}
	var out []string
	for _, r := range f.Rules {
		for _, mc := range r.MapCalls {
			if !seen[mc.Path] {
				seen[mc.Path] = true
				out = append(out, mc.Path)
			}
		}
	}
	s := strings.Join(out, ",")
	if len(s) > maxMetaValue {
		s = s[:maxMetaValue]
	}
	return s
}

// firstBind is the first of binds a TCP check can reach: as written,
// its address ("" for every address) and port. Sockets, file
// descriptors, QUIC and the loopback are left out.
func firstBind(binds []string) (raw, addr string, port int, ok bool) {
	for _, b := range binds {
		if a, p, ok := parseBind(b); ok {
			return b, a, p, true
		}
	}
	return "", "", 0, false
}

// prometheusExporter says whether f has HAProxy's own exporter serve a
// request (http-request use-service prometheus-exporter), and on which
// path: its condition's, when it names one - path /metrics, path_beg
// /metrics -, else /metrics.
func prometheusExporter(f haproxyflow.Frontend) (path string, ok bool) {
	for _, u := range f.UseServices {
		if u.Name != "prometheus-exporter" {
			continue
		}
		for _, line := range condLines(f.ACLs, u.Cond) {
			if p := aclPath(line); p != "" {
				return p, true
			}
		}
		return defaultMetricsPath, true
	}
	return "", false
}

// aclPath is the path an ACL's line matches exactly or as a prefix,
// "" if none.
func aclPath(line []string) string {
	if len(line) == 0 {
		return ""
	}
	fetch, method := fetchOf(line[0]), ""
	switch fetch {
	case "path":
	case "path_beg":
		method = "beg"
	default:
		return ""
	}
	values, m := aclValues(line)
	if m != "" {
		method = m
	}
	if method != "" && method != "str" && method != "beg" {
		return ""
	}
	for _, v := range values {
		if strings.HasPrefix(v, "/") && !strings.ContainsAny(v, " *?") {
			return v
		}
	}
	return ""
}

func parseBind(b string) (addr string, port int, ok bool) {
	b, _, _ = strings.Cut(b, ",") // "a:80,b:81": the first
	if fam, rest, found := strings.Cut(b, "@"); found {
		switch fam {
		case "ipv4", "ipv6", "tcp", "tcp4", "tcp6":
			b = rest
		default: // unix@, abns@, fd@, sockpair@, quic4@, udp@...
			return "", 0, false
		}
	}
	if strings.HasPrefix(b, "/") {
		return "", 0, false
	}
	i := strings.LastIndex(b, ":")
	if i < 0 {
		return "", 0, false
	}
	host, ports := strings.Trim(b[:i], "[]"), b[i+1:]
	ports, _, _ = strings.Cut(ports, "-") // a range: its first
	p, err := strconv.Atoi(ports)
	if err != nil || p <= 0 || p > 65535 {
		return "", 0, false
	}
	switch host {
	case "", "*", "0.0.0.0", "::":
		return "", p, true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "", 0, false
	}
	if host == "localhost" {
		return "", 0, false
	}
	return host, p, true
}

// hostRef is a host a frontend routes: the backend it goes to, where
// it was found ("map:<path>" or "acl"), and whether by the TLS server
// name.
type hostRef struct {
	host, backend, source string
	sni                   bool
}

// hostsOf are the hosts f routes, in its rules' order, each once.
func hostsOf(f haproxyflow.Frontend, readMap func(string) (map[string]string, error)) ([]hostRef, error) {
	mode := cmpOr(f.Mode, "tcp")
	if mode != "http" && mode != "tcp" {
		return nil, nil
	}
	var out []hostRef
	seen := map[string]bool{}
	add := func(r hostRef) {
		if mode == "tcp" && !r.sni {
			return // no Host header in TCP mode
		}
		if !seen[r.host] {
			seen[r.host] = true
			out = append(out, r)
		}
	}
	for _, r := range f.Rules {
		switch r.Type {
		case "dynamic":
			for _, mc := range r.MapCalls {
				kind := mapInput(r.Exprs, mc.Path)
				if kind == "" || !hostMatching(mc.Converter) {
					continue
				}
				entries, err := readMap(mc.Path)
				if err != nil {
					return nil, err
				}
				keys := make([]string, 0, len(entries))
				for k := range entries {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					h := k
					if kind == "base" {
						h, _, _ = strings.Cut(k, "/")
					}
					if h = hostname(h); h != "" {
						add(hostRef{host: h, backend: r.Prefix + entries[k] + r.Suffix, source: "map:" + mc.Path, sni: kind == "sni"})
					}
				}
			}
		case "use":
			for _, h := range condHosts(f.ACLs, r.Cond) {
				add(hostRef{host: h.host, backend: r.Backend, source: "acl", sni: h.sni})
			}
		}
	}
	return out, nil
}

var (
	hostFetch  = regexp.MustCompile(`^(?:req\.)?f?hdr\(\s*host\s*(?:,[^)]*)?\)$`)
	sniFetch   = regexp.MustCompile(`^(?:req\.ssl_sni|req_ssl_sni|ssl_fc_sni)$`)
	domFetch   = regexp.MustCompile(`^hdr_dom\(\s*host\s*(?:,[^)]*)?\)$`)
	readsVar   = regexp.MustCompile(`^var\(`)
	hostLabel  = `[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?`
	fqdn       = regexp.MustCompile(`^` + hostLabel + `(?:\.` + hostLabel + `)+$`)
	allNumeric = regexp.MustCompile(`^[0-9.]+$`)
)

// fetchOf is an expression's fetch: what comes before its first
// converter, lower case.
func fetchOf(expr string) string {
	depth := 0
	for i, c := range expr {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				return strings.ToLower(strings.TrimSpace(expr[:i]))
			}
		}
	}
	return strings.ToLower(strings.TrimSpace(expr))
}

// fetchKind says what a fetch reads: "host" (the Host header), "base"
// (host and path), "sni" (the TLS server name), "" otherwise.
func fetchKind(fetch string) string {
	switch {
	case hostFetch.MatchString(fetch):
		return "host"
	case fetch == "base":
		return "base"
	case sniFetch.MatchString(fetch):
		return "sni"
	}
	return ""
}

// mapInput is what the expression calling the map at path reads (as
// fetchKind) - through a variable, what the rule's other expressions
// read.
func mapInput(exprs []string, path string) string {
	for _, e := range exprs {
		if !strings.Contains(e, path) {
			continue
		}
		f := fetchOf(e)
		if !readsVar.MatchString(f) {
			return fetchKind(f)
		}
		for _, other := range exprs {
			if other != e {
				if k := fetchKind(fetchOf(other)); k != "" {
					return k
				}
			}
		}
	}
	return ""
}

// hostMatching: a map converter whose keys are hosts as written - not
// regular expressions, substrings or suffixes.
func hostMatching(converter string) bool {
	parts := strings.Split(converter, "_")
	base := parts[0]
	if len(parts) > 1 {
		base += "_" + parts[1]
	}
	switch base {
	case "map", "map_str", "map_dom", "map_beg":
		return true
	}
	// map_int, map_ip...: "map" followed by a type isn't keyed by text.
	return false
}

// hostname is h as a URL's host - a full domain name, optionally with a
// port, lower case - or "" for anything else: IP addresses, wildcards,
// patterns.
func hostname(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	name, port, hasPort := strings.Cut(h, ":")
	if hasPort {
		if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
			return ""
		}
	}
	if !fqdn.MatchString(name) || allNumeric.MatchString(name) {
		return ""
	}
	return h
}

// aclValues are an ACL line's values, after its fetch and flags, and
// its -m method ("" without).
func aclValues(line []string) (values []string, method string) {
	flags := true
	for i := 1; i < len(line); i++ {
		w := line[i]
		if flags && strings.HasPrefix(w, "-") {
			switch w {
			case "--":
				flags = false
			case "-m":
				if i+1 < len(line) {
					method = line[i+1]
					i++
				}
			case "-f", "-u":
				i++ // a file, an ID: no value of its own
			}
			continue
		}
		flags = false
		values = append(values, w)
	}
	return values, method
}

type condHost struct {
	host string
	sni  bool
}

// condHosts are the hosts a use_backend condition requires: the values
// of the Host ACLs it names (not negated) or holds between braces -
// matched exactly or as a domain, never a pattern. "unless" requires
// none.
func condHosts(acls map[string][][]string, cond string) []condHost {
	var out []condHost
	for _, line := range condLines(acls, cond) {
		out = append(out, aclHosts(line)...)
	}
	return out
}

// condLines are the ACL lines an "if" condition requires: those of the
// named ACLs it doesn't negate, and those it holds between braces.
// "unless" requires none.
func condLines(acls map[string][][]string, cond string) [][]string {
	words := strings.Fields(cond)
	if len(words) == 0 || words[0] != "if" {
		return nil
	}
	var out [][]string
	for i := 1; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "{":
			end := i + 1
			for end < len(words) && words[end] != "}" {
				end++
			}
			out = append(out, words[i+1:end])
			i = end
		case w == "||" || w == "or" || strings.HasPrefix(w, "!"):
		default:
			out = append(out, acls[w]...)
		}
	}
	return out
}

// aclHosts are the hosts an ACL's line matches: fetch, flags, values.
func aclHosts(line []string) []condHost {
	if len(line) == 0 {
		return nil
	}
	fetch := fetchOf(line[0])
	kind := fetchKind(fetch)
	method := ""
	if domFetch.MatchString(fetch) {
		kind, method = "host", "dom"
	}
	if kind != "host" && kind != "sni" {
		return nil
	}
	values, m := aclValues(line)
	if m != "" {
		method = m
	}
	if method != "" && method != "str" && method != "dom" {
		return nil
	}
	var out []condHost
	for _, v := range values {
		if h := hostname(v); h != "" {
			out = append(out, condHost{host: h, sni: kind == "sni"})
		}
	}
	return out
}
