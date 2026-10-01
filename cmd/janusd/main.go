// Command janusd is the Janus control-plane daemon: it serves the
// SystemService/LifecycleService/HAProxyService/NetworkService gRPC API
// (see api/proto/janus/v1alpha1) that janusctl and any external
// controller use instead of SSH.
//
// mTLS is mandatory on every connection (internal/pki) - there is no
// plaintext or unauthenticated mode - and every RPC is role-checked
// against the calling certificate's roles (internal/api's
// UnaryAuthInterceptor/StreamAuthInterceptor, see internal/api/authz.go
// for the actual os:admin/os:reader split). On first boot (empty
// -pki-dir) a CA, this node's server certificate, and an initial admin
// client certificate are generated and the admin certificate/key are
// printed once, since there's no shell to retrieve them from later (see
// docs/architecture.md's "no shell" design goal) - copy them somewhere
// safe immediately. Phase 3's Install flow will replace this with a
// proper side channel.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/api"
	"github.com/swenske/Janus/internal/bgp"
	"github.com/swenske/Janus/internal/bootcommit"
	"github.com/swenske/Janus/internal/bootrevert"
	"github.com/swenske/Janus/internal/consoledrain"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/exporter"
	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/firewall"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/kmsgwatch"
	"github.com/swenske/Janus/internal/netconfig"
	"github.com/swenske/Janus/internal/netmgr"
	"github.com/swenske/Janus/internal/nodeexporter"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/ring"
	"github.com/swenske/Janus/internal/selfregister"
	"github.com/swenske/Janus/internal/timesync"
	"github.com/swenske/Janus/internal/vrrp"
)

// version is set via -ldflags "-X main.version=..." by the release build
// (see Makefile), left as "dev" for local builds.
var version = "dev"

// serviceLogLines is how many lines of each service's output
// SystemService.Logs keeps in memory.
const serviceLogLines = 5000

