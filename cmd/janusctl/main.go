// Command janusctl is the Janus admin CLI, a thin client over the
// gRPC API served by janusd (api/proto/janus/v1alpha1), always
// over mTLS (internal/pki) - there is no insecure fallback. Command
// surface grows alongside its corresponding service implementation (see
// docs/architecture.md's roadmap).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/diskseed"
	"github.com/swenske/Janus/internal/pki"
)

var version = "dev"

func main() {
	endpoint := flag.String("endpoint", "127.0.0.1:9505", "janusd gRPC endpoint")
	// Defaults match janusd's own default -pki-dir - convenient when
	// janusctl runs on the same filesystem as the daemon (local/dev
	// use); a real remote operator passes their own issued certificate
	// (see "haproxy show-info" etc. needing a cert from
	// GenerateClientConfiguration first, or the admin cert janusd
	// printed on its first boot).
	caFile := flag.String("ca", "/etc/janus/pki/ca.crt", "path to the CA certificate")
	certFile := flag.String("cert", "/etc/janus/pki/admin.crt", "path to the client certificate")
	keyFile := flag.String("key", "/etc/janus/pki/admin.key", "path to the client private key")
	flag.Parse()

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	// "image" subcommands operate directly on a disk file offline -
	// unlike every other command here, they need no running janusd/gRPC
	// connection at all (that's the entire point, see internal/diskseed's
	// own package doc) - dispatched before dial() so a missing/
	// unreachable -endpoint never gets in the way.
	if flag.Arg(0) == "image" {
		runImage(flag.Args()[1:])
		return
	}

	conn, err := dial(*endpoint, *caFile, *certFile, *keyFile)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	switch cmd := flag.Arg(0); cmd {
	case "version":
		runVersion(conn)
	case "system":
		runSystem(conn, flag.Args()[1:])
	case "haproxy":
		runHAProxy(conn, flag.Args()[1:])
	case "pki":
		runPKI(conn, flag.Args()[1:])
	case "lifecycle":
		runLifecycle(conn, flag.Args()[1:])
	case "network":
		runNetwork(conn, flag.Args()[1:])
	default:
		fmt.Fprintf(os.Stderr, "janusctl: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func dial(endpoint, caFile, certFile, keyFile string) (*grpc.ClientConn, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate %s: %w", caFile, err)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read client certificate %s: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read client key %s: %w", keyFile, err)
	}

	tlsConfig, err := pki.ClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("build TLS config: %w", err)
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}
	return conn, nil
}

