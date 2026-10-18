package haproxy

import (
	"strings"
	"testing"
)

func TestRedactConfigLines(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// userlist
		{"    user admin insecure-password s3cret groups ops", "    user admin insecure-password <redacted> groups ops"},
		{`  user bob password $6$salt$hash`, `  user bob password <redacted>`},
		{`  user "x y" password 'a b' # note`, `  user "x y" password <redacted> # note`},
		{"  user haproxy", "  user haproxy"}, // global's "user": no password
		// stats auth
		{"    stats auth admin:hunter2", "    stats auth admin:<redacted>"},
		{`    stats auth "admin:two words"`, `    stats auth "admin:<redacted>"`},
		{"    stats auth nocolon", "    stats auth <redacted>"},
		{"    stats auth admin:", "    stats auth admin:"},
		{"    stats uri /stats", "    stats uri /stats"},
		// environment
		{"    setenv TOKEN abc123", "    setenv TOKEN <redacted>"},
		{"    presetenv TOKEN abc123", "    presetenv TOKEN <redacted>"},
		{`    acme-vars "token=abc,zone=x"`, "    acme-vars <redacted>"},
		// credential headers set or sent
		{`    http-request set-header Authorization "Basic dXNlcjpwYXNz" if { path_beg /api }`, `    http-request set-header Authorization <redacted> if { path_beg /api }`},
		{"    http-request add-header x-api-key k3y", "    http-request add-header x-api-key <redacted>"},
		{"    http-response set-header Set-Cookie sid=1", "    http-response set-header Set-Cookie <redacted>"},
		{`    http-request replace-header Cookie (.*) "sid=s3cret; \1"`, `    http-request replace-header Cookie (.*) <redacted>`},
		{"    http-request set-header X-Forwarded-Proto https", "    http-request set-header X-Forwarded-Proto https"},
		{`    http-check send meth GET uri /health hdr Host app hdr Authorization "Bearer t0k"`, `    http-check send meth GET uri /health hdr Host app hdr Authorization <redacted>`},
		{`    option httpchk GET /check HTTP/1.0\r\nAuthorization:\ Basic\ dXNlcjpwYXNz`, `    option httpchk GET /check HTTP/1.0\r\nAuthorization:\ <redacted>`},
		{`    option httpchk GET /check "HTTP/1.1\r\nX-Api-Key: k3y\r\nHost: a"`, `    option httpchk GET /check "HTTP/1.1\r\nX-Api-Key: <redacted>\r\nHost: a"`},
		// ACLs comparing a credential header
		{"    acl key_ok req.hdr(x-api-key) -m str k1 k2", "    acl key_ok req.hdr(x-api-key) -m str <redacted> <redacted>"},
		{"    acl key_ok hdr(X-Api-Key),lower -i -- -k3y", "    acl key_ok hdr(X-Api-Key),lower -i -- <redacted>"},
		{"    acl keys hdr(x-api-key) -f /etc/haproxy/files/keys.lst", "    acl keys hdr(x-api-key) -f /etc/haproxy/files/keys.lst"},
		{"    http-request deny unless { req.hdr(authorization) -m str \"Bearer t0k\" } || { src 10.0.0.1 }", "    http-request deny unless { req.hdr(authorization) -m str <redacted> } || { src 10.0.0.1 }"},
		{"    http-request set-var(txn.k) req.hdr(x-api-key) if { src 10.0.0.1 }", "    http-request set-var(txn.k) req.hdr(x-api-key) if { src 10.0.0.1 }"},
		{"    acl api path_beg /api", "    acl api path_beg /api"},
		// untouched
		{"# stats auth admin:in-a-comment", "# stats auth admin:in-a-comment"},
		{"", ""},
		{"frontend web", "frontend web"},
	} {
		if got := redactLine(c.in); got != c.want {
			t.Errorf("redactLine(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestRedactConfig: lines keep their place and their endings, the rest
// is left byte for byte.
func TestRedactConfig(t *testing.T) {
	cfg := "global\r\n    stats socket /run/janus/haproxy-admin.sock mode 660 level admin\n\nuserlist ops\n    user a insecure-password x\nlisten stats\n    stats auth a:b\n    bind :8404"
	want := "global\r\n    stats socket /run/janus/haproxy-admin.sock mode 660 level admin\n\nuserlist ops\n    user a insecure-password <redacted>\nlisten stats\n    stats auth a:<redacted>\n    bind :8404"
	got := string(RedactConfig([]byte(cfg)))
	if got != want {
		t.Fatalf("RedactConfig:\n got %q\nwant %q", got, want)
	}
	if strings.Count(got, "\n") != strings.Count(cfg, "\n") {
		t.Error("line count changed")
	}
	plain := "global\n    maxconn 100\n"
	if got := string(RedactConfig([]byte(plain))); got != plain {
		t.Errorf("a configuration without a secret changed: %q", got)
	}
}