func main() {
	showVersion := flag.Bool("version", false, "print the daemon version and exit")
	addr := flag.String("addr", ":9505", "gRPC listen address")
	pkiDir := flag.String("pki-dir", "/etc/janus/pki", "directory holding the node's CA/server/admin certificates (generated here on first boot)")
	haproxyBin := flag.String("haproxy-binary", "/usr/local/sbin/haproxy", "path to the haproxy binary")
	haproxyCfg := flag.String("haproxy-config", "/etc/haproxy/haproxy.cfg", "path to haproxy's active config file")
	haproxyPid := flag.String("haproxy-pid", "/run/janus/haproxy.pid", "path to haproxy's pid file")
	haproxySock := flag.String("haproxy-stats-socket", "/run/janus/haproxy-admin.sock", "path to haproxy's stats socket (must match the 'stats socket' line in haproxy-config)")
	haproxyCertStore := flag.String("haproxy-cert-store", "", "where certificates uploaded at runtime are kept, to be put back into HAProxy after each reload or restart (default: runtime-certs next to -haproxy-config - STATE's haproxy/ on a node)")
	haproxyDrain := flag.Duration("haproxy-drain", 2*time.Second, "with the keepalived or bird extension, how long a deliberate stop of HAProxy (service stop, reboot, shutdown, update, rollback) waits between giving up the virtual IPs and anycast routes and closing its listeners - the traffic moves to another node meanwhile")
	haproxyChrootDir := flag.String("haproxy-chroot-dir", "/var/empty", "directory haproxy chroots into after binding listeners and dropping privileges (must match the 'chroot' line in haproxy-config); created here since this rootfs has no package manager to have provisioned it")
	manageHost := flag.Bool("manage-host", false, "this janusd runs a Janus node: it configures the node's network, hostname and clock (internal/netmgr, internal/timesync). Off, they're only reported - never set this on a machine whose network janusd mustn't touch")
	configDir := flag.String("config-dir", exporter.Dir, "directory for janusd's persistent settings (the exporter's, the optional modules'); a node's is on STATE")
	flag.Parse()
	started := time.Now()
	exporter.Dir = *configDir
	nodeexporter.Dir = *configDir

	if *showVersion {
		fmt.Println("janusd " + version)
		return
	}

	// Captured for SystemService.Logs from the start - except the
	// one-time PKI print below, so the admin private key never sits in
	// memory where an API client could read it back.
	serviceLogs := map[string]*ring.Ring[string]{
		"janusd":  ring.New[string](serviceLogLines),
		"haproxy": ring.New[string](serviceLogLines),
	}
	captured := io.MultiWriter(os.Stderr, &ring.LineWriter{Ring: serviceLogs["janusd"]})
	log.SetOutput(captured)

	// The firewall (the nftables extension) before anything listens: the
	// window where the node is up without its saved ruleset is the boot
	// itself, until here.
	firewall.Dir = *configDir
	fwMgr := firewall.New()
	if *manageHost {
		if err := fwMgr.Boot(); err != nil {
			log.Printf("firewall: %v - the node runs without its saved ruleset", err)
		}
	}

	// Network, hostname and clock come first: the node's certificates
	// are issued for its hostname and addresses, and dated by its clock.
	var serverCert *pki.ServerCert // set once the PKI is loaded
	var timeSvc *timesync.Service
	netMgr := netmgr.New(netmgr.Options{
		Manage: *manageHost,
		OnChange: func() {
			// The addresses or hostname may have changed: the server
			// certificate must cover them, and the NTP servers may have.
			if serverCert != nil {
				refreshServerCert(serverCert)
			}
			if timeSvc != nil {
				timeSvc.Kick()
			}
		},
	})
	if err := netMgr.Start(); err != nil {
		log.Printf("netmgr: the stored network configuration can't apply (%v) - running on the defaults this boot", err)
		events.Publish("network.fallback", map[string]string{"error": err.Error()})
	}
	timeSvc = timesync.New(timesync.Options{
		Manage: *manageHost,
		Servers: func() ([]string, string) {
			cfg, _ := netMgr.Config()
			return netconfig.EffectiveNTP(cfg, netMgr.DHCPNTPServers())
		},
	})
	// HAProxy first: it needs neither the clock nor the PKI, and a node
	// that waits for NTP below must still serve traffic meanwhile.
	// haproxy needs its pid-file/stats-socket directory (and this
	// daemon's own runtime dir in general) to exist - the target OS has
	// no package manager / installer to have created it ahead of time,
	// so this is the one place that responsibility can live.
	if err := os.MkdirAll(filepath.Dir(*haproxyPid), 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(*haproxyPid), err)
	}

	// The chroot jail haproxy.cfg's `chroot` directive points into.
	// Nothing is ever accessed inside it after the chroot() call (every
	// file haproxy needs - config, maps, ACLs, certs - is opened before
	// it drops privileges), so it's created empty and inaccessible on
	// purpose: mode 0000, not even readable by its own owner.
	if err := os.MkdirAll(*haproxyChrootDir, 0o000); err != nil {
		log.Fatalf("mkdir %s: %v", *haproxyChrootDir, err)
	}

	haproxyMgr := haproxy.NewManager(*haproxyBin, *haproxyCfg, *haproxyPid, *haproxySock)
	haproxyMgr.Output = &ring.LineWriter{Ring: serviceLogs["haproxy"]}
	haproxyMgr.CertStoreDir = *haproxyCertStore
	if haproxyMgr.CertStoreDir == "" {
		haproxyMgr.CertStoreDir = filepath.Join(filepath.Dir(*haproxyCfg), "runtime-certs")
	}

	// Start haproxy from whatever config is already on disk (the
	// bootstrap default at first boot - see rootfs/base/etc/haproxy -
	// or the last config ApplyConfig wrote), so a node runs HAProxy from
	// boot without needing an API call first. Not fatal: a dev build
	// without the haproxy binary in place should still serve the gRPC
	// API for everything else.
	haproxyRunning := true
	if err := haproxyMgr.Boot(); err != nil {
		haproxyRunning = false
		log.Printf("haproxy: initial start failed (continuing without it): %v", err)
	}

	// The image's optional extensions (internal/extensions): their
	// services start with HAProxy, before the NTP wait and the PKI, for
	// the same reason.
	extManifests, err := extensions.Load(extensions.Dir)
	if err != nil {
		log.Printf("extensions: %v - no extension service started", err)
		extManifests = nil
	}
	extMgr := extensions.NewManager(extManifests, func(id string) io.Writer {
		serviceLogs[id] = ring.New[string](serviceLogLines)
		return io.MultiWriter(os.Stderr, &ring.LineWriter{Ring: serviceLogs[id]})
	})
	// keepalived (the VRRP extension) waits for its configuration file:
	// put the saved one there, and the HAProxy health file it can track,
	// before the services start.
	vrrpMgr := vrrp.New(extMgr)
	// HAProxy's health, for keepalived's track_file and BIRD's haproxy_*
	// protocols: it answers on its stats socket, and isn't being stopped -
	// a soft stop closes the listeners long before the process exits.
	haproxyHealthy := func() bool {
		if !haproxyMgr.Serving() {
			return false
		}
		_, err := haproxyMgr.ShowInfo()
		return err == nil
	}
	if *manageHost && vrrpMgr.Available() {
		if err := vrrpMgr.Boot(); err != nil {
			log.Printf("vrrp: %v", err)
		}
		go vrrp.KeepHealth(haproxyHealthy, haproxyMgr.Changed, 2*time.Second, nil)
	}
	// BIRD (the BGP extension) likewise; its haproxy_* protocols are kept
	// down while HAProxy doesn't answer - checked every second: a BGP
	// session takes longer than that to come up.
	bgpMgr := bgp.New(extMgr)
	if *manageHost && bgpMgr.Available() {
		if err := bgpMgr.Boot(); err != nil {
			log.Printf("bgp: %v", err)
		}
		go bgpMgr.KeepGate(haproxyHealthy, haproxyMgr.Changed, time.Second, nil)
	}
	// A deliberate stop of HAProxy gives the virtual IPs up and withdraws
	// the anycast routes at once (Changed wakes both checks), then waits
	// this long before closing the listeners: the traffic has moved to
	// another node by then, and none is refused meanwhile.
	if *manageHost && (vrrpMgr.Available() || bgpMgr.Available()) {
		haproxyMgr.DrainDelay = *haproxyDrain
	}
	// prometheus-node-exporter runs as its saved settings say.
	if extMgr.Has(nodeexporter.ServiceID) {
		cfg, _, err := nodeexporter.Load()
		if err != nil {
			log.Printf("node-exporter: %v - running the defaults", err)
		}
		if err := extMgr.Configure(nodeexporter.ServiceID, nodeexporter.Args(cfg), cfg.Enabled); err != nil {
			log.Printf("node-exporter: %v", err)
		}
	}
	extMgr.Start()

	// SIGTERM is rootfs/init asking janusd to stop before a power-off or
	// reboot (/sbin/shutdown, e.g. from the QEMU guest agent): HAProxy is
	// soft-stopped like the Shutdown RPC does, the extension services
	// stopped, then janusd exits and init takes the machine down.
	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, syscall.SIGTERM)
	go func() {
		<-stopSignals
		log.Printf("janusd: SIGTERM - stopping HAProxy and extension services")
		extMgr.StopAll()
		if err := haproxyMgr.Stop(5 * time.Second); err != nil {
			log.Printf("haproxy: stop: %v", err)
		}
		syscall.Sync()
		os.Exit(0)
	}()
	for _, m := range extManifests {
		log.Printf("extensions: %s %s", m.Name, m.Version)
	}

	if *manageHost {
		go timeSvc.Run(context.Background())
		// No global address, no NTP server to reach (a board without a
		// network): nothing to wait for.
		if !pki.Bootstrapped(*pkiDir) && time.Now().Before(clockFloor) && hasGlobalAddress() {
			waitForClock(timeSvc)
		}
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "janus"
	}

	pkiBootstrap, err := pki.LoadOrBootstrap(*pkiDir, hostname, pki.LocalIPs())
	if err != nil {
		log.Fatalf("pki: %v", err)
	}
	if pkiBootstrap.AdminIssued {
		log.SetOutput(os.Stderr) // console only - see serviceLogs above
		log.Printf("pki: first boot - generated a new CA and admin client certificate in %s", *pkiDir)
		// The CA cert isn't secret (it only lets a client verify the
		// server's identity, not authenticate as anyone) - printed
		// alongside the admin cert/key anyway, not just left to a
		// separate STATE-partition extraction, since a console reader
		// bootstrapping a node needs all three to actually connect
		// (janusctl's -ca/-cert/-key) and splitting them across two
		// different recovery paths for one single one-time event was
		// real friction, not a meaningful security boundary - whoever
		// can read this console already has the admin cert/key printed
		// right below, which is the actually sensitive half.
		log.Printf("pki: CA CERTIFICATE (needed for janusctl's -ca flag):\n%s", pkiBootstrap.CA.CertPEM)
		log.Printf("pki: ADMIN CERTIFICATE (save this now, it will not be printed again):\n%s%s", pkiBootstrap.AdminCertPEM, pkiBootstrap.AdminKeyPEM)
		// pkiDir may be the Phase 3 cont'd persistent STATE partition
		// (see rootfs/init/main.go's mountState) - force these bytes to
		// the underlying block device now rather than trusting they're
		// still there if the node loses power before some later,
		// unrelated sync happens to occur.
		syscall.Sync()
		log.SetOutput(captured)
	}
	events.Publish("janusd.started", map[string]string{"version": version})

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen on %s: %v", *addr, err)
	}

	// If rootfs/init's checkBootCommit left a pending wait_for_health
	// marker for this boot, confirm it against HAProxy's *actual*
	// health - not just "this daemon process is still running", which
	// says nothing about whether haproxy itself ever came up - and
	// revert automatically if it never does. Runs in the background:
	// gRPC must start regardless, and a marker (the rare case) shouldn't
	// delay it.
	if marker, err := bootcommit.Read(); err != nil {
		log.Printf("bootcommit: read marker: %v", err)
	} else if marker != nil {
		go confirmBootHealth(marker, haproxyMgr)
	}

	// Point 2 suite, tranche 5: if this node was provisioned with a
	// Controller (LifecycleService.Install's controller_address/
	// controller_ca_cert, see internal/api/install.go), and hasn't
	// already announced itself, do so now - in the background, same
	// reasoning as bootcommit above: gRPC must start regardless, and
	// the overwhelming majority of boots have no Controller configured
	// at all.
	go selfRegisterIfConfigured(pkiBootstrap.CA, hostname, *addr)

	// The server certificate follows the node's addresses and hostname
	// (reissued on a change - see netMgr's OnChange above) and is renewed
	// before it expires.
	serverCert = pki.NewServerCert(pkiBootstrap.CA, *pkiDir, pkiBootstrap.ServerCert)
	refreshServerCert(serverCert)
	go func() {
		for range time.Tick(12 * time.Hour) {
			refreshServerCert(serverCert)
		}
	}()

	// The node's own Prometheus exporter (internal/exporter). The kernel
	// log and STATE are only this node's to report when janusd runs it.
	metrics := &metricsSources{version: version, started: started, ca: pkiBootstrap.CA, serverCert: serverCert,
		haproxy: haproxyMgr, ext: extMgr, net: netMgr, time: timeSvc, firewall: fwMgr, vrrp: vrrpMgr, bgp: bgpMgr}
	if *manageHost {
		metrics.kmsg = &kmsgwatch.Counts{}
		metrics.statePath = "/etc/.state"
		go kmsgwatch.Watch("/dev/kmsg", metrics.kmsg)
	}
	exp := exporter.New(metrics.collectors()...)
	expCfg, _, err := exporter.Load()
	if err != nil {
		log.Printf("exporter: %v - using the defaults", err)
	}
	if err := exp.Apply(expCfg); err != nil {
		log.Printf("exporter: %v", err)
	}

	tlsConfig := pkiBootstrap.CA.ServerTLSConfigFor(serverCert)
	srv := grpc.NewServer(append(connectionOptions(keepaliveTime, keepaliveTimeout),
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.StatsHandler(api.ConnStats{}),
		grpc.ChainUnaryInterceptor(api.UnaryMetricsInterceptor, api.UnaryAuthInterceptor),
		grpc.ChainStreamInterceptor(api.StreamMetricsInterceptor, api.StreamAuthInterceptor),
	)...)
	janusv1alpha1.RegisterSystemServiceServer(srv, &api.System{BuildVersion: version, CA: pkiBootstrap.CA, ServiceLogs: serviceLogs, HAProxy: haproxyMgr, Extensions: extMgr, Exporter: exp})
	janusv1alpha1.RegisterLifecycleServiceServer(srv, &api.Lifecycle{HAProxy: haproxyMgr})
	janusv1alpha1.RegisterHAProxyServiceServer(srv, &api.HAProxy{Manager: haproxyMgr})
	janusv1alpha1.RegisterNetworkServiceServer(srv, &api.Network{Net: netMgr, Time: timeSvc, Firewall: fwMgr, VRRP: vrrpMgr, HAProxyHealthy: haproxyHealthy, BGP: bgpMgr})

	log.Printf("janusd %s listening on %s (mTLS required)", version, *addr)
	printMOTD(motdInfo{
		Version:        version,
		KernelVersion:  api.KernelVersion(),
		ActiveSlot:     api.CurrentActiveSlot(),
		APIAddresses:   apiAddresses(*addr, pki.LocalIPs()),
		HAProxyRunning: haproxyRunning,
		FirstBoot:      pkiBootstrap.AdminIssued,
	})
	if err := srv.Serve(lis); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

