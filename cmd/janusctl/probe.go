package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// runProbe runs "system probe KIND TARGET": the node tests the network
// and each step prints as it comes; the exit status is 1 when the probe
// failed.
func runProbe(conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("system probe", flag.ExitOnError)
	source := fs.String("source", "", "send from this address of the node (a VIP, an anycast address)")
	recordType := fs.String("type", "", "dns: A, AAAA, CNAME, SRV, TXT, MX, NS or PTR")
	server := fs.String("server", "", "dns: ask this server, host[:port]")
	connectTo := fs.String("connect-to", "", "tls, http: connect here, host[:port], the target giving SNI and Host")
	sni := fs.String("sni", "", "tls, http: the server name sent")
	alpn := fs.String("alpn", "", "tls, http: protocols offered, comma-separated")
	caFile := fs.String("ca-file", "", "tls, http: verify against this CA (PEM) rather than the system's")
	method := fs.String("method", "", "http: GET, HEAD or OPTIONS")
	var headers []*janusv1alpha1.ProbeHeader
	fs.Func("H", "http: a header, 'Name: value' (repeatable; Host overrides the Host header)", func(v string) error {
		name, value, ok := strings.Cut(v, ":")
		if !ok {
			return errors.New("'Name: value'")
		}
		headers = append(headers, &janusv1alpha1.ProbeHeader{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
		return nil
	})
	count := fs.Uint("count", 0, "ping: echo requests (4, 20 at most)")
	interval := fs.Duration("interval", 0, "ping: between them (1s, 200ms at least)")
	size := fs.Uint("size", 0, "ping, traceroute: payload bytes (56, 9216 at most)")
	df := fs.Bool("df", false, "ping, traceroute: set the DF bit - an MTU test")
	maxHops := fs.Uint("max-hops", 0, "traceroute: 30 by default and at most")
	timeout := fs.Duration("timeout", 0, "each step's (5s; ping 2s, traceroute 1s; 10s at most)")
	verbose := fs.Bool("v", false, "the HTTP headers and body, the certificates")
	parseAnywhere(fs, args)
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: janusctl system probe [flags] dns|tcp|tls|http|ping|traceroute TARGET")
		os.Exit(2)
	}
	req := &janusv1alpha1.ProbeRequest{
		Kind: fs.Arg(0), Target: fs.Arg(1), Source: *source, RecordType: *recordType, DnsServer: *server,
		ConnectTo: *connectTo, Sni: *sni, Method: *method, Headers: headers,
		Count: uint32(*count), IntervalMs: uint32(interval.Milliseconds()), Size: uint32(*size), //nolint:gosec // G115: the node refuses what's out of bounds
		DontFragment: *df, MaxHops: uint32(*maxHops), TimeoutMs: uint32(timeout.Milliseconds()), //nolint:gosec // G115: the node refuses what's out of bounds
	}
	if *alpn != "" {
		req.Alpn = strings.Split(*alpn, ",")
	}
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			log.Fatalf("janusctl: %v", err)
		}
		req.CaPem = string(pem)
	}

	c, stop := interruptible()
	defer stop()
	stream, err := janusv1alpha1.NewSystemServiceClient(conn).Probe(c, req)
	check("Probe", err)
	ok := false
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		check("Probe", err)
		printProbeEvent(ev, *verbose)
		if ev.GetStage() == "done" {
			ok = ev.GetOk()
		}
	}
	if !ok {
		os.Exit(1)
	}
}