// runPcap streams SystemService.PacketCapture's pcap bytes to a file or
// stdout until -duration elapses or the user interrupts.
func runPcap(conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("system pcap", flag.ExitOnError)
	iface := fs.String("i", "", "interface to capture on (required, e.g. eth0)")
	filter := fs.String("f", "", "tcpdump-style filter expression (see docs/packet-capture.md for the supported subset)")
	promisc := fs.Bool("promisc", false, "put the interface in promiscuous mode for the capture's duration")
	includeOwn := fs.Bool("include-own-stream", false, "also capture this capture's own gRPC connection (left out by default; it feeds back on itself and grows very fast)")
	snapLen := fs.Uint("snaplen", 0, "bytes kept per packet (0 = 65535)")
	duration := fs.Duration("duration", 0, "stop after this long, rounded up to whole seconds (0 = until interrupted)")
	out := fs.String("o", "-", "output pcap file, - for stdout")
	_ = fs.Parse(args)
	if *iface == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: janusctl system pcap -i IFACE [-f FILTER] [-promisc] [-include-own-stream] [-snaplen N] [-duration D] [-o FILE]")
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

	// -duration is enforced by the node itself (duration_seconds), so it
	// flushes its last packets and ends the stream cleanly; Ctrl-C is the
	// only client-side stop.
	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	seconds := uint32((*duration + time.Second - 1) / time.Second)

	stream, err := janusv1alpha1.NewSystemServiceClient(conn).PacketCapture(c, &janusv1alpha1.PacketCaptureRequest{
		Interface:        *iface,
		BpfFilter:        *filter,
		Promiscuous:      *promisc,
		SnapLen:          uint32(*snapLen),
		DurationSeconds:  seconds,
		IncludeOwnStream: *includeOwn,
	})
	if err != nil {
		log.Fatalf("PacketCapture: %v", err)
	}
	var total int
	for {
		msg, err := stream.Recv()
		if err != nil {
			// Our own deadline or Ctrl-C ending the stream is the normal
			// way a capture stops, not a failure.
			if err == io.EOF || c.Err() != nil {
				break
			}
			log.Fatalf("PacketCapture: %v", err)
		}
		if _, err := w.Write(msg.GetBytes()); err != nil {
			log.Fatalf("write: %v", err)
		}
		total += len(msg.GetBytes())
	}
	if *out != "-" {
		fmt.Fprintf(os.Stderr, "wrote %d bytes to %s\n", total, *out)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: janusctl [-endpoint host:port] <command>")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  version                    print janusctl's own version and the connected node's version")
	fmt.Fprintln(os.Stderr, "  system info                print version/kernel/active slot + memory/CPU/load/disk stats (the dashboard's own single-node fetch)")
	fmt.Fprintln(os.Stderr, "  system pcap -i IFACE [-f FILTER] [-promisc] [-include-own-stream] [-snaplen N] [-duration D] [-o FILE]  live packet capture as a pcap file (stdout by default - pipe into tcpdump -r - or wireshark -k -i -); see docs/packet-capture.md")
	for _, line := range systemUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	fmt.Fprintln(os.Stderr, "  network status             optional modules (bird, keepalived, nftables) and whether this image has them")
	fmt.Fprintln(os.Stderr, "  haproxy backends           backends, their servers, addresses and states")
	fmt.Fprintln(os.Stderr, "  haproxy show-info          HAProxy version/uptime/connections (stats socket)")
	fmt.Fprintln(os.Stderr, "  haproxy stats              raw 'show stat' CSV from the stats socket")
	fmt.Fprintln(os.Stderr, "  haproxy get-config         print the currently active haproxy.cfg")
	fmt.Fprintln(os.Stderr, "  haproxy apply-config FILE  validate + apply + seamlessly reload with FILE's contents")
	fmt.Fprintln(os.Stderr, "  haproxy map-list                  list file-backed maps known to the running config")
	fmt.Fprintln(os.Stderr, "  haproxy map-get MAP                dump MAP's key/value entries")
	fmt.Fprintln(os.Stderr, "  haproxy map-set MAP KEY VALUE      upsert one entry in MAP")
	fmt.Fprintln(os.Stderr, "  haproxy map-delete MAP KEY          delete one entry from MAP")
	fmt.Fprintln(os.Stderr, "  haproxy acl-add ACL VALUE           add one pattern value to ACL")
	fmt.Fprintln(os.Stderr, "  haproxy acl-delete ACL VALUE        delete one pattern value from ACL")
	fmt.Fprintln(os.Stderr, "  haproxy cert-list                   list certificates in HAProxy's cert store")
	fmt.Fprintln(os.Stderr, "  haproxy cert-upload [-crt-list PATH] [-sni host1,host2] NAME FILE  upload a PEM cert+key bundle as NAME, optionally binding it into crt-list PATH")
	fmt.Fprintln(os.Stderr, "  haproxy cert-delete [-crt-list PATH] NAME  delete a certificate (unbinding from crt-list PATH first if given)")
	fmt.Fprintln(os.Stderr, "  pki generate-client-config [-role os:admin|os:reader] DIR  issue a new client certificate, write ca.crt/client.crt/client.key to DIR")
	fmt.Fprintln(os.Stderr, "  lifecycle install [-sha256 HEX] [-controller-address HOST:PORT -controller-ca FILE] DISK BUNDLE_DIR  partition a blank DISK from scratch and write a release bundle (image/release/assemble.sh) to both A/B slots - does not reboot anything; -controller-address/-controller-ca make the installed node self-register with that Controller on first boot")
	fmt.Fprintln(os.Stderr, "  lifecycle rollback         switch the ESP to the other A/B slot's staged UKI and reboot into it")
	fmt.Fprintln(os.Stderr, "  lifecycle upgrade [-sha256 HEX] [-wait-for-health] [-health-timeout SECONDS] BUNDLE_DIR  write a release bundle (image/release/assemble.sh) to the inactive slot, switch, and reboot into it - with -wait-for-health, reverts and reboots back automatically if the new slot never stays up long enough to confirm healthy")
	fmt.Fprintln(os.Stderr, "  lifecycle upload-release BUNDLE_DIR  stream a local release bundle's 4 files to this node's own staging storage, for a node that can't dial out to fetch one itself - prints the staging path to pass as BUNDLE_DIR to a later 'lifecycle upgrade'")
	fmt.Fprintln(os.Stderr, "  image seed-controller -controller-address HOST:PORT -controller-ca FILE DISK  write controller self-registration config directly onto an already-built DISK's existing STATE partition - no janusd/gRPC needed, doesn't touch partitioning or the rootfs (raw disk images only; qemu-img convert a qcow2 to raw first, see docs/provisioning-a-node.md)")
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func runVersion(conn *grpc.ClientConn) {
	fmt.Println("Client:", version)

	c, cancel := ctx()
	defer cancel()

	resp, err := janusv1alpha1.NewSystemServiceClient(conn).Version(c, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("Version: %v", err)
	}
	fmt.Println("Node:", resp.GetVersion())
}

// runSystem is the dashboard's own planned single-node fetch, bundled
// into one CLI command for now - see docs/architecture.md's dashboard
// design (Point 2 in the rebranding plan): one HTTP request per node
// view will call the same set of RPCs this prints.
func runSystem(conn *grpc.ClientConn, args []string) {
	if len(args) > 0 && args[0] == "pcap" {
		runPcap(conn, args[1:])
		return
	}
	if len(args) > 0 && args[0] != "info" && runSystemCommand(conn, args[0], args[1:]) {
		return
	}
	if len(args) == 0 || args[0] != "info" {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewSystemServiceClient(conn)

	c, cancel := ctx()
	defer cancel()
	ver, err := client.Version(c, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("Version: %v", err)
	}
	fmt.Printf("version: %s\n", ver.GetVersion())
	fmt.Printf("go version: %s\n", ver.GetGoVersion())
	fmt.Printf("kernel version: %s\n", ver.GetKernelVersion())
	fmt.Printf("active slot: %s\n", ver.GetActiveSlot())

	c, cancel = ctx()
	defer cancel()
	mem, err := client.Memory(c, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("Memory: %v", err)
	}
	fmt.Printf("memory: total=%d available=%d cached=%d bytes\n", mem.GetTotalBytes(), mem.GetAvailableBytes(), mem.GetCachedBytes())

	c, cancel = ctx()
	defer cancel()
	cpu, err := client.CPUInfo(c, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("CPUInfo: %v", err)
	}
	fmt.Printf("cpus: %d\n", len(cpu.GetCpus()))
	for _, info := range cpu.GetCpus() {
		fmt.Printf("  cpu%d: %s (%.0f MHz)\n", info.GetProcessor(), info.GetModelName(), info.GetMhz())
	}

	c, cancel = ctx()
	defer cancel()
	load, err := client.LoadAvg(c, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("LoadAvg: %v", err)
	}
	fmt.Printf("load average: %.2f %.2f %.2f\n", load.GetLoad1(), load.GetLoad5(), load.GetLoad15())

	c, cancel = ctx()
	defer cancel()
	disks, err := client.DiskStats(c, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("DiskStats: %v", err)
	}
	fmt.Printf("disks: %d\n", len(disks.GetDisks()))
	for _, d := range disks.GetDisks() {
		fmt.Printf("  %s: reads=%d writes=%d\n", d.GetDeviceName(), d.GetReadCompleted(), d.GetWriteCompleted())
	}
}

func runPKI(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch sub := args[0]; sub {
	case "generate-client-config":
		fs := flag.NewFlagSet("pki generate-client-config", flag.ExitOnError)
		role := fs.String("role", "os:admin", "role to request (os:admin or os:reader - see internal/api/authz.go)")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl pki generate-client-config [-role os:admin|os:reader] DIR")
			os.Exit(2)
		}
		dir := fs.Arg(0)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatalf("mkdir %s: %v", dir, err)
		}

		c, cancel := ctx()
		defer cancel()
		resp, err := janusv1alpha1.NewSystemServiceClient(conn).GenerateClientConfiguration(c, &janusv1alpha1.GenerateClientConfigurationRequest{
			Roles: []string{*role},
		})
		if err != nil {
			log.Fatalf("GenerateClientConfiguration: %v", err)
		}

		for name, data := range map[string][]byte{"ca.crt": resp.GetCa(), "client.crt": resp.GetCrt(), "client.key": resp.GetKey()} {
			mode := os.FileMode(0o644)
			if name == "client.key" {
				mode = 0o600
			}
			if err := os.WriteFile(dir+"/"+name, data, mode); err != nil {
				log.Fatalf("write %s: %v", name, err)
			}
		}
		fmt.Printf("Wrote %s/{ca.crt,client.crt,client.key}\n", dir)

	default:
		fmt.Fprintf(os.Stderr, "janusctl pki: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}

func runLifecycle(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch sub := args[0]; sub {
	case "install":
		fs := flag.NewFlagSet("lifecycle install", flag.ExitOnError)
		sha256Flag := fs.String("sha256", "", "expected sha256 of BUNDLE_DIR/rootfs.squashfs (defaults to reading BUNDLE_DIR/rootfs.squashfs.sha256, if present - see image/release/assemble.sh)")
		controllerAddress := fs.String("controller-address", "", "address of a Controller (Janus Controller's node self-registration port, see dashboard/backend/register.go) for the installed node to announce itself to on first boot - if unset, the node never self-registers. Requires -controller-ca.")
		controllerCA := fs.String("controller-ca", "", "path to the Controller's CA certificate (PEM) - the installed node uses this to verify it's talking to the real Controller before ever sending it a credential; required whenever -controller-address is set")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "usage: janusctl lifecycle install [-sha256 HEX] [-controller-address HOST:PORT -controller-ca FILE] DISK BUNDLE_DIR")
			os.Exit(2)
		}
		disk, bundleDir := fs.Arg(0), fs.Arg(1)
		sum := *sha256Flag
		if sum == "" {
			if data, err := os.ReadFile(filepath.Join(bundleDir, "rootfs.squashfs.sha256")); err == nil {
				sum = strings.TrimSpace(string(data))
			}
		}
		var controllerCACert []byte
		if *controllerAddress != "" {
			if *controllerCA == "" {
				log.Fatalf("Install: -controller-ca is required whenever -controller-address is set")
			}
			data, err := os.ReadFile(*controllerCA)
			if err != nil {
				log.Fatalf("Install: read -controller-ca %s: %v", *controllerCA, err)
			}
			controllerCACert = data
		}

		// Longer than Upgrade's own 60s - Install writes the full
		// rootfs to *both* A/B slots plus builds the ESP and STATE
		// filesystems from scratch, more work than Upgrade's single-
		// slot raw writes.
		c, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		stream, err := janusv1alpha1.NewLifecycleServiceClient(conn).Install(c, &janusv1alpha1.InstallRequest{
			Source:            &janusv1alpha1.ImageSource{Reference: bundleDir, Sha256: sum},
			Disk:              disk,
			ControllerAddress: *controllerAddress,
			ControllerCaCert:  controllerCACert,
		})
		if err != nil {
			log.Fatalf("Install: %v", err)
		}
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Fatalf("Install: %v", err)
			}
			fmt.Printf("[%s %.0f%%] %s\n", resp.GetStage(), resp.GetProgress()*100, resp.GetMessage())
		}

	case "rollback":
		c, cancel := ctx()
		defer cancel()
		resp, err := janusv1alpha1.NewLifecycleServiceClient(conn).Rollback(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("Rollback: %v", err)
		}
		fmt.Printf("Rolling back to slot %s, node is rebooting\n", resp.GetActiveSlot())

	case "upgrade":
		fs := flag.NewFlagSet("lifecycle upgrade", flag.ExitOnError)
		sha256Flag := fs.String("sha256", "", "expected sha256 of BUNDLE_DIR/rootfs.squashfs (defaults to reading BUNDLE_DIR/rootfs.squashfs.sha256, if present - see image/release/assemble.sh)")
		waitForHealth := fs.Bool("wait-for-health", false, "revert and reboot back to the current slot automatically if the new slot doesn't stay up long enough to confirm healthy (see -health-timeout) - the revert itself happens on the node, not over this call, which still returns as soon as it reboots")
		healthTimeout := fs.Uint("health-timeout", 0, "seconds the new slot's janusd has to keep running before it's considered healthy and the upgrade is confirmed; 0 uses the node's own default (60s) - only meaningful with -wait-for-health")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl lifecycle upgrade [-sha256 HEX] [-wait-for-health] [-health-timeout SECONDS] BUNDLE_DIR")
			os.Exit(2)
		}
		bundleDir := fs.Arg(0)
		sum := *sha256Flag
		if sum == "" {
			if data, err := os.ReadFile(filepath.Join(bundleDir, "rootfs.squashfs.sha256")); err == nil {
				sum = strings.TrimSpace(string(data))
			}
		}

		// Longer than ctx()'s default 10s - Upgrade writes the new
		// rootfs.squashfs/rootfs.verity to a partition device directly,
		// still fast for this project's image sizes, but no reason to
		// cut it close. Not related to -health-timeout at all: this
		// call returns once the node reboots, well before any health
		// confirmation (or possible revert) happens on a later boot.
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		stream, err := janusv1alpha1.NewLifecycleServiceClient(conn).Upgrade(c, &janusv1alpha1.UpgradeRequest{
			Source:               &janusv1alpha1.ImageSource{Reference: bundleDir, Sha256: sum},
			WaitForHealth:        *waitForHealth,
			HealthTimeoutSeconds: uint32(*healthTimeout),
		})
		if err != nil {
			log.Fatalf("Upgrade: %v", err)
		}
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Fatalf("Upgrade: %v", err)
			}
			fmt.Printf("[%s %.0f%%] %s\n", resp.GetStage(), resp.GetProgress()*100, resp.GetMessage())
		}

	case "upload-release":
		fs := flag.NewFlagSet("lifecycle upload-release", flag.ExitOnError)
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl lifecycle upload-release BUNDLE_DIR")
			os.Exit(2)
		}
		bundleDir := fs.Arg(0)

		client := janusv1alpha1.NewLifecycleServiceClient(conn)
		const chunkSize = 512 * 1024
		var stagingDir string
		for _, name := range []string{"rootfs.squashfs", "rootfs.verity", "uki-a.efi", "uki-b.efi"} {
			path := filepath.Join(bundleDir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				log.Fatalf("UploadReleaseFile: read %s: %v", path, err)
			}

			c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			stream, err := client.UploadReleaseFile(c)
			if err != nil {
				cancel()
				log.Fatalf("UploadReleaseFile: %s: %v", name, err)
			}

			// A Send() failure mid-stream (e.g. the server rejecting an
			// oversized file and closing its own end early) only ever
			// surfaces as a generic io.EOF here - the real status error
			// is only retrievable via CloseAndRecv(), never from Send()
			// itself (a real, easy-to-miss gRPC client-streaming
			// gotcha, confirmed by a real oversized-file test producing
			// exactly this: Send returned a bare "EOF" while the
			// server's own actual FailedPrecondition message was only
			// visible via CloseAndRecv). So a Send() error here just
			// stops sending further chunks and falls through to the
			// same CloseAndRecv() call below, rather than reporting the
			// meaningless EOF directly.
			sentFilename := false
			for offset := 0; offset < len(data); offset += chunkSize {
				req := &janusv1alpha1.UploadReleaseFileRequest{Chunk: data[offset:min(offset+chunkSize, len(data))]}
				if !sentFilename {
					req.Filename = name
					sentFilename = true
				}
				if err := stream.Send(req); err != nil {
					break
				}
			}
			if !sentFilename {
				_ = stream.Send(&janusv1alpha1.UploadReleaseFileRequest{Filename: name})
			}

			resp, err := stream.CloseAndRecv()
			cancel()
			if err != nil {
				log.Fatalf("UploadReleaseFile: %s: %v", name, err)
			}
			stagingDir = resp.GetStagingDir()
			fmt.Printf("Uploaded %s (%d bytes)\n", name, resp.GetBytesWritten())
		}
		fmt.Printf("Release bundle staged at %s - pass this as the source to 'janusctl lifecycle upgrade'\n", stagingDir)

	default:
		fmt.Fprintf(os.Stderr, "janusctl lifecycle: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}

// runImage handles "image" subcommands - unlike every other command in
// this file, these never take a *grpc.ClientConn: they operate directly
// on a disk file offline, no janusd involved at all (see
// internal/diskseed's own package doc for why this exists).
func runImage(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch sub := args[0]; sub {
	case "seed-controller":
		fs := flag.NewFlagSet("image seed-controller", flag.ExitOnError)
		controllerAddress := fs.String("controller-address", "", "address of a Controller (Janus Controller's node self-registration port, see dashboard/backend/register.go) for the node to announce itself to on first boot - required")
		controllerCA := fs.String("controller-ca", "", "path to the Controller's CA certificate (PEM) - the node uses this to verify it's talking to the real Controller before ever sending it a credential; required")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 || *controllerAddress == "" || *controllerCA == "" {
			fmt.Fprintln(os.Stderr, "usage: janusctl image seed-controller -controller-address HOST:PORT -controller-ca FILE DISK")
			os.Exit(2)
		}
		disk := fs.Arg(0)
		caCert, err := os.ReadFile(*controllerCA)
		if err != nil {
			log.Fatalf("seed-controller: read -controller-ca %s: %v", *controllerCA, err)
		}
		if err := diskseed.SeedController(disk, *controllerAddress, caCert); err != nil {
			log.Fatalf("seed-controller: %v", err)
		}
		fmt.Printf("wrote controller self-registration config to %s's STATE partition\n", disk)

	default:
		fmt.Fprintf(os.Stderr, "janusctl image: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}

func runHAProxy(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewHAProxyServiceClient(conn)

	switch sub := args[0]; sub {
	case "show-info":
		c, cancel := ctx()
		defer cancel()
		info, err := client.ShowInfo(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("ShowInfo: %v", err)
		}
		fmt.Printf("Version:      %s\n", info.GetVersion())
		fmt.Printf("Uptime:       %ds\n", info.GetUptimeSeconds())
		fmt.Printf("Connections:  %d / %d\n", info.GetCurrentConnections(), info.GetMaxConnections())

	case "stats":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.Stats(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("Stats: %v", err)
		}
		os.Stdout.Write(resp.GetRawCsv())

	case "backends":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.BackendList(c, &emptypb.Empty{})
		check("BackendList", err)
		tw := table("BACKEND", "SERVER", "ADDRESS", "STATE")
		for _, b := range resp.GetBackends() {
			if len(b.GetServers()) == 0 {
				fmt.Fprintf(tw, "%s	-	-	-\n", b.GetName())
			}
			for _, sv := range b.GetServers() {
				fmt.Fprintf(tw, "%s	%s	%s	%s\n", b.GetName(), sv.GetName(), sv.GetAddress(), sv.GetState())
			}
		}
		tw.Flush()

	case "get-config":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.GetConfig(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("GetConfig: %v", err)
		}
		os.Stdout.Write(resp.GetConfig())

	case "apply-config":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy apply-config FILE")
			os.Exit(2)
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			log.Fatalf("read %s: %v", args[1], err)
		}

		c, cancel := ctx()
		defer cancel()
		stream, err := client.ApplyConfig(c, &janusv1alpha1.ApplyConfigRequest{Config: data})
		if err != nil {
			log.Fatalf("ApplyConfig: %v", err)
		}
		var last *janusv1alpha1.ApplyConfigResponse
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Fatalf("ApplyConfig: %v", err)
			}
			fmt.Printf("[%s] %s\n", resp.GetStage(), resp.GetMessage())
			last = resp
		}
		if last != nil && !last.GetAccepted() {
			os.Exit(1)
		}

	case "map-list":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.MapList(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("MapList: %v", err)
		}
		for _, name := range resp.GetMaps() {
			fmt.Println(name)
		}

	case "map-get":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy map-get MAP")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		resp, err := client.MapGet(c, &janusv1alpha1.MapGetRequest{Map: args[1]})
		if err != nil {
			log.Fatalf("MapGet: %v", err)
		}
		for k, v := range resp.GetEntries() {
			fmt.Printf("%s %s\n", k, v)
		}

	case "map-set":
		if len(args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy map-set MAP KEY VALUE")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.MapUpdate(c, &janusv1alpha1.MapUpdateRequest{Map: args[1], Key: args[2], Value: args[3]})
		if err != nil {
			log.Fatalf("MapUpdate: %v", err)
		}

	case "map-delete":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy map-delete MAP KEY")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.MapUpdate(c, &janusv1alpha1.MapUpdateRequest{Map: args[1], Key: args[2], Delete: true})
		if err != nil {
			log.Fatalf("MapUpdate: %v", err)
		}

	case "acl-add":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy acl-add ACL VALUE")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.ACLUpdate(c, &janusv1alpha1.ACLUpdateRequest{Acl: args[1], Value: args[2]})
		if err != nil {
			log.Fatalf("ACLUpdate: %v", err)
		}

	case "acl-delete":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy acl-delete ACL VALUE")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.ACLUpdate(c, &janusv1alpha1.ACLUpdateRequest{Acl: args[1], Value: args[2], Delete: true})
		if err != nil {
			log.Fatalf("ACLUpdate: %v", err)
		}

	case "cert-list":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.CertificateList(c, &emptypb.Empty{})
		if err != nil {
			log.Fatalf("CertificateList: %v", err)
		}
		for _, cert := range resp.GetCertificates() {
			fmt.Printf("%s\tnotAfter=%s\tstatus=%s\n", cert.GetName(), cert.GetNotAfter(), cert.GetStatus())
		}

	case "cert-upload":
		fs := flag.NewFlagSet("haproxy cert-upload", flag.ExitOnError)
		crtList := fs.String("crt-list", "", "bind into this crt-list (a 'bind ... ssl crt-list <path>' already in the running config) - leave empty to only upload, not bind")
		sni := fs.String("sni", "", "comma-separated SNI names to scope the binding to (only meaningful with -crt-list)")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy cert-upload [-crt-list PATH] [-sni host1,host2] NAME FILE")
			os.Exit(2)
		}
		name, file := fs.Arg(0), fs.Arg(1)
		data, err := os.ReadFile(file)
		if err != nil {
			log.Fatalf("read %s: %v", file, err)
		}
		var sniList []string
		if *sni != "" {
			sniList = strings.Split(*sni, ",")
		}
		c, cancel := ctx()
		defer cancel()
		_, err = client.CertificateUpload(c, &janusv1alpha1.CertificateUploadRequest{
			Name: name, PemBundle: data, CrtList: *crtList, Sni: sniList,
		})
		if err != nil {
			log.Fatalf("CertificateUpload: %v", err)
		}

	case "cert-delete":
		fs := flag.NewFlagSet("haproxy cert-delete", flag.ExitOnError)
		crtList := fs.String("crt-list", "", "unbind from this crt-list before deleting - required if the certificate is still bound anywhere")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy cert-delete [-crt-list PATH] NAME")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		_, err := client.CertificateDelete(c, &janusv1alpha1.CertificateDeleteRequest{Name: fs.Arg(0), CrtList: *crtList})
		if err != nil {
			log.Fatalf("CertificateDelete: %v", err)
		}

	default:
		fmt.Fprintf(os.Stderr, "janusctl haproxy: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}