// refreshServerCert reissues the server certificate if the node's
// hostname or addresses changed, or it nears expiry.
func refreshServerCert(s *pki.ServerCert) {
	hostname, err := os.Hostname()
	if err != nil {
		return
	}
	reissued, err := s.Refresh(hostname, pki.LocalIPs())
	if err != nil {
		log.Printf("pki: reissue the server certificate: %v", err)
		return
	}
	if reissued {
		syscall.Sync()
		log.Printf("pki: server certificate reissued for %s and the node's current addresses", hostname)
		events.Publish("pki.server_cert_reissued", map[string]string{"hostname": hostname})
	}
}

// clockFloor: a clock before this can't be right - the node has no
// battery-backed clock (a Raspberry Pi boots in 1970) and hasn't
// synchronized yet.
var clockFloor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// hasGlobalAddress reports whether the node has any global unicast
// address - some way to reach an NTP server at all.
func hasGlobalAddress() bool {
	for _, ip := range pki.LocalIPs() {
		if ip.IsGlobalUnicast() {
			return true
		}
	}
	return false
}

// waitForClock holds a first boot whose clock is obviously wrong until
// it's synchronized, since the certificates generated next are dated by
// it: a CA "valid" from 1970 to 1980 is expired for every client. A
// clock that's merely a little off (a battery-backed one) doesn't wait:
// certificates are backdated an hour anyway. Bounded: a node without NTP
// access still has to come up, with a warning.
func waitForClock(t *timesync.Service) {
	const wait = 2 * time.Minute
	log.Printf("timesync: first boot and the clock reads %s - waiting up to %s for NTP before generating certificates", time.Now().UTC().Format(time.RFC3339), wait)
	if !t.WaitSynced(wait) {
		log.Printf("timesync: WARNING: no NTP server answered - the certificates generated now carry the date %s and clients will refuse them; give the node NTP access, then reset it", time.Now().UTC().Format(time.DateOnly))
	}
}

