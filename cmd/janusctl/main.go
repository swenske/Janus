// Command janusctl is the Janus admin CLI, a thin client over the
// gRPC API served by janusd (api/proto/janus/v1alpha1), always
// over mTLS (internal/pki) - there is no insecure fallback. Command
// surface grows alongside its corresponding service implementation (see
// docs/architecture.md's roadmap).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/diskseed"
	"github.com/swenske/Janus/internal/netconfig"
	"github.com/swenske/Janus/internal/pki"
)

var version = "dev"

// actingFor is the metadata through which a Controller certificate says
// whom it acts for (-as-user, -as-roles; internal/api/authz.go).
var actingFor metadata.MD

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
	asUser := flag.String("as-user", "", "with a Controller certificate (role janus:controller): the user the calls are made for")
	asRoles := flag.String("as-roles", "", "with -as-user: that user's roles, comma-separated (os:admin, os:operator, os:reader)")
	ctxFlag := flag.String("context", "", "the context to use (janusctl login; default: the current one)")
	nodesFlag := flag.String("n", "", "with a context: the node(s) to run the command on, by name or ID, comma-separated")
	allFlag := flag.Bool("all", false, "with a context: run the command on every node of the fleet")
	flag.Parse()
	if *asUser != "" || *asRoles != "" {
		actingFor = metadata.Pairs(pki.AsUserKey, *asUser, pki.AsRolesKey, *asRoles)
	}

	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}

	// "image" subcommands operate directly on a disk file offline -
	// unlike every other command here, they need no running janusd/gRPC
	// connection at all (that's the entire point, see internal/diskseed's
	// own package doc) - dispatched before dial() so a missing/
	// unreachable -endpoint never gets in the way.
	switch flag.Arg(0) {
	case "image":
		runImage(flag.Args()[1:])
		return
	case "login":
		runLogin(flag.Args()[1:])
		return
	case "context":
		runContext(flag.Args()[1:])
		return
	case "nodes":
		runNodes(*ctxFlag)
		return
	}

	// A context (janusctl login) unless the node's own certificate is
	// given: -endpoint/-ca/-cert/-key.
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	redial := func(ep string) (*grpc.ClientConn, error) { return dial(ep, *caFile, *certFile, *keyFile) }
	if !explicit["endpoint"] && !explicit["ca"] && !explicit["cert"] && !explicit["key"] {
		cfg, err := loadConfig()
		if err != nil {
			log.Fatal(err)
		}
		if name, ctx := currentContext(cfg, *ctxFlag); ctx != nil {
			nodes, err := selectNodes(ctx, *nodesFlag, *allFlag)
			if err != nil {
				log.Fatalf("janusctl: %v", err)
			}
			if err := fresh(cfg, name, ctx); err != nil {
				log.Fatalf("janusctl: %v", err)
			}
			if len(nodes) > 1 {
				runOnEach(name, nodes, flag.Args())
				return
			}
			node := nodes[0]
			tlsConfig, err := nodeTLS(name, node)
			if err != nil {
				log.Fatalf("janusctl: %v", err)
			}
			*endpoint = node.Address
			redial = func(ep string) (*grpc.ClientConn, error) { return dialTLS(ep, tlsConfig) }
		} else if *ctxFlag != "" || *nodesFlag != "" || *allFlag {
			log.Fatal("janusctl: no context - janusctl login -controller HOST")
		}
	}

	// janusctl's own version first: shown even when no node is
	// reachable (checking an installed package).
	if flag.Arg(0) == "version" {
		fmt.Println("Client:", version)
	}

	conn, err := redial(*endpoint)
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
	case "access":
		runAccess(conn, flag.Args()[1:])
	case "lifecycle":
		runLifecycle(conn, flag.Args()[1:])
	case "network":
		runNetwork(conn, *endpoint, redial, flag.Args()[1:])
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
	return dialTLS(endpoint, tlsConfig)
}

