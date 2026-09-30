package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

var systemUsage = []string{
	"system hostname                       print the node's hostname",
	"system reboot [-powercycle]           soft-stop HAProxy, then reboot the machine",
	"system shutdown                       soft-stop HAProxy, then power the machine off",
	"system restart                        restart janusd only - HAProxy keeps serving",
	"system reset -wipe-state [-wipe-ephemeral]  wipe the persistent STATE partition (PKI, applied config, Controller registration) and reboot - a new CA/admin cert is printed on the console",
	"system events [-since ID]             stream the node's event log (Ctrl-C to stop)",
	"system dmesg [-f]                     kernel ring buffer (-f: follow)",
	"system logs [-f] [-n LINES] SERVICE   janusd or haproxy output (-f: follow)",
	"system stats                          CPU/memory of janusd and haproxy",
	"system systemstat                     boot time, context switches, processes created",
	"system ps                             every process",
	"system du [-r] PATH...                disk usage (-r: one line per directory)",
	"system netdev                         network interface counters",
	"system netstat                        TCP/UDP sockets",
	"system mounts                         mounted filesystems",
	"system services                       managed services and their health",
	"system service start|stop|restart ID  control a managed service (haproxy; janusd: restart only)",
	"system ls [-r] PATH                   list a directory (-r: recursive)",
	"system cat PATH                       print a file",
	"system cp [-o FILE] PATH              tar archive of PATH (stdout by default)",
}

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
		_ = fs.Parse(args)
		mode := janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT
		if *powercycle {
			mode = janusv1alpha1.RebootMode_REBOOT_MODE_POWERCYCLE
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

	case "dmesg":
		fs := flag.NewFlagSet("system dmesg", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow")
		_ = fs.Parse(args)
		c, stop := interruptible()
		defer stop()
		stream, err := client.Dmesg(c, &janusv1alpha1.DmesgRequest{Follow: *follow})
		check("Dmesg", err)
		copyData(c, "Dmesg", stream.Recv, os.Stdout)

	case "logs":
		fs := flag.NewFlagSet("system logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow")
		lines := fs.Int("n", 0, "only the last N lines (0 = everything kept)")
		_ = fs.Parse(args)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system logs [-f] [-n LINES] janusd|haproxy")
			os.Exit(2)
		}
		c, stop := interruptible()
		defer stop()
		stream, err := client.Logs(c, &janusv1alpha1.LogsRequest{Id: fs.Arg(0), Follow: *follow, TailLines: int32(*lines)})
		check("Logs", err)
		copyData(c, "Logs", stream.Recv, os.Stdout)

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
		tw := table("PROTO", "LOCAL", "REMOTE", "STATE")
		for _, conn := range resp.GetConnections() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", conn.GetProtocol(), conn.GetLocalAddress(), conn.GetRemoteAddress(), conn.GetState())
		}
		tw.Flush()

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

func table(headers ...string) *tabwriter.Writer {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	return tw
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
