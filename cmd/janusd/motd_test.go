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
		if strings.Trim(bar, "█▄▀▐▌ ") != "" {
			t.Errorf("logo row %d: bar columns %q contain something other than the bar", i, bar)
		}
		if strings.Trim(row, "█▄▀▐▌ ") != "" {
			t.Errorf("logo row %d: %q uses a glyph the console font may not have", i, row)
		}
		// Janus's two faces mirror each other: read right to left (with
		// the half blocks swapped), every row is itself.
		r := []rune(row)
		mirror := make([]rune, len(r))
		for j, c := range r {
			switch c {
			case '▐':
				c = '▌'
			case '▌':
				c = '▐'
			}
			mirror[len(r)-1-j] = c
		}
		if string(mirror) != row {
			t.Errorf("logo row %d isn't symmetric: %q vs %q", i, row, string(mirror))
		}
	}
	if motdBarStart+motdBarEnd-1 != len([]rune(motdLogo[0]))-1 {
		t.Errorf("bar columns [%d, %d) aren't centered in a %d-column logo", motdBarStart, motdBarEnd, len([]rune(motdLogo[0])))
	}
}

func TestRenderMOTD(t *testing.T) {
	info := motdInfo{
		Version:        "v2026.09.30",
		KernelVersion:  "6.18.53",
		ActiveSlot:     "B",
		KernelTrack:    "longterm",
		APIAddresses:   []string{"172.16.1.78:9505"},
		HAProxyVersion: "3.4.6",
		HAProxyRunning: true,
		CAFingerprint:  "0123456789abcdef0123456789abcdef89abcdef0123456789abcdef01234567",
	}
	out := renderMOTD(info, false)
	for _, want := range []string{"J A N U S", "v2026.09.30", "6.18.53 longterm · boot slot B", "172.16.1.78:9505 (gRPC, mTLS)", "haproxy   3.4.6 · running", "janusctl",
		"ca sha256 01234567 89abcdef 01234567 89abcdef\n", "          89abcdef 01234567 89abcdef 01234567\n"} {
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