// Connection limits for the gRPC server.
//
// Several RPCs stream until the client cancels: Events, Logs and Dmesg
// with follow, and PacketCapture without duration_seconds. A client that
// vanishes without closing its TCP connection (host powered off, network
// cut, NAT entry expired) never cancels, and without keepalive the
// server would only notice after the kernel's TCP timeouts - hours -
// with the stream, or the capture, running all that time. The server
// pings a connection it hasn't heard from for keepaliveTime and drops it
// if the ping isn't answered within keepaliveTimeout, cancelling its
// streams. gRPC clients (janusctl, the Controller) answer pings on their
// own; no client-side change is needed.
//
// maxConcurrentStreams bounds what one connection can hold open at
// once. The Controller multiplexes every open browser page for a node
// over one connection (a few followed streams per page), so this is
// sized well above that, as a guard against runaway clients rather than
// a limit legitimate use should meet.
const (
	keepaliveTime        = 2 * time.Minute
	keepaliveTimeout     = 20 * time.Second
	maxConcurrentStreams = 256
)

func connectionOptions(pingAfter, pingTimeout time.Duration) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: pingAfter, Timeout: pingTimeout}),
		grpc.MaxConcurrentStreams(maxConcurrentStreams),
	}
}

// healthPollInterval/healthStableChecks bound how quickly a genuinely
// healthy HAProxy gets confirmed: 3 consecutive successful checks,
// 500ms apart, is enough to rule out a single fluke (a check that
// raced startup, say) without adding a slow, arbitrary-feeling delay
// on top of an already-real signal.
const (
	healthPollInterval   = 500 * time.Millisecond
	healthStableChecks   = 3
	defaultHealthTimeout = 60 * time.Second // used if the marker's own HealthTimeoutSeconds is unset
)

