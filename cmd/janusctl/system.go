package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// runSystemCommand handles the system subcommands beyond info and pcap;
// false means cmd isn't one of them.
func runSystemCommand(conn *grpc.ClientConn, cmd string, args []string) bool {
	client := janusv1alpha1.NewSystemServiceClient(conn)
	switch cmd {
	case "hostname":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.Hostname(c, &emptypb.Empty{})
		check("Hostname", err)
		fmt.Println(resp.GetHostname())

	case "reboot":
		fs := flag.NewFlagSet("system reboot", flag.ExitOnError)
		powercycle := fs.Bool("powercycle", false, "request a power cycle (same as a normal reboot today)")
		kexecMode := fs.Bool("kexec", false, "jump into the active kernel without going through the firmware - seconds instead of a server's POST; refused, nothing rebooted, when Secure Boot is on and the image isn't signed by a Janus release certificate")
		_ = fs.Parse(args)
		mode := janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT
		if *powercycle {
			mode = janusv1alpha1.RebootMode_REBOOT_MODE_POWERCYCLE
		}
		if *kexecMode {
			mode = janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.Reboot(c, &janusv1alpha1.RebootRequest{Mode: mode})
		check("Reboot", err)
		fmt.Println("node is rebooting")

	case "shutdown":
		c, cancel := ctx()
		defer cancel()
		_, err := client.Shutdown(c, &emptypb.Empty{})
		check("Shutdown", err)
		fmt.Println("node is powering off")

	case "restart":
		c, cancel := ctx()
		defer cancel()
		_, err := client.Restart(c, &emptypb.Empty{})
		check("Restart", err)
		fmt.Println("janusd is restarting (HAProxy keeps serving)")

	case "reset":
		fs := flag.NewFlagSet("system reset", flag.ExitOnError)
		wipeState := fs.Bool("wipe-state", false, "wipe the persistent STATE partition")
		wipeEphemeral := fs.Bool("wipe-ephemeral", false, "wipe ephemeral state (tmpfs - any reboot does)")
		_ = fs.Parse(args)
		if !*wipeState && !*wipeEphemeral {
			fmt.Fprintln(os.Stderr, "usage: janusctl system reset -wipe-state [-wipe-ephemeral]")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.Reset(c, &janusv1alpha1.ResetRequest{WipeState: *wipeState, WipeEphemeral: *wipeEphemeral})
		check("Reset", err)
		fmt.Println("node is resetting and rebooting")
		if *wipeState {
			fmt.Println("its PKI is gone: a new CA and admin certificate will be printed on its console at the next boot - your current certificates stop working")
		}

	case "events":
		fs := flag.NewFlagSet("system events", flag.ExitOnError)
		since := fs.Uint64("since", 0, "only events after this ID")
		_ = fs.Parse(args)
		c, stop := interruptible()
		defer stop()
		stream, err := client.Events(c, &janusv1alpha1.EventsRequest{SinceId: *since})
		check("Events", err)
		for {
			e, err := stream.Recv()
			if done(c, err) {
				return true
			}
			check("Events", err)
			fmt.Printf("%d\t%s\t%s\t%s\n", e.GetId(), time.Unix(0, e.GetUnixTimeNs()).Format(time.RFC3339), e.GetType(), e.GetPayload())
		}

	case "audit":
		fs := flag.NewFlagSet("system audit", flag.ExitOnError)
		since := fs.Uint64("since", 0, "only entries after this ID")
		_ = fs.Parse(args)
		c, stop := interruptible()
		defer stop()
		stream, err := client.Audit(c, &janusv1alpha1.EventsRequest{SinceId: *since})
		check("Audit", err)
		for {
			e, err := stream.Recv()
			if done(c, err) {
				return true
			}
			check("Audit", err)
			fmt.Println(auditLine(e))
		}

	case "dmesg":
		fs := flag.NewFlagSet("system dmesg", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow")
		boot := fs.Int("boot", 0, "a previous boot's kernel messages, from the LOGS partition: -1 the boot before this one (janusctl system boots)")
		_ = fs.Parse(args)
		c, stop := interruptible()
		defer stop()
		stream, err := client.Dmesg(c, &janusv1alpha1.DmesgRequest{Follow: *follow, Boot: int32(*boot)})
		check("Dmesg", err)
		copyData(c, "Dmesg", stream.Recv, os.Stdout)

	case "logs":
		fs := flag.NewFlagSet("system logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow")
		lines := fs.Int("n", 0, "only the last N lines (0 = everything kept)")
		boot := fs.Int("boot", 0, "a previous boot's output, from the LOGS partition: -1 the boot before this one (janusctl system boots)")
		_ = fs.Parse(args)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system logs [-f] [-n LINES] [-boot N] janusd|haproxy")
			os.Exit(2)
		}
		c, stop := interruptible()
		defer stop()
		stream, err := client.Logs(c, &janusv1alpha1.LogsRequest{Id: fs.Arg(0), Follow: *follow, TailLines: int32(*lines), Boot: int32(*boot)})
		check("Logs", err)
		copyData(c, "Logs", stream.Recv, os.Stdout)

	case "boots":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.LogBoots(c, &emptypb.Empty{})
		check("LogBoots", err)
		if !resp.GetAvailable() {
			fmt.Println("no LOGS partition: only this boot's logs are kept, in memory")
			return true
		}
		fmt.Printf("LOGS partition %s, %s: %d records written since janusd started, %d dropped\n",
			resp.GetDevice(), humanBytes(resp.GetSizeBytes()), resp.GetRecordsWritten(), resp.GetRecordsDropped())
		tw := table("BOOT", "STARTED", "SIZE", "BOOT ID")
		for _, b := range resp.GetBoots() {
			fmt.Fprintf(tw, "%d\t%s\t%d MiB\t%s\n", b.GetOffset(), time.Unix(0, b.GetStartedUnixNs()).Format(time.RFC3339), b.GetMebibytes(), b.GetBootId())
		}
		tw.Flush()

	case "stats":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.Stats(c, &emptypb.Empty{})
		check("Stats", err)
		tw := table("SERVICE", "CPU%", "MEMORY")
		for _, p := range resp.GetProcesses() {
			fmt.Fprintf(tw, "%s\t%.1f\t%s\n", p.GetId(), p.GetCpuPercent(), humanBytes(p.GetMemoryBytes()))
		}
		tw.Flush()

	case "systemstat":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.SystemStat(c, &emptypb.Empty{})
		check("SystemStat", err)
		boot := time.Unix(int64(resp.GetBootTimeUnix()), 0)
		fmt.Printf("booted: %s (up %s)\n", boot.Format(time.RFC3339), time.Since(boot).Round(time.Second))
		fmt.Printf("context switches: %d\n", resp.GetContextSwitches())
		fmt.Printf("processes created: %d\n", resp.GetProcessesCreated())
		fmt.Printf("cpu ticks: %d total, %d idle, %d steal\n", resp.GetCpuTotalTicks(), resp.GetCpuIdleTicks(), resp.GetCpuStealTicks())

	case "ps":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.Processes(c, &emptypb.Empty{})
		check("Processes", err)
		tw := table("PID", "CPU%", "MEMORY", "COMMAND")
		for _, p := range resp.GetProcesses() {
			fmt.Fprintf(tw, "%d\t%.1f\t%s\t%s\n", p.GetPid(), p.GetCpuPercent(), humanBytes(p.GetMemoryBytes()), p.GetCommand())
		}
		tw.Flush()

	case "du":
		fs := flag.NewFlagSet("system du", flag.ExitOnError)
		recursive := fs.Bool("r", false, "one line per directory")
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system du [-r] PATH...")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		stream, err := client.DiskUsage(c, &janusv1alpha1.DiskUsageRequest{Paths: fs.Args(), Recursive: *recursive})
		check("DiskUsage", err)
		for {
			u, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			check("DiskUsage", err)
			fmt.Printf("%s\t%s\n", humanBytes(uint64(u.GetSizeBytes())), u.GetPath())
		}

	case "netdev":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.NetworkDeviceStats(c, &emptypb.Empty{})
		check("NetworkDeviceStats", err)
		tw := table("INTERFACE", "RX", "TX", "RX ERRORS", "TX ERRORS")
		for _, d := range resp.GetDevices() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", d.GetName(), humanBytes(d.GetRxBytes()), humanBytes(d.GetTxBytes()), d.GetRxErrors(), d.GetTxErrors())
		}
		tw.Flush()

	case "netstat":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.Netstat(c, &emptypb.Empty{})
		check("Netstat", err)
		tw := table("PROTO", "LOCAL", "REMOTE", "STATE", "RECEIVED", "SENT")
		for _, conn := range resp.GetConnections() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", conn.GetProtocol(), conn.GetLocalAddress(), conn.GetRemoteAddress(), conn.GetState(), countedBytes(conn.RxBytes), countedBytes(conn.TxBytes))
		}
		tw.Flush()

	case "clients":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.ClientList(c, &emptypb.Empty{})
		check("ClientList", err)
		printClients(os.Stdout, resp, time.Now())

	case "mounts":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.Mounts(c, &emptypb.Empty{})
		check("Mounts", err)
		tw := table("FILESYSTEM", "MOUNTED ON", "SIZE", "AVAILABLE", "MODE")
		for _, m := range resp.GetMounts() {
			mode := "rw"
			if m.GetReadOnly() {
				mode = "ro"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.GetFilesystem(), m.GetMountedOn(), humanBytes(m.GetSizeBytes()), humanBytes(m.GetAvailableBytes()), mode)
		}
		tw.Flush()

	case "metrics":
		fs := flag.NewFlagSet("system metrics", flag.ExitOnError)
		enable := fs.Bool("enable", false, "turn the exporter on")
		disable := fs.Bool("disable", false, "turn the exporter off")
		port := fs.Uint("port", 0, "serve on this port (default 10056)")
		address := fs.String("address", "", "serve on this address only (\"auto\" for the main interface's, \"*\" for every address)")
		_ = fs.Parse(args)
		c, cancel := ctx()
		defer cancel()
		resp, err := client.MetricsConfigGet(c, &emptypb.Empty{})
		check("MetricsConfigGet", err)
		if *enable || *disable || *port != 0 || *address != "" {
			if *enable && *disable {
				fmt.Fprintln(os.Stderr, "-enable and -disable together")
				os.Exit(2)
			}
			cfg := resp.GetConfig()
			req := &janusv1alpha1.MetricsConfig{Enabled: cfg.GetEnabled(), Port: cfg.GetPort(), Address: cfg.GetAddress()}
			if *enable {
				req.Enabled = true
			}
			if *disable {
				req.Enabled = false
			}
			if *port != 0 {
				req.Port = uint32(*port)
			}
			switch *address {
			case "":
			case "*":
				req.Address = ""
			default:
				req.Address = *address
			}
			resp, err = client.MetricsConfigSet(c, req)
			check("MetricsConfigSet", err)
		}
		cfg := resp.GetConfig()
		where := listenURLs(cfg.GetAddress(), resp.GetEffectiveAddresses(), cfg.GetPort())
		switch {
		case !cfg.GetEnabled():
			fmt.Println("Exporter: disabled")
		case resp.GetListening():
			fmt.Printf("Exporter: %s\n", where)
		default:
			fmt.Printf("Exporter: enabled on %s, but not listening: %s\n", where, resp.GetError())
		}
		if resp.GetIsDefault() {
			fmt.Println("(default settings)")
		}

	case "console-budget":
		fs := flag.NewFlagSet("system console-budget", flag.ExitOnError)
		rate := fs.Int("rate", -1, "bytes a second of the services' output the console gets (0: none of it)")
		burst := fs.Int("burst", -1, "at most this much at once")
		useDefault := fs.Bool("default", false, "janusd's default again")
		_ = fs.Parse(args)
		c, cancel := ctx()
		defer cancel()
		resp, err := client.ConsoleBudgetGet(c, &emptypb.Empty{})
		check("ConsoleBudgetGet", err)
		if *useDefault || *rate >= 0 || *burst >= 0 {
			if *useDefault && (*rate >= 0 || *burst >= 0) {
				fmt.Fprintln(os.Stderr, "-default with -rate or -burst")
				os.Exit(2)
			}
			cfg := resp.GetConfig()
			req := &janusv1alpha1.ConsoleBudget{RateBytesPerSecond: cfg.GetRateBytesPerSecond(), BurstBytes: cfg.GetBurstBytes(), UseDefault: *useDefault}
			if *rate >= 0 {
				req.RateBytesPerSecond = uint32(*rate)
			}
			if *burst >= 0 {
				req.BurstBytes = uint32(*burst)
			}
			resp, err = client.ConsoleBudgetSet(c, req)
			check("ConsoleBudgetSet", err)
		}
		cfg := resp.GetConfig()
		if cfg.GetRateBytesPerSecond() == 0 {
			fmt.Println("Console: none of the services' output (janusd's own log only)")
		} else {
			fmt.Printf("Console: %d bytes a second of the services' output, %d at most at once\n", cfg.GetRateBytesPerSecond(), cfg.GetBurstBytes())
		}
		if resp.GetIsDefault() {
			fmt.Println("(default)")
		} else {
			fmt.Printf("(default: %d a second, %d at once)\n", resp.GetDefault().GetRateBytesPerSecond(), resp.GetDefault().GetBurstBytes())
		}
		fmt.Printf("Lines left out since janusd started: %d (kept: janusctl system logs)\n", resp.GetLinesLeftOutTotal())

	case "node-exporter":
		fs := flag.NewFlagSet("system node-exporter", flag.ExitOnError)
		enable := fs.Bool("enable", false, "run node_exporter")
		disable := fs.Bool("disable", false, "stop node_exporter, and keep it stopped")
		address := fs.String("address", "", "listen on this address only (\"auto\" for the main interface's, \"*\" for every address)")
		port := fs.Uint("port", 0, "listen on this port (default 9100)")
		collectors := fs.String("collectors", "", "the collectors to run, comma-separated (\"default\" for the default ones)")
		_ = fs.Parse(args)
		c, cancel := ctx()
		defer cancel()
		resp, err := client.NodeExporterConfigGet(c, &emptypb.Empty{})
		check("NodeExporterConfigGet", err)
		if *enable || *disable || *address != "" || *port != 0 || *collectors != "" {
			if *enable && *disable {
				fmt.Fprintln(os.Stderr, "-enable and -disable together")
				os.Exit(2)
			}
			cfg := resp.GetConfig()
			req := &janusv1alpha1.NodeExporterConfig{Enabled: cfg.GetEnabled(), Address: cfg.GetAddress(), Port: cfg.GetPort(), Collectors: cfg.GetCollectors()}
			if *enable || *disable {
				req.Enabled = *enable
			}
			switch *address {
			case "":
			case "*":
				req.Address = ""
			default:
				req.Address = *address
			}
			if *port != 0 {
				req.Port = uint32(*port)
			}
			switch *collectors {
			case "":
			case "default":
				req.Collectors = nil
			default:
				req.Collectors = strings.Split(*collectors, ",")
			}
			resp, err = client.NodeExporterConfigSet(c, req)
			check("NodeExporterConfigSet", err)
		}
		cfg := resp.GetConfig()
		if cfg.GetEnabled() {
			fmt.Printf("node_exporter: %s (%s)\n", listenURLs(cfg.GetAddress(), resp.GetEffectiveAddresses(), cfg.GetPort()), resp.GetState())
		} else {
			fmt.Printf("node_exporter: disabled (%s)\n", resp.GetState())
		}
		if resp.GetError() != "" {
			fmt.Println("Error:", resp.GetError())
		}
		on := map[string]bool{}
		for _, n := range cfg.GetCollectors() {
			on[n] = true
		}
		tw := table("COLLECTOR", "", "DESCRIPTION")
		for _, k := range resp.GetAvailableCollectors() {
			mark := ""
			if on[k.GetName()] {
				mark = "on"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", k.GetName(), mark, k.GetDescription())
		}
		tw.Flush()
		if resp.GetIsDefault() {
			fmt.Println("(default settings)")
		}

	case "services":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.ServiceList(c, &emptypb.Empty{})
		check("ServiceList", err)
		tw := table("SERVICE", "STATE", "HEALTH")
		for _, s := range resp.GetServices() {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", s.GetId(), s.GetState(), s.GetHealth())
		}
		tw.Flush()

	case "service":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system service start|stop|restart ID")
			os.Exit(2)
		}
		// A soft stop waits for in-flight connections, up to 10s node-side.
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req := &janusv1alpha1.ServiceRequest{Id: args[1]}
		var resp *janusv1alpha1.ServiceResponse
		var err error
		switch args[0] {
		case "start":
			resp, err = client.ServiceStart(c, req)
		case "stop":
			resp, err = client.ServiceStop(c, req)
		case "restart":
			resp, err = client.ServiceRestart(c, req)
		default:
			fmt.Fprintln(os.Stderr, "usage: janusctl system service start|stop|restart ID")
			os.Exit(2)
		}
		check("Service "+args[0], err)
		fmt.Printf("%s: %s, %s\n", resp.GetService().GetId(), resp.GetService().GetState(), resp.GetService().GetHealth())

	case "ls":
		fs := flag.NewFlagSet("system ls", flag.ExitOnError)
		recursive := fs.Bool("r", false, "recursive")
		_ = fs.Parse(args)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system ls [-r] PATH")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		stream, err := client.List(c, &janusv1alpha1.ListRequest{Root: fs.Arg(0), Recursive: *recursive})
		check("List", err)
		tw := table("MODE", "SIZE", "NAME")
		for {
			f, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			check("List", err)
			if f.GetError() != "" {
				fmt.Fprintf(tw, "?\t?\t%s (%s)\n", f.GetRelativeName(), f.GetError())
				continue
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\n", fs2mode(f.GetMode()), f.GetSize(), f.GetRelativeName())
		}
		tw.Flush()

	case "cat":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system cat PATH")
			os.Exit(2)
		}
		c, stop := interruptible()
		defer stop()
		stream, err := client.Read(c, &janusv1alpha1.ReadRequest{Path: args[0]})
		check("Read", err)
		copyData(c, "Read", stream.Recv, os.Stdout)

	case "cp":
		fs := flag.NewFlagSet("system cp", flag.ExitOnError)
		out := fs.String("o", "-", "output tar file, - for stdout")
		_ = fs.Parse(args)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system cp [-o FILE] PATH")
			os.Exit(2)
		}
		var w io.Writer = os.Stdout
		if *out != "-" {
			f, err := os.Create(*out)
			if err != nil {
				log.Fatalf("create %s: %v", *out, err)
			}
			defer f.Close()
			w = f
		}
		c, stop := interruptible()
		defer stop()
		stream, err := client.Copy(c, &janusv1alpha1.CopyRequest{RootPath: fs.Arg(0)})
		check("Copy", err)
		copyData(c, "Copy", stream.Recv, w)

	default:
		return false
	}
	return true
}

