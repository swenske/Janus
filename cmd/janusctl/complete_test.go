package main

import (
	"bytes"
	"strings"
	"testing"
)

// fakeSource answers like a configuration with two contexts and three
// nodes, and a node with two services - noting what was asked online.
type fakeSource struct {
	asked *[]string
}

func (fakeSource) contexts() []candidate {
	return []candidate{{"lab", "controller.lab:443"}, {"prod", "controller.prod:443 (current)"}}
}

func (fakeSource) nodes(globals) []candidate {
	return []candidate{{"lb1", ""}, {"lb2", ""}, {"lb3", ""}}
}

func (s fakeSource) fleetNodes(g globals) []candidate { return s.nodes(g) }

func (fakeSource) issuers(globals) []candidate { return []candidate{{"laptop", ""}, {"ci", ""}} }

func (s fakeSource) online(g globals, kind argKind, prior []string, cur string) ([]candidate, []string) {
	*s.asked = append(*s.asked, strings.Join(append([]string{g.context, g.nodes, kindName(kind)}, prior...), "|")+"|"+cur)
	switch kind {
	case argService:
		return []candidate{{"janusd", ""}, {"haproxy", ""}}, nil
	case argRemote:
		return []candidate{{"/etc/haproxy/", "directory"}}, []string{dirNoSpace}
	}
	return []candidate{{"x", ""}}, nil
}

func kindName(k argKind) string {
	return map[argKind]string{argService: "service", argMapKey: "mapkey", argSet: "set", argRemote: "remote", argMap: "map", argInterface: "iface"}[k]
}

func values(cands []candidate) string {
	var out []string
	for _, c := range cands {
		out = append(out, c.value)
	}
	return strings.Join(out, " ")
}

func TestComplete(t *testing.T) {
	var asked []string
	src := fakeSource{&asked}
	for _, tc := range []struct {
		line, want, dirs string
	}{
		{"", "login context nodes version tui system haproxy network access pki fleet lifecycle image completion help", ""},
		{"system lo", "info hostname services service logs events dmesg stats systemstat ps netdev netstat mounts du ls cat cp pcap metrics node-exporter sysctl reboot shutdown restart reset", ""},
		{"system logs ", "janusd haproxy", ""},
		{"system logs -", "-f -n", ""},
		{"system logs -n ", "", ""},
		{"system logs janusd ", "", ""},
		{"-context ", "lab prod", ""},
		{"-n lb1,", "lb1,lb2 lb1,lb3", ""},
		{"-n=", "-n=lb1 -n=lb2 -n=lb3", ""},
		{"-", "-context -n -all -endpoint -ca -cert -key -as-user -as-roles", ""},
		{"image seed-network -config ", "", ":files"},
		{"access rotate-ca ", "", ":dirs"},
		{"fleet issuer revoke ", "laptop ci", ""},
		{"fleet issuer revoke -kit ", "", ":files"},
		{"pki generate-client-config -role=os:", "-role=os:admin -role=os:operator -role=os:reader", ""},
		{"pki generate-client-config -role ", "os:admin os:operator os:reader", ""},
		{"completion ", "bash zsh fish", ""},
		{"lifecycle upgrade -sha256 ", "", ""},
		{"nosuch ", "", ""},
		{"haproxy acme ", "status get check apply renew", ""},
		{"network firewall set-add inet ", "x", ""},
		{"network firewall set-add inet filter blocked 192.0.2.1 ", "", ""},
		{"context ", "list use delete", ""},
		{"context use ", "lab prod", ""},
	} {
		words := strings.Split(tc.line, " ")
		dirs, cands := complete(words, src)
		if got := values(cands); got != tc.want || strings.Join(dirs, " ") != tc.dirs {
			t.Errorf("complete(%q) = %q %v, want %q %q", tc.line, got, dirs, tc.want, tc.dirs)
		}
	}

	// Online: with the context and nodes given, what came before.
	asked = nil
	// context|nodes|kind|arguments before|word under the cursor
	cases := map[string]string{
		"-context lab -n lb2 system service restart ": "lab|lb2|service|restart|",
		"haproxy map-set /etc/haproxy/hosts.map ":     "||mapkey|/etc/haproxy/hosts.map|",
		"network firewall set-add inet filter ":       "||set|inet|filter|",
		"system cat /etc/ha":                          "||remote|/etc/ha",
		"system pcap -i ":                             "||iface|",
	}
	for line, want := range cases {
		asked = nil
		dirs, _ := complete(strings.Split(line, " "), src)
		if len(asked) != 1 || asked[0] != want {
			t.Errorf("complete(%q) asked %q, want %q", line, asked, want)
		}
		if strings.HasPrefix(line, "system cat") && strings.Join(dirs, " ") != dirNoSpace {
			t.Errorf("a path on the node: directives %v", dirs)
		}
	}
}

func TestWriteCompletion(t *testing.T) {
	cands := []candidate{{"os:admin", "everything"}, {"os:reader", "reads only"}, {"other", ""}}
	var b bytes.Buffer
	writeCompletion(&b, "zsh", []string{dirNoSpace}, cands, "os:")
	if got := b.String(); got != ":nospace\nos\\:admin:everything\nos\\:reader:reads only\n" {
		t.Errorf("zsh: %q", got)
	}
	b.Reset()
	writeCompletion(&b, "fish", nil, cands, "o")
	if got := b.String(); got != "os:admin\teverything\nos:reader\treads only\nother\t\n" {
		t.Errorf("fish: %q", got)
	}
	b.Reset()
	writeCompletion(&b, "bash", nil, cands, "ot")
	if got := b.String(); got != "other\n" {
		t.Errorf("bash: %q", got)
	}
}