// confirmBootHealth runs internal/bootcommit.Confirm against a real
// HAProxy health signal (ShowInfo succeeding means the stats socket is
// up and answering, which requires the haproxy process itself to
// actually be running - not just that this daemon, janusd, is)
// and, on failure, reverts back to marker.RevertTo the same way
// rootfs/init's own boot-time revert path does (internal/bootrevert),
// then reboots. Meant to run in its own goroutine - it blocks for up to
// the marker's own health-timeout.
func confirmBootHealth(marker *bootcommit.Marker, mgr *haproxy.Manager) {
	log.Printf("bootcommit: confirming health for slot %s (revert to %s if it never comes up)", marker.Slot, marker.RevertTo)

	confirmed, err := bootcommit.Confirm(marker,
		func() error {
			_, err := mgr.ShowInfo()
			return err
		},
		func() error {
			if err := bootrevert.To(marker); err != nil {
				return err
			}
			syscall.Sync()
			log.Printf("bootcommit: rebooting to complete the revert to slot %s", marker.RevertTo)
			events.Publish("bootcommit.reverted", map[string]string{"slot": marker.Slot, "revert_to": marker.RevertTo})
			consoledrain.Wait(os.Stderr, 2*time.Second)
			return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
		},
		healthPollInterval, healthStableChecks, defaultHealthTimeout)

	if err != nil {
		// Either Clear() failed on the confirm path, or the revert
		// itself failed - either way, this node may now be stuck on an
		// unconfirmed slot with no automatic recovery left, worth
		// logging loudly rather than just silently returning.
		log.Printf("bootcommit: %v", err)
		return
	}
	if confirmed {
		log.Printf("bootcommit: confirmed healthy for slot %s", marker.Slot)
		events.Publish("bootcommit.confirmed", map[string]string{"slot": marker.Slot})
	}
	// !confirmed && err == nil: the revert (and reboot) succeeded -
	// nothing further to do, the machine is already on its way down.
}

