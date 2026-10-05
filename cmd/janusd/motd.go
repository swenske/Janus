package main

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// motdLogo is brand/favicon/favicon.svg rasterized onto half-block
// characters (one character = two square "pixels" vertically, 3 SVG
// units per pixel): the two outward-facing triangles are Janus's two
// faces, the taller bar between them is the brand-orange stroke.
// Columns [motdBarStart, motdBarEnd) are the bar.
var motdLogo = []string{
	"           ▄▄▄▄▄▄           ",
	"           ██████           ",
	"           ██████           ",
	"         ▄ ██████ ▄         ",
	"       ▄██ ██████ ██▄       ",
	"    ▄▄████ ██████ ████▄▄    ",
	"  ▄███████ ██████ ███████▄  ",
	"  ▀███████ ██████ ███████▀  ",
	"    ▀▀████ ██████ ████▀▀    ",
	"       ▀██ ██████ ██▀       ",
	"         ▀ ██████ ▀         ",
	"           ██████           ",
	"           ██████           ",
	"           ▀▀▀▀▀▀           ",
}

const (
	motdBarStart = 11
	motdBarEnd   = 17

	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiOrange = "\x1b[38;5;166m" // closest xterm-256 color to the brand's #C2502A
)

type motdInfo struct {
	Version        string
	KernelVersion  string
	ActiveSlot     string
	APIAddresses   []string
	HAProxyRunning bool
	FirstBoot      bool
	// CAFingerprint is the SHA-256 of the node's own CA (hex): what
	// janusctl fleet adopt -ca-fingerprint checks the node against.
	CAFingerprint string
}

// renderMOTD builds the console banner janusd prints once it's
// listening - this appliance has no shell or login, so the serial
// console is the only place a "message of the day" can ever be seen.
func renderMOTD(info motdInfo, color bool) string {
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return code + s + ansiReset
	}

	slot := ""
	if info.ActiveSlot != "" {
		slot = " · boot slot " + info.ActiveSlot
	}
	haproxyState := paint(ansiOrange, "not running")
	if info.HAProxyRunning {
		haproxyState = "running"
	}
	api := "(no address yet)"
	if len(info.APIAddresses) > 0 {
		api = strings.Join(info.APIAddresses, ", ")
	}

	text := []string{
		"",
		"",
		paint(ansiBold, "J A N U S"),
		paint(ansiDim, "Immutable HAProxy appliance · alpha"),
		"",
		"version   " + info.Version,
		"kernel    " + info.KernelVersion + slot,
		"api       " + api + " (gRPC, mTLS)",
		"haproxy   " + haproxyState,
	}
	if fp := info.CAFingerprint; len(fp) == 64 {
		group := func(s string) string { return s[0:8] + " " + s[8:16] + " " + s[16:24] + " " + s[24:32] }
		text = append(text, "ca sha256 "+group(fp[:32]), "          "+group(fp[32:]))
	}
	text = append(text,
		"",
		"No shell, no SSH: manage this node through",
		"its API, with janusctl or the Janus Controller.",
	)
	if info.FirstBoot {
		text = append(text, "", paint(ansiOrange, "First boot: the admin credentials are printed above - save them now."))
	}

	var b strings.Builder
	b.WriteString("\n")
	for i := 0; i < len(motdLogo) || i < len(text); i++ {
		logo := strings.Repeat(" ", len([]rune(motdLogo[0])))
		if i < len(motdLogo) {
			logo = motdLogo[i]
		}
		if color {
			r := []rune(logo)
			logo = string(r[:motdBarStart]) + ansiOrange + string(r[motdBarStart:motdBarEnd]) + ansiReset + string(r[motdBarEnd:])
		}
		line := "  " + logo
		if i < len(text) && text[i] != "" {
			line += "   " + text[i]
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// apiAddresses pairs every local IP with the gRPC listen port, falling
// back to the raw listen address if it can't be split. IPv6 link-local
// addresses are left out: unusable without an interface zone.
func apiAddresses(listenAddr string, ips []net.IP) []string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return []string{listenAddr}
	}
	var out []string
	for _, ip := range ips {
		if ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, net.JoinHostPort(ip.String(), port))
	}
	return out
}

// motdColor honors the NO_COLOR convention (https://no-color.org).
func motdColor() bool {
	_, set := os.LookupEnv("NO_COLOR")
	return !set
}

func printMOTD(info motdInfo) {
	fmt.Fprint(os.Stdout, renderMOTD(info, motdColor()))
}