// dialTLS connects to a node with tlsConfig.
func dialTLS(endpoint string, tlsConfig *tls.Config) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig.Clone()))}
	if actingFor != nil {
		opts = append(opts,
			grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, o ...grpc.CallOption) error {
				return invoker(metadata.NewOutgoingContext(ctx, actingFor), method, req, reply, cc, o...)
			}),
			grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, o ...grpc.CallOption) (grpc.ClientStream, error) {
				return streamer(metadata.NewOutgoingContext(ctx, actingFor), desc, cc, method, o...)
			}))
	}
	conn, err := grpc.NewClient(endpoint, opts...)
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
	fmt.Fprintln(os.Stderr, "usage: janusctl [-context NAME] [-n NODE[,NODE...] | -all] <command>        with a context (janusctl login)")
	fmt.Fprintln(os.Stderr, "       janusctl -endpoint host:port -ca FILE -cert FILE -key FILE <command>   with a node's own certificate")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  login [-context NAME] [-controller HOST[:PORT]] [-controller-ca FILE | -controller-fingerprint SHA256]  sign in to a Controller with the API token in JANUS_TOKEN: a certificate of its fleet for your account (an hour), and its nodes")
	fmt.Fprintln(os.Stderr, "  context [list | use NAME | delete NAME]  the Controllers signed in to")
	fmt.Fprintln(os.Stderr, "  nodes                      the context's nodes (refreshed with JANUS_TOKEN)")
	fmt.Fprintln(os.Stderr, "  version                    print janusctl's own version and the connected node's version")
	fmt.Fprintln(os.Stderr, "  system info                print version/kernel/active slot + memory/CPU/load/disk stats (the dashboard's own single-node fetch)")
	fmt.Fprintln(os.Stderr, "  system pcap -i IFACE [-f FILTER] [-promisc] [-include-own-stream] [-snaplen N] [-duration D] [-o FILE]  live packet capture as a pcap file (stdout by default - pipe into tcpdump -r - or wireshark -k -i -); see docs/packet-capture.md")
	for _, line := range systemUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	fmt.Fprintln(os.Stderr, "  network status             hostname, interfaces, addresses, boot DHCP lease, routes, DNS, clock synchronization")
	fmt.Fprintln(os.Stderr, "  network get                the network configuration, as JSON (the format apply, Install and NoCloud take)")
	fmt.Fprintln(os.Stderr, "  network apply [-timeout 30s] [-no-confirm] FILE  apply a configuration on trial, then confirm it over the node's new address - unconfirmed, the node reverts by itself")
	fmt.Fprintln(os.Stderr, "  network confirm            confirm the configuration on trial (over an address it keeps)")
	fmt.Fprintln(os.Stderr, "  network modules            optional modules (bird, keepalived, nftables) and whether this image has them")
	for _, line := range firewallUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	for _, line := range vrrpUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	for _, line := range bgpUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	for _, line := range consulUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
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
	for _, line := range haproxyFilesUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	for _, line := range acmeUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	for _, line := range accessUsage {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	fmt.Fprintln(os.Stderr, "  pki generate-client-config [-role os:admin|os:reader] [-name NAME] [-ttl DURATION] DIR  issue a new client certificate (named NAME, valid DURATION - one year at most), write ca.crt/client.crt/client.key to DIR")
	fmt.Fprintln(os.Stderr, "  lifecycle install [-sha256 HEX] [-controller-address HOST:PORT -controller-ca FILE] [-network-config FILE] [-insecure-skip-signature-check] DISK BUNDLE_DIR  partition a blank DISK from scratch and write a release bundle (image/release/assemble.sh) to both A/B slots - does not reboot anything; -controller-address/-controller-ca make the installed node self-register with that Controller on first boot")
	fmt.Fprintln(os.Stderr, "  lifecycle rollback         switch the ESP to the other A/B slot's staged UKI and reboot into it")
	fmt.Fprintln(os.Stderr, "  lifecycle upgrade [-sha256 HEX] [-wait-for-health] [-health-timeout SECONDS] [-insecure-skip-signature-check] [-allow-schematic-change] BUNDLE_DIR  write a release bundle (image/release/assemble.sh), whose UKIs must be signed by a Janus release key, to the inactive slot, switch, and reboot into it - with -wait-for-health, reverts and reboots back automatically if the new slot never stays up long enough to confirm healthy")
	fmt.Fprintln(os.Stderr, "  lifecycle upload-release BUNDLE_DIR  stream a local release bundle's 4 files to this node's own staging storage, for a node that can't dial out to fetch one itself - prints the staging path to pass as BUNDLE_DIR to a later 'lifecycle upgrade'")
	fmt.Fprintln(os.Stderr, "  image seed-network -config FILE DISK  write a network configuration onto an already-built DISK's STATE partition, offline, applied from the node's first boot (raw disk images only)")
	fmt.Fprintln(os.Stderr, "  image seed-controller -controller-address HOST:PORT -controller-ca FILE DISK  write controller self-registration config directly onto an already-built DISK's existing STATE partition - no janusd/gRPC needed, doesn't touch partitioning or the rootfs (raw disk images only; qemu-img convert a qcow2 to raw first, see docs/provisioning-a-node.md)")
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func runVersion(conn *grpc.ClientConn) {
	c, cancel := ctx()
	defer cancel()

	resp, err := janusv1alpha1.NewSystemServiceClient(conn).Version(c, &emptypb.Empty{})
	if err != nil {
		log.Fatalf("Version: %v", err)
	}
	fmt.Println("Node:", resp.GetVersion())
	if resp.GetArch() != "" {
		fmt.Println("Architecture:", resp.GetArch())
	}
	if resp.GetSchematicId() != "" {
		fmt.Println("Image schematic:", resp.GetSchematicId())
	}
	for _, e := range resp.GetExtensions() {
		fmt.Printf("Extension: %s %s\n", e.GetName(), e.GetVersion())
	}
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
		name := fs.String("name", "", "who or what the certificate is for, its common name (e.g. alice-laptop) - default: client")
		ttl := fs.Duration("ttl", 0, "how long the certificate is valid, e.g. 12h or 720h - default and most: one year")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl pki generate-client-config [-role os:admin|os:reader] [-name NAME] [-ttl DURATION] DIR")
			os.Exit(2)
		}
		if *ttl < 0 || *ttl%time.Second != 0 || *ttl > math.MaxUint32*time.Second {
			log.Fatalf("-ttl %s: a whole number of seconds, one year at most", *ttl)
		}
		dir := fs.Arg(0)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatalf("mkdir %s: %v", dir, err)
		}

		c, cancel := ctx()
		defer cancel()
		resp, err := janusv1alpha1.NewSystemServiceClient(conn).GenerateClientConfiguration(c, &janusv1alpha1.GenerateClientConfigurationRequest{
			Roles:      []string{*role},
			Name:       *name,
			TtlSeconds: uint32(*ttl / time.Second),
		})
		if err != nil {
			log.Fatalf("GenerateClientConfiguration: %v", err)
		}
		if err := pki.CheckIssued(resp.GetCrt(), *name, *ttl); err != nil {
			log.Fatalf("GenerateClientConfiguration: %v (nothing written)", err)
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
		controllerFleetRoot := fs.String("controller-fleet-root", "", "path to the Controller's fleet root (PEM, its Provision panel) - optional: the node then checks the Controller through its fleet first, and takes only that fleet's trust")
		networkConfig := fs.String("network-config", "", "path to a network configuration (JSON, as `janusctl network get` prints it) the installed node applies from its first boot - default: kernel boot DHCP")
		insecureSkip := fs.Bool("insecure-skip-signature-check", false, "accept UKIs not signed by a Janus release key - development bundles only: without the check, whoever can alter the bundle on its way to the node controls what it boots")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "usage: janusctl lifecycle install [-sha256 HEX] [-controller-address HOST:PORT -controller-ca FILE [-controller-fleet-root FILE]] [-network-config FILE] [-insecure-skip-signature-check] DISK BUNDLE_DIR")
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
		var fleetRoot []byte
		if *controllerFleetRoot != "" {
			data, err := os.ReadFile(*controllerFleetRoot)
			if err != nil {
				log.Fatalf("Install: read -controller-fleet-root %s: %v", *controllerFleetRoot, err)
			}
			fleetRoot = data
		}
		var netCfg *janusv1alpha1.NetworkConfig
		if *networkConfig != "" {
			data, err := os.ReadFile(*networkConfig)
			if err != nil {
				log.Fatalf("Install: read -network-config: %v", err)
			}
			if netCfg, err = netconfig.Parse(data); err != nil {
				log.Fatalf("Install: -network-config %s: %v", *networkConfig, err)
			}
		}

		// Longer than Upgrade's own 60s - Install writes the full
		// rootfs to *both* A/B slots plus builds the ESP and STATE
		// filesystems from scratch, more work than Upgrade's single-
		// slot raw writes.
		c, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		stream, err := janusv1alpha1.NewLifecycleServiceClient(conn).Install(c, &janusv1alpha1.InstallRequest{
			Source:                  &janusv1alpha1.ImageSource{Reference: bundleDir, Sha256: sum, InsecureSkipSignatureCheck: *insecureSkip},
			Disk:                    disk,
			ControllerAddress:       *controllerAddress,
			ControllerCaCert:        controllerCACert,
			ControllerFleetRootCert: fleetRoot,
			NetworkConfig:           netCfg,
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
		insecureSkip := fs.Bool("insecure-skip-signature-check", false, "accept UKIs not signed by a Janus release key - development bundles only: without the check, whoever can alter the bundle on its way to the node controls what it boots")
		allowSchematic := fs.Bool("allow-schematic-change", false, "accept a bundle built from another image schematic than the node's own - without it, an update that would drop the node's extensions is refused")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl lifecycle upgrade [-sha256 HEX] [-wait-for-health] [-health-timeout SECONDS] [-insecure-skip-signature-check] [-allow-schematic-change] BUNDLE_DIR")
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
			Source:               &janusv1alpha1.ImageSource{Reference: bundleDir, Sha256: sum, InsecureSkipSignatureCheck: *insecureSkip, AllowSchematicChange: *allowSchematic},
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
		controllerFleetRoot := fs.String("controller-fleet-root", "", "path to the Controller's fleet root (PEM) - optional: the node checks the Controller through its fleet first")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 || *controllerAddress == "" || *controllerCA == "" {
			fmt.Fprintln(os.Stderr, "usage: janusctl image seed-controller -controller-address HOST:PORT -controller-ca FILE [-controller-fleet-root FILE] DISK")
			os.Exit(2)
		}
		disk := fs.Arg(0)
		caCert, err := os.ReadFile(*controllerCA)
		if err != nil {
			log.Fatalf("seed-controller: read -controller-ca %s: %v", *controllerCA, err)
		}
		var fleetRoot []byte
		if *controllerFleetRoot != "" {
			if fleetRoot, err = os.ReadFile(*controllerFleetRoot); err != nil {
				log.Fatalf("seed-controller: read -controller-fleet-root %s: %v", *controllerFleetRoot, err)
			}
		}
		if err := diskseed.SeedController(disk, *controllerAddress, caCert, fleetRoot); err != nil {
			log.Fatalf("seed-controller: %v", err)
		}
		fmt.Printf("wrote controller self-registration config to %s's STATE partition\n", disk)

	case "seed-network":
		fs := flag.NewFlagSet("image seed-network", flag.ExitOnError)
		config := fs.String("config", "", "path to the network configuration (JSON, as `janusctl network get` prints it) - required")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 || *config == "" {
			fmt.Fprintln(os.Stderr, "usage: janusctl image seed-network -config FILE DISK")
			os.Exit(2)
		}
		data, err := os.ReadFile(*config)
		if err != nil {
			log.Fatalf("seed-network: read -config: %v", err)
		}
		cfg, err := netconfig.Parse(data)
		if err != nil {
			log.Fatalf("seed-network: %s: %v", *config, err)
		}
		if err := diskseed.SeedNetwork(fs.Arg(0), cfg); err != nil {
			log.Fatalf("seed-network: %v", err)
		}
		fmt.Printf("wrote the network configuration to %s's STATE partition\n", fs.Arg(0))

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

	if runHAProxyFiles(client, args[0], args[1:]) {
		return
	}
	switch sub := args[0]; sub {
	case "acme":
		runACME(conn, args[1:])

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
