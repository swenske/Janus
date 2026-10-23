package nodeproxy

import (
	"reflect"
	"testing"
)

func TestParseTLSLog(t *testing.T) {
	// HAProxy's stdout: its own messages, httplog and httpslog lines (a
	// plain frontend's say -/-/-), a log-format with %sslv %sslc apart, a
	// request whose path looks like a version.
	log := `[NOTICE]   (1) : haproxy version is 3.4.7-8872a26
10.0.30.101:41234 [19/Oct/2026:10:12:01.123] web-tls~ app/app1 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET / HTTP/1.1" 0/0/0/0/0 -/TLSv1.3/TLS_AES_256_GCM_SHA384
10.0.30.101:41236 [19/Oct/2026:10:12:01.130] web-tls~ app/app2 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET / HTTP/2.0" 0/0/0/0/1 shop.example.org/TLSv1.3/TLS_AES_256_GCM_SHA384
10.0.30.101:41238 [19/Oct/2026:10:12:01.140] web-tls~ app/app1 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET / HTTP/1.1" 0/0/0/0/0 -/TLSv1.2/ECDHE-ECDSA-AES128-GCM-SHA256
10.0.30.101:41240 [19/Oct/2026:10:12:01.150] web app/app1 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET / HTTP/1.1" 0/0/0/0/0 -/-/-
10.0.30.101:41242 [19/Oct/2026:10:12:01.160] web app/app1 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET /docs/TLSv1.2/intro HTTP/1.1"
10.0.30.101:41244 [19/Oct/2026:10:12:01.170] api~ api/a1 200 TLSv1.2 ECDHE-ECDSA-CHACHA20-POLY1305 "GET /api/items HTTP/1.1"

`
	got := parseTLSLog(log)
	want := TLSLog{
		Lines:    7,
		Access:   6,
		TLS:      4,
		Versions: []NamedCount{{"TLSv1.2", 2}, {"TLSv1.3", 2}},
		Ciphers:  []NamedCount{{"TLS_AES_256_GCM_SHA384", 2}, {"ECDHE-ECDSA-AES128-GCM-SHA256", 1}, {"ECDHE-ECDSA-CHACHA20-POLY1305", 1}},
		Families: []NamedCount{{"IPv4", 6}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	// Nothing logged: empty lists, not null.
	if e := parseTLSLog(""); e.Lines != 0 || e.Versions == nil || e.Ciphers == nil || e.Families == nil {
		t.Errorf("empty: %+v", e)
	}
}

func TestClientFamily(t *testing.T) {
	// The client's address as HAProxy's formats put it, after a syslog
	// header or not; an IPv4 client of an IPv6 listener is IPv4's.
	log := `10.0.30.101:41234 [19/Oct/2026:10:12:01.123] web app/app1 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET / HTTP/1.1"
2001:db8::7:41236 [19/Oct/2026:10:12:01.130] web app/app1 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET / HTTP/1.1"
2001:db8::7:8080 [19/Oct/2026:10:12:01.131] web app/app1 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET / HTTP/1.1"
::ffff:10.0.30.102:41238 [19/Oct/2026:10:12:01.140] web app/app1 0/0/1/2/3 200 512 - - ---- 3/3/0/0/0 0/0 "GET / HTTP/1.1"
<134>Oct 19 10:12:01 haproxy[8]: [2001:db8::8]:41240 [19/Oct/2026:10:12:01.150] db db/pg1 1/0/12 512 -- 1/1/0/0/0 0/0
10.0.30.103 - - [19/Oct/2026:10:12:01 +0000] "GET / HTTP/1.1" 200 512 "" "" 41242 160 "web" "app" "app1" 0 0 1 2 3 ---- 3 3 0 0 0 0 0 "" ""
2001:db8::9 - - [19/Oct/2026:10:12:01 +0000] "GET / HTTP/1.1" 200 512 "" "" 41244 170 "web" "app" "app1" 0 0 1 2 3 ---- 3 3 0 0 0 0 0 "" ""
web-tls~ [19/Oct/2026:10:12:01.160] no client address first
[NOTICE]   (1) : haproxy version is 3.4.7-8872a26
`
	got := parseTLSLog(log).Families
	want := []NamedCount{{"IPv6", 4}, {"IPv4", 3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
