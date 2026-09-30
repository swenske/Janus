package main

import (
	"net"
	"strings"
	"testing"
)

func TestMOTDLogoIsRectangular(t *testing.T) {
	width := len([]rune(motdLogo[0]))
	for i, row := range motdLogo {
		if n := len([]rune(row)); n != width {
			t.Errorf("logo row %d is %d runes wide, want %d", i, n, width)
		}
		bar := string([]rune(row)[motdBarStart:motdBarEnd])
		if strings.Trim(bar, "█▄▀") != "" {
			t.Errorf("logo row %d: bar columns %q contain something other than the bar", i, bar)
		}
	}
}

func TestRenderMOTD(t *testing.T) {
	info := motdInfo{
		Version:        "v2026.09.30",
		KernelVersion:  "6.18.53",
		ActiveSlot:     "B",
		APIAddresses:   []string{"172.16.1.78:9505"},
		HAProxyRunning: true,
	}
	out := renderMOTD(info, false)
	for _, want := range []string{"J A N U S", "v2026.09.30", "6.18.53 · boot slot B", "172.16.1.78:9505 (gRPC, mTLS)", "haproxy   running", "janusctl"} {
		if !strings.Contains(out, want) {
			t.Errorf("MOTD missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("color=false output contains ANSI escapes:\n%q", out)
	}
	if strings.Contains(out, "First boot") {
		t.Errorf("non-first-boot MOTD mentions first boot:\n%s", out)
	}

	info.ActiveSlot, info.HAProxyRunning, info.FirstBoot = "", false, true
	out = renderMOTD(info, true)
	if strings.Contains(out, "boot slot") {
		t.Errorf("MOTD without a slot still mentions one:\n%s", out)
	}
	for _, want := range []string{"not running", "First boot", ansiOrange} {
		if !strings.Contains(out, want) {
			t.Errorf("MOTD missing %q:\n%s", want, out)
		}
	}
}

func TestAPIAddresses(t *testing.T) {
	got := apiAddresses(":9505", []net.IP{net.ParseIP("10.0.2.15"), net.ParseIP("fe80::1"), net.ParseIP("fd00::1")})
	want := []string{"10.0.2.15:9505", "[fd00::1]:9505"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("apiAddresses = %v, want %v", got, want)
	}
	if got := apiAddresses("bogus", nil); len(got) != 1 || got[0] != "bogus" {
		t.Errorf("apiAddresses(bogus) = %v", got)
	}
}
