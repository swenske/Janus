package main

import "testing"

func TestHAProxyMessage(t *testing.T) {
	for line, want := range map[string]int{
		"[NOTICE]   (1) : haproxy version is 3.4.7":                         5,
		"[WARNING]  (85) : Server app/app1 is DOWN, reason: Layer4 timeout": 4,
		"[ALERT]    (85) : config : parsing [/etc/haproxy/haproxy.cfg:12]": 1,
		`10.0.2.2:34824 [15/Oct/2026:17:11:37.211] janus-health "GET / HTTP/1.1"`: -1,
		"<134>Oct 15 17:11:37 haproxy[85]: 10.0.2.2:34824 ...":                  -1,
		"[15/Oct/2026:17:11:37.211] a raw log-format starting with a date":      -1,
	} {
		sev, ok := haproxyMessage(line)
		if ok != (want >= 0) || (ok && int(sev) != want) {
			t.Errorf("haproxyMessage(%q) = %d, %v; want %d", line, sev, ok, want)
		}
	}
}