// printProbeEvent prints one step: its stage, ok or failed, how long
// it took, what it found.
func printProbeEvent(ev *janusv1alpha1.ProbeEvent, verbose bool) {
	state := map[bool]string{true: "ok", false: "FAIL"}[ev.GetOk()]
	took := fmt.Sprintf("%.1fms", ev.GetElapsedMs())
	if ev.GetStage() == "done" {
		took = ""
	}
	line := func(what string) { fmt.Printf("%-8s %-5s %9s  %s\n", ev.GetStage(), state, took, what) }
	sub := func(format string, a ...any) { fmt.Printf("%26s"+format+"\n", append([]any{""}, a...)...) }
	switch d := ev.GetDetail().(type) {
	case *janusv1alpha1.ProbeEvent_Dns:
		what := fmt.Sprintf("%s %s via %s: %s", d.Dns.GetName(), d.Dns.GetType(), d.Dns.GetServer(), strings.Join(d.Dns.GetRecords(), ", "))
		if !ev.GetOk() {
			what = fmt.Sprintf("%s %s via %s: %s", d.Dns.GetName(), d.Dns.GetType(), d.Dns.GetServer(), ev.GetMessage())
		}
		line(what)
	case *janusv1alpha1.ProbeEvent_Connect:
		if ev.GetOk() {
			line(fmt.Sprintf("%s from %s", d.Connect.GetRemote(), d.Connect.GetLocal()))
		} else {
			line(fmt.Sprintf("%s: %s", d.Connect.GetRemote(), ev.GetMessage()))
		}
	case *janusv1alpha1.ProbeEvent_Tls:
		t := d.Tls
		trust := "certificate verified"
		if !t.GetVerified() {
			trust = "certificate NOT trusted: " + t.GetVerifyError()
		}
		line(fmt.Sprintf("%s, %s, ALPN %s, SNI %s - %s", t.GetVersion(), t.GetCipher(), orDash(t.GetAlpn()), orDash(t.GetServerName()), trust))
		if t.GetOcsp() != "" {
			sub("OCSP stapled: %s", t.GetOcsp())
		}
		for i, c := range t.GetChain() {
			if i > 0 && !verbose {
				break
			}
			names := strings.Join(append(c.GetDnsNames(), c.GetIps()...), ", ")
			sub("cert %d: %s (%s) until %s, issuer %s", i, c.GetSubject(), orDash(names), time.Unix(c.GetNotAfterUnix(), 0).UTC().Format("2006-01-02"), c.GetIssuer())
		}
	case *janusv1alpha1.ProbeEvent_Http:
		h := d.Http
		size := fmt.Sprintf("%d bytes", h.GetBodyBytes())
		if h.GetBodyTruncated() {
			size = "more than " + size
		}
		line(fmt.Sprintf("%s %d %s, %s, first byte after %.1fms", h.GetProto(), h.GetStatus(), h.GetStatusText(), size, h.GetTtfbMs()))
		if verbose {
			for _, x := range h.GetHeaders() {
				sub("%s: %s", x.GetName(), x.GetValue())
			}
			if h.GetBodyPreview() != "" {
				sub("%s", strings.ReplaceAll(h.GetBodyPreview(), "\n", "\n"+strings.Repeat(" ", 26)))
			}
		}
	case *janusv1alpha1.ProbeEvent_Reply:
		r := d.Reply
		switch r.GetKind() {
		case "reply":
			line(fmt.Sprintf("seq %d from %s: ttl %d, %d bytes, %.2fms", r.GetSeq(), r.GetFrom(), r.GetTtl(), r.GetBytes(), r.GetRttMs()))
		case "timeout":
			line(fmt.Sprintf("seq %d: no reply", r.GetSeq()))
		default:
			what := fmt.Sprintf("seq %d: %s", r.GetSeq(), r.GetKind())
			if r.GetFrom() != "" {
				what += " from " + r.GetFrom()
			}
			if r.GetMtu() > 0 {
				what += fmt.Sprintf(", MTU %d", r.GetMtu())
			}
			if ev.GetMessage() != "" {
				what += " - " + ev.GetMessage()
			}
			line(what)
		}
	case *janusv1alpha1.ProbeEvent_Hop:
		h := d.Hop
		var rtts []string
		for _, r := range h.GetRttsMs() {
			if r < 0 {
				rtts = append(rtts, "*")
			} else {
				rtts = append(rtts, fmt.Sprintf("%.2f", r))
			}
		}
		where := orDash(h.GetAddress())
		if h.GetName() != "" {
			where += " (" + h.GetName() + ")"
		}
		fmt.Printf("%-8s %-5d %9s  %s  %s ms\n", "hop", h.GetTtl(), took, where, strings.Join(rtts, " "))
	case *janusv1alpha1.ProbeEvent_Summary:
		s := d.Summary
		what := ev.GetMessage()
		if s.GetReceived() > 0 {
			what += fmt.Sprintf(", min/avg/max %.2f/%.2f/%.2f ms", s.GetMinMs(), s.GetAvgMs(), s.GetMaxMs())
		}
		line(what)
	default:
		line(ev.GetMessage())
	}
}

// orDash is s, or "-" for nothing.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
