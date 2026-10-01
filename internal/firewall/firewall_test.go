package firewall

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Captured from `nft -j list sets` (nftables 1.1.7, the extension's own
// build) after adding a range, an address and a prefix with a timeout,
// and a concatenation.
const setsJSON = `{"nftables": [{"metainfo": {"version": "1.1.7", "release_name": "Commodore Bullmoose #8", "json_schema_version": 1}}, {"set": {"family": "inet", "name": "blocklist", "table": "filter", "type": "ipv4_addr", "handle": 2, "flags": ["interval", "timeout"], "elem": [{"range": ["10.0.0.1", "10.0.0.9"]}, "192.0.2.7", {"elem": {"val": {"prefix": {"addr": "198.51.100.0", "len": 24}}, "timeout": 3600, "expires": 3599}}]}}, {"set": {"family": "inet", "name": "s2", "table": "filter", "type": ["ipv4_addr", "inet_service"], "handle": 9, "elem": [{"concat": ["192.0.2.1", 443]}]}}]}`

func TestParseSets(t *testing.T) {
	sets, err := parseSets([]byte(setsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 {
		t.Fatalf("%d sets", len(sets))
	}
	b := sets[0]
	if b.Family != "inet" || b.Table != "filter" || b.Name != "blocklist" || b.Type != "ipv4_addr" || !slices.Equal(b.Flags, []string{"interval", "timeout"}) {
		t.Fatalf("blocklist = %+v", b)
	}
	var values []string
	for _, e := range b.Elements {
		values = append(values, e.Value)
	}
	if !slices.Equal(values, []string{"10.0.0.1-10.0.0.9", "192.0.2.7", "198.51.100.0/24"}) {
		t.Fatalf("elements %q", values)
	}
	if b.Elements[2].Timeout != time.Hour || b.Elements[2].Expires != 3599*time.Second {
		t.Fatalf("timeout %+v", b.Elements[2])
	}
	if s := sets[1]; s.Type != "ipv4_addr . inet_service" || s.Elements[0].Value != "192.0.2.1 . 443" {
		t.Fatalf("s2 = %+v", s)
	}
}

func TestValidation(t *testing.T) {
	for _, v := range []string{"192.0.2.7", "198.51.100.0/24", "10.0.0.1-10.0.0.9", "2001:db8::/32", "192.0.2.1 . 443", "eth0 . 22"} {
		if err := ValidElement(v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"", " 1.2.3.4", "1.2.3.4 }", "1.2.3.4; flush ruleset", "1.2.3.4\nflush ruleset", `"x"`, "$var", "@set", "1.2.3.4 # c"} {
		if err := ValidElement(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	if err := ValidSetRef("inet", "filter", "blocklist"); err != nil {
		t.Error(err)
	}
	for _, ref := range [][3]string{{"inet6", "filter", "s"}, {"inet", "fil ter", "s"}, {"inet", "filter", "s;x"}, {"inet", "", "s"}} {
		if ValidSetRef(ref[0], ref[1], ref[2]) == nil {
			t.Errorf("%v accepted", ref)
		}
	}
}

func TestErrorLines(t *testing.T) {
	got := errorLines("In file included from /dev/stdin:2:1-63:\n/run/janus/firewall/ruleset-1.nft:3:5-9: Error: syntax error\nline two\n\n", "/run/janus/firewall/ruleset-1.nft")
	if len(got) != 2 || got[0] != "ruleset:3:5-9: Error: syntax error" {
		t.Fatalf("%q", got)
	}
}

const testRuleset = `table inet filter {
	set blocklist {
		type ipv4_addr
		flags interval, timeout
	}
	set allow {
		type ipv4_addr
	}
	chain input {
		type filter hook input priority filter; policy accept;
		ip saddr @blocklist drop
	}
}
`

// TestInNetns runs the real nft against a real kernel - only inside a
// throwaway network namespace. Run it as
//
//	go test -c -o firewall.test ./internal/firewall
//	JANUS_NFT=<path to nft> unshare -rn ./firewall.test -test.run TestInNetns -test.v
//
// (the extension's nft: tar -xf build/extensions/extension-nftables-amd64.tar usr/local/sbin/nft).
func TestInNetns(t *testing.T) {
	nft := os.Getenv("JANUS_NFT")
	if nft == "" {
		t.Skip("set JANUS_NFT to an nft binary and run under `unshare -rn`")
	}
	Binary = nft
	Dir, RunDir = t.TempDir(), t.TempDir()
	if live, err := Live(); err != nil || strings.TrimSpace(live) != "" {
		t.Fatalf("refusing to run: this network namespace already has a ruleset (%v): %s", err, live)
	}

	// Checking: errors point at the document's own lines.
	errs, err := Check("table inet t {\n\tchain c {\n\t\tbogus\n\t}\n}\n")
	if err == nil || len(errs) == 0 || !strings.HasPrefix(errs[0], "ruleset:3:") {
		t.Fatalf("Check of a bad ruleset = %q %v", errs, err)
	}
	if errs, err := Check(testRuleset); err != nil {
		t.Fatalf("Check = %q %v", errs, err)
	}
	if live, _ := Live(); strings.TrimSpace(live) != "" {
		t.Fatal("Check changed the ruleset")
	}

	m := New()
	before := time.Now()
	time.Sleep(10 * time.Millisecond)
	if _, _, err := m.ApplyTrial(testRuleset, time.Second); err == nil {
		t.Fatal("a 1s trial was accepted")
	}
	revertAt, errs, err := m.ApplyTrial(testRuleset, MinTrial)
	if err != nil {
		t.Fatalf("ApplyTrial = %q %v", errs, err)
	}
	if time.Until(revertAt) > MinTrial || time.Until(revertAt) < MinTrial-time.Second {
		t.Fatalf("revert at %s", revertAt)
	}
	if live, _ := Live(); !strings.Contains(live, "set blocklist") {
		t.Fatalf("not applied: %s", live)
	}
	// A connection from before the trial proves nothing.
	if err := m.Confirm(before); err != ErrOldConnection {
		t.Fatalf("Confirm over an old connection = %v", err)
	}
	if err := m.Confirm(time.Now()); err != nil {
		t.Fatal(err)
	}
	if saved, isDefault, _ := Saved(); isDefault || saved != testRuleset {
		t.Fatalf("not saved: %q", saved)
	}
	if err := m.Confirm(time.Now()); err != ErrNoTrial {
		t.Fatalf("second Confirm = %v", err)
	}

	// Sets: a kept element, one with a timeout, deletion, injection.
	if err := m.UpdateSet("inet", "filter", "blocklist", []Element{{Value: "192.0.2.7"}, {Value: "198.51.100.0/24", Timeout: time.Hour}, {Value: "203.0.113.9"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateSet("inet", "filter", "blocklist", nil, []string{"203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateSet("inet", "filter", "blocklist", []Element{{Value: "1.2.3.4 } ; flush ruleset ; add element inet filter blocklist { 5.6.7.8"}}, nil); err == nil {
		t.Fatal("an element breaking out of the list was accepted")
	}
	if err := m.UpdateSet("inet", "filter", "nope", []Element{{Value: "192.0.2.8"}}, nil); err == nil {
		t.Fatal("an element added to a set that doesn't exist")
	}
	sets, err := m.Sets()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Element{}
	for _, e := range sets[1].Elements { // allow, then blocklist (sorted)
		got[e.Value] = e
	}
	if sets[1].Name != "blocklist" || len(got) != 2 || !got["192.0.2.7"].Persistent || got["198.51.100.0/24"].Persistent || got["198.51.100.0/24"].Timeout != time.Hour {
		t.Fatalf("blocklist = %+v", sets[1])
	}

	// A trial that isn't confirmed reverts - to the confirmed ruleset,
	// with its kept element, not the one with a timeout.
	if _, errs, err := m.ApplyTrial("table inet other {\n}\n", MinTrial); err != nil {
		t.Fatalf("ApplyTrial = %q %v", errs, err)
	}
	if live, _ := Live(); strings.Contains(live, "blocklist") || !strings.Contains(live, "table inet other") {
		t.Fatalf("second trial not applied: %s", live)
	}
	if pending, _ := m.Trial(); !pending {
		t.Fatal("no trial pending")
	}
	time.Sleep(MinTrial + time.Second)
	live, _ := Live()
	if strings.Contains(live, "table inet other") || !strings.Contains(live, "192.0.2.7") || strings.Contains(live, "198.51.100.0/24") {
		t.Fatalf("after the revert:\n%s", live)
	}
	if pending, _ := m.Trial(); pending {
		t.Fatal("still pending after the revert")
	}

	// Boot puts the saved ruleset and its kept elements back.
	if _, _, err := run("flush ruleset\n", "-f", "-"); err != nil {
		t.Fatal(err)
	}
	if err := m.Boot(); err != nil {
		t.Fatal(err)
	}
	if live, _ := Live(); !strings.Contains(live, "192.0.2.7") || !strings.Contains(live, "set allow") {
		t.Fatalf("after Boot:\n%s", live)
	}
	if _, err := os.Stat(filepath.Join(Dir, setsFile)); err != nil {
		t.Fatal(err)
	}

	// An empty ruleset removes the firewall: nothing saved any more.
	if _, errs, err := m.ApplyTrial("", MinTrial); err != nil {
		t.Fatalf("ApplyTrial(\"\") = %q %v", errs, err)
	}
	if err := m.Confirm(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, isDefault, err := Saved(); !isDefault || err != nil {
		t.Fatalf("after confirming an empty ruleset: default %v, %v", isDefault, err)
	}
	if live, _ := Live(); strings.TrimSpace(live) != "" {
		t.Fatalf("an empty ruleset left:\n%s", live)
	}
}