// selfRegisterIfConfigured is Point 2 suite tranche 5's actual trigger:
// reads internal/selfregister.Dir (bind-mounted from STATE's own
// controller/ subdirectory by rootfs/init's mountState - see that
// function's own comment), and if a Controller was provisioned for this
// node (LifecycleService.Install's controller_address/
// controller_ca_cert) and it hasn't already announced itself, does so
// now. A failed attempt (Controller unreachable, say) is simply logged
// and left for the next boot to retry - selfregister.MarkRegistered is
// only ever written on success, so nothing here needs its own retry
// loop.
func selfRegisterIfConfigured(ca *pki.CA, hostname, grpcAddr string) {
	cfg, err := selfregister.Read(selfregister.Dir)
	if err != nil {
		log.Printf("selfregister: read config: %v", err)
		return
	}
	if cfg == nil {
		return // no Controller was provisioned for this node - the overwhelming majority of boots
	}
	if selfregister.AlreadyRegistered(selfregister.Dir) {
		return
	}

	advertiseAddr, err := selfregister.DetectAdvertiseAddress(cfg.Address, grpcAddr)
	if err != nil {
		log.Printf("selfregister: determine address to advertise to Controller at %s: %v", cfg.Address, err)
		return
	}

	log.Printf("selfregister: announcing to Controller at %s as %s (%s)", cfg.Address, hostname, advertiseAddr)
	if err := selfregister.Register(cfg, ca, hostname, advertiseAddr); err != nil {
		log.Printf("selfregister: registration failed, will retry on next boot: %v", err)
		events.Publish("selfregister.failed", map[string]string{"controller": cfg.Address, "error": err.Error()})
		return
	}
	if err := selfregister.MarkRegistered(selfregister.Dir); err != nil {
		log.Printf("selfregister: mark registered: %v", err)
		return
	}
	log.Printf("selfregister: successfully announced to Controller at %s, awaiting approval", cfg.Address)
	events.Publish("selfregister.announced", map[string]string{"controller": cfg.Address})
}