func check(what string, err error) {
	if err != nil {
		log.Fatalf("%s: %v", what, err)
	}
}

// interruptible is a context Ctrl-C cancels - the normal way to stop a
// followed stream.
func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// done reports whether a stream ended normally: EOF, or cancelled by us.
func done(c context.Context, err error) bool {
	return errors.Is(err, io.EOF) || (err != nil && c.Err() != nil) || status.Code(err) == codes.Canceled
}

func copyData(c context.Context, what string, recv func() (*janusv1alpha1.Data, error), w io.Writer) {
	for {
		d, err := recv()
		if done(c, err) {
			return
		}
		check(what, err)
		if _, err := w.Write(d.GetBytes()); err != nil {
			log.Fatalf("write: %v", err)
		}
	}
}

// table is a table on stdout, headers first (ui.go: bold, its states
// coloured, on a terminal).
func table(headers ...string) *tableWriter {
	tw := newTable(os.Stdout)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	return tw
}

// countedBytes: a counter the node may not give (a UDP socket, a
// listener, an older node) - "-" then.
func countedBytes(n *uint64) string {
	if n == nil {
		return "-"
	}
	return humanBytes(*n)
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fs2mode(m uint32) string {
	return fs.FileMode(m).String()
}

// auditLine is an audit entry as janusctl system audit prints it: "ID
// TIME METHOD by WHO from PEER: RESULT".
func auditLine(e *janusv1alpha1.Event) string {
	var a struct {
		Caller     string   `json:"caller"`
		Roles      []string `json:"roles"`
		Via        string   `json:"via"`
		Peer       string   `json:"peer"`
		Code       string   `json:"code"`
		Error      string   `json:"error"`
		Denied     bool     `json:"denied"`
		DurationMs int64    `json:"duration_ms"`
	}
	_ = json.Unmarshal(e.GetPayload(), &a)
	who := a.Caller
	if len(a.Roles) > 0 {
		who += " (" + strings.Join(a.Roles, ", ") + ")"
	}
	if a.Via != "" {
		who += " via " + a.Via
	}
	result := fmt.Sprintf("OK, %d ms", a.DurationMs)
	switch {
	case a.Denied:
		result = "refused: " + a.Error
	case a.Code != "OK":
		result = a.Code + ": " + a.Error
	}
	return fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t%s", e.GetId(), time.Unix(0, e.GetUnixTimeNs()).Format(time.RFC3339), e.GetType(), who, a.Peer, result)
}

// printClients is ClientList as a table: a connection a line - this
// one marked, a name the node keeps from a non-admin "-" -, then the
// users a Controller acts for, under it.
func printClients(w io.Writer, resp *janusv1alpha1.ClientListResponse, now time.Time) {
	ago := func(ns int64) string {
		if ns <= 0 {
			return "-"
		}
		// The node's clock may be a few seconds ahead of this one.
		if d := now.Sub(time.Unix(0, ns)).Round(time.Second); d > 0 {
			return d.String() + " ago"
		}
		return "now"
	}
	tw := newTable(w)
	unnamed := false
	fmt.Fprintln(tw, "ID\tCLIENT\tROLES\tFROM\tVIA\tOPENED\tCALLS\tSTREAMS\tIN\tOUT\tLAST CALL")
	for _, c := range resp.GetConnections() {
		name := cmp.Or(c.GetName(), "-")
		if c.GetController() {
			name += " (Controller)"
		}
		if c.GetSelf() {
			name += " (this one)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n", c.GetId(), name, cmp.Or(strings.Join(c.GetRoles(), ","), "-"),
			cmp.Or(c.GetRemoteAddress(), "-"), cmp.Or(c.GetInterface(), "-"), ago(c.GetOpenedUnixNs()), c.GetCalls(), c.GetOpenStreams(),
			humanBytes(c.GetBytesReceived()), humanBytes(c.GetBytesSent()), ago(c.GetLastCallUnixNs()))
		for _, u := range c.GetUsers() {
			name := "  for " + u.GetName()
			if u.GetSelf() {
				name += " (you)"
			}
			fmt.Fprintf(tw, "\t%s\t%s\t\t\t\t%d\t%d\t\t\t%s\n", name, strings.Join(u.GetRoles(), ","), u.GetCalls(), u.GetOpenStreams(), ago(u.GetLastCallUnixNs()))
		}
		if hidden := int(c.GetUserCount()) - len(c.GetUsers()); hidden > 0 {
			fmt.Fprintf(tw, "\t  for %d more\t\t\t\t\t\t\t\t\t\n", hidden)
			unnamed = true
		}
	}
	tw.Flush()
	if unnamed {
		fmt.Fprintln(w, "The users a Controller acts for are named to an admin.")
	}
}

// listenURLs says where an exporter answers: its effective addresses
// (the main interface's for "auto"), else <node> for every address, or
// the address set (a node older than effective_addresses).
func listenURLs(address string, effective []string, port uint32) string {
	url := func(host string) string {
		return "http://" + net.JoinHostPort(host, strconv.Itoa(int(port))) + "/metrics"
	}
	switch {
	case len(effective) > 0:
		urls := make([]string, len(effective))
		for i, a := range effective {
			urls[i] = url(a)
		}
		out := strings.Join(urls, " ")
		if address == "auto" {
			out += " (main interface)"
		}
		return out
	case address == "auto":
		return url("<node>") + " (every address: no main interface)"
	case address == "":
		return url("<node>")
	default:
		return url(address)
	}
}
