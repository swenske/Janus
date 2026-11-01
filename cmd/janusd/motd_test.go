package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

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
	if strings.Contains(out, "First boot") || strings.Contains(out, "pairing") {
		t.Errorf("a node that trusts a fleet: its MOTD mentions a first boot or a pairing:\n%s", out)
	}
	info.PairingCode = "9BC3-3016-3D39-2K7Q-M2XD"
	if out := renderMOTD(info, false); !strings.Contains(out, "pairing   9BC3-3016-3D39-2K7Q-M2XD\n") || !strings.Contains(out, "janusctl fleet adopt -code, or Add node") {
		t.Errorf("a node with its pairing window open: no code in the MOTD:\n%s", out)
	}
	info.RescueUntil = time.Date(2026, 11, 1, 18, 30, 0, 0, time.UTC)
	if out := renderMOTD(info, false); !strings.Contains(out, "rescue    9BC3-3016-3D39-2K7Q-M2XD\n") || !strings.Contains(out, "janusctl access rescue -code, until 18:30 UTC") || strings.Contains(out, "pairing ") {
		t.Errorf("a node with a rescue window: its MOTD:\n%s", out)
	}
	info.PairingCode, info.RescueUntil = "", time.Time{}

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
