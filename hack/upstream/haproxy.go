package main

import (
	"html"
	"regexp"
	"strings"
)

// haproxyBugs reads haproxy.org's list of the bugs known in a version
// (bugs-<version>.html: every fix merged into its branch since, rated by
// HAProxy itself). The bugs a bump fixes are those listed for the old
// version and no longer for the new one. CRITICAL ("a short-term
// reliability or security issue") and MAJOR count as security fixes for a
// load balancer; MEDIUM and MINOR as plain bugs. A fix in a part Janus's
// HAProxy is built without (pkgs/haproxy/Dockerfile) can't apply.
type haproxyBugs struct{}

func (haproxyBugs) describe() string { return "haproxy.org/bugs/bugs-<version>.html" }

// haproxyBugRe matches a row of the bugs table. Subjects aren't always
// escaped ("http_auth_bearer(<hdr>)"). Rows without a "BUG/<SEVERITY>"
// tag are backported improvements, not bugs: the page's totals leave them
// out too.
var haproxyBugRe = regexp.MustCompile(`(?m)^<tr[^>]*><td>[0-9-]+</td><td><a href="([^"]+)">(.*)</a></td></tr>$`)

var haproxyTagRe = regexp.MustCompile(`^BUG/([A-Z]+):`)

func (haproxyBugs) affecting(e *env, _ *component, v string) ([]vuln, error) {
	page, err := e.f.get(e.ctx, "https://www.haproxy.org/bugs/bugs-"+v+".html")
	if err != nil {
		return nil, err
	}
	return parseHAProxyBugs(string(page)), nil
}

var haproxySeverity = map[string]string{
	"CRITICAL": "critical",
	"MAJOR":    "high",
	"MEDIUM":   "medium",
	"MINOR":    "low",
}

func parseHAProxyBugs(page string) []vuln {
	var out []vuln
	for _, m := range haproxyBugRe.FindAllStringSubmatch(page, -1) {
		link := html.UnescapeString(m[1])
		if strings.HasPrefix(link, "//") {
			link = "https:" + link
		}
		commit := link[strings.LastIndex(link, "=")+1:]
		title := html.UnescapeString(m[2])
		t := haproxyTagRe.FindStringSubmatch(title)
		if t == nil {
			continue
		}
		sev := haproxySeverity[t[1]]
		if sev == "" {
			sev = "unknown"
		}
		out = append(out, vuln{
			ID:            "haproxy-" + commit,
			Title:         title,
			Severity:      sev,
			URL:           link,
			Bug:           sev == "medium" || sev == "low",
			NotApplicable: haproxyNotBuilt(title),
		})
	}
	return out
}

// haproxyBuiltWithout is what Janus's HAProxy is built without, by the
// subsystem prefixes HAProxy's commit subjects use for it.
var haproxyBuiltWithout = []struct {
	prefixes []string
	reason   string
}{
	{[]string{"quic", "mux_quic", "mux-quic", "h3", "qpack", "xprt_quic", "quic_"}, "Janus's HAProxy is built without QUIC/HTTP/3"},
	{[]string{"hlua", "lua"}, "Janus's HAProxy is built without Lua"},
	{[]string{"ot", "opentracing"}, "Janus's HAProxy is built without OpenTracing"},
	{[]string{"51d", "wurfl", "da", "deviceatlas"}, "Janus's HAProxy is built without device detection"},
	{[]string{"pcre"}, "Janus's HAProxy is built without PCRE"},
}

// haproxyNotBuilt says why a fix can't apply - "" when it can. The
// subsystem is the subject's word after "BUG/<SEVERITY>: ".
func haproxyNotBuilt(subject string) string {
	_, rest, ok := strings.Cut(subject, ": ")
	if !ok {
		return ""
	}
	sub, _, ok := strings.Cut(rest, ": ")
	if !ok {
		return ""
	}
	sub = strings.ToLower(sub)
	for _, f := range haproxyBuiltWithout {
		for _, p := range f.prefixes {
			if sub == p || (strings.HasSuffix(p, "_") && strings.HasPrefix(sub, p)) || strings.HasPrefix(sub, p+"/") {
				return f.reason
			}
		}
	}
	return ""
}
