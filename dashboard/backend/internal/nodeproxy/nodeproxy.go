// Package nodeproxy runs one dedicated HTTPS listener per registered
// node, on that node's own allocated port (see dashboard/backend/
// internal/store) - the browser's TLS client-certificate negotiation
// happens per origin (host:port), so managing more than one node with
// native browser cert selection needs a distinct origin per node, not
// one shared listener (see the rebranding/dashboard plan's own
// architecture section for the full reasoning).
//
// The client certificate a browser presents here only ever proves the
// browser is allowed into *this* node's view (it's checked against that
// node's own CA) - it is never, and cryptographically can never be,
// reused to talk to the real node (a TLS server can verify a client
// holds a private key, it can never extract that key). All real gRPC
// calls to the node use the store.Node's own dedicated service
// credential instead.
package nodeproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// staticFiles is the entire per-node dashboard view - deliberately
// plain HTML/JS, no build step, no framework (unlike dashboard/
// frontend's own React SPA, which lives on a completely different
// origin - see this package's own doc comment for why the two can't
// just be the same thing).
//
//go:embed static
var staticFiles embed.FS

// Listener is one running per-node HTTPS endpoint.
type Listener struct {
	node   *store.Node
	server *http.Server
}

// Start begins serving node's dashboard view on its own allocated port
// immediately - callers should treat a returned error as "this node's
// listener never came up", not something to retry inline.
func Start(node *store.Node, dashboardServerCert tls.Certificate, st *store.Store) (*Listener, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(node.CACertPEM) {
		return nil, fmt.Errorf("node %s: no valid certificates in stored ca.crt", node.ID)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{dashboardServerCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}

	handler, err := newHandler(node, st)
	if err != nil {
		return nil, err
	}

	addr := fmt.Sprintf(":%d", node.Port)
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("node %s: listen on %s: %w", node.ID, addr, err)
	}

	srv := &http.Server{Handler: handler}
	go func() {
		// ErrServerClosed is the expected outcome of Stop() below, not a
		// real failure - nothing else to do with a listener error after
		// the fact except let it die; the node just stops being
		// reachable through the dashboard until re-added.
		_ = srv.Serve(ln)
	}()

	return &Listener{node: node, server: srv}, nil
}

// newHandler is everything a per-node listener serves, behind CSRF
// protection. The browser attaches the client certificate that opens
// this origin to *any* request aimed at it, including one a page from
// another site triggers - so the certificate alone doesn't prove the
// operator meant the request. http.CrossOriginProtection rejects
// cross-origin browser requests with a non-safe method (every endpoint
// that changes something: power, services, config, maps, certificates,
// upgrades...); GET stays open, since a cross-origin page can't read the
// response. Requests from non-browser clients (curl, scripts) carry
// neither Sec-Fetch-Site nor Origin and are unaffected.
func newHandler(node *store.Node, st *store.Store) (http.Handler, error) {
	view, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		handleInfo(w, r, node)
	})
	mux.HandleFunc("GET /api/node", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, map[string]string{"id": node.ID, "name": node.Name, "address": node.Addr()})
	})
	registerOpsRoutes(mux, node)
	registerLifecycleRoutes(mux, node)
	registerReleaseRoutes(mux)
	registerUpdateRoutes(mux, node)
	registerPcapRoutes(mux, node)
	registerSystemRoutes(mux, node)
	registerNetworkRoutes(mux, node, st)
	mux.Handle("/", http.FileServerFS(view))

	return http.NewCrossOriginProtection().Handler(mux), nil
}

// sameOriginOrDirect reports whether a browser request came from this
// origin's own page or was typed/bookmarked by the user - for the rare
// GET that has a side effect (starting a packet capture), which
// http.CrossOriginProtection deliberately leaves alone. Non-browser
// clients send no Sec-Fetch-Site and pass.
func sameOriginOrDirect(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// Stop shuts this node's listener down - used both for explicit node
// removal and (in a future slice) restarting a listener after its
// service credential is rotated.
func (l *Listener) Stop(ctx context.Context) error {
	err := l.server.Shutdown(ctx)
	closeNodeConn(l.node.ID)
	return err
}

// conns holds one long-lived gRPC connection per node (keyed by node
// ID), shared by every handler: live views poll every second, and a
// fresh TLS handshake per request would dominate that. grpc.ClientConn
// multiplexes concurrent calls and reconnects on its own.
var conns sync.Map

// dialNode returns node's shared gRPC connection, using its own stored
// service credential - never the browser's client certificate (see the
// package doc comment for why that's not just a design choice but a
// cryptographic impossibility). Callers must not Close it.
func dialNode(node *store.Node) (*grpc.ClientConn, error) {
	if c, ok := conns.Load(node.ID); ok {
		return c.(*grpc.ClientConn), nil
	}
	tlsConfig, err := pki.ClientTLSConfig(node.CACertPEM, node.ServiceCertPEM, node.ServiceKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("build TLS config: %w", err)
	}
	// A rebooting node should be reachable again within seconds of
	// coming back, not after gRPC's default backoff (up to 2 minutes).
	backoffCfg := backoff.DefaultConfig
	backoffCfg.MaxDelay = 5 * time.Second
	c, err := grpc.NewClient(node.Addr(),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoffCfg, MinConnectTimeout: 5 * time.Second}))
	if err != nil {
		return nil, err
	}
	if existing, loaded := conns.LoadOrStore(node.ID, c); loaded {
		c.Close()
		return existing.(*grpc.ClientConn), nil
	}
	return c, nil
}

func closeNodeConn(nodeID string) {
	if c, ok := conns.LoadAndDelete(nodeID); ok {
		c.(*grpc.ClientConn).Close()
	}
}

// infoResponse is the dashboard's single-node view payload - the same
// set of SystemService RPCs hack/qemu-system-info-test.sh already proves
// return real values from a real boot (see internal/api/system_stats.go).
type infoResponse struct {
	Version       string       `json:"version"`
	GoVersion     string       `json:"go_version"`
	KernelVersion string       `json:"kernel_version"`
	ActiveSlot    string       `json:"active_slot"`
	Memory        *memoryInfo  `json:"memory"`
	CPUs          []cpuInfo    `json:"cpus"`
	LoadAvg       *loadAvgInfo `json:"load_avg"`
	Disks         []diskInfo   `json:"disks"`
}

type memoryInfo struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	CachedBytes    uint64 `json:"cached_bytes"`
}

type cpuInfo struct {
	Processor uint32  `json:"processor"`
	ModelName string  `json:"model_name"`
	MHz       float64 `json:"mhz"`
}

type loadAvgInfo struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

type diskInfo struct {
	DeviceName     string `json:"device_name"`
	ReadCompleted  uint64 `json:"read_completed"`
	WriteCompleted uint64 `json:"write_completed"`
}

func handleInfo(w http.ResponseWriter, r *http.Request, node *store.Node) {
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}

	client := janusv1alpha1.NewSystemServiceClient(conn)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ver, err := client.Version(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Version: %v", err), http.StatusBadGateway)
		return
	}
	mem, err := client.Memory(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("Memory: %v", err), http.StatusBadGateway)
		return
	}
	cpu, err := client.CPUInfo(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("CPUInfo: %v", err), http.StatusBadGateway)
		return
	}
	load, err := client.LoadAvg(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("LoadAvg: %v", err), http.StatusBadGateway)
		return
	}
	disks, err := client.DiskStats(ctx, &emptypb.Empty{})
	if err != nil {
		http.Error(w, fmt.Sprintf("DiskStats: %v", err), http.StatusBadGateway)
		return
	}

	resp := infoResponse{
		Version:       ver.GetVersion(),
		GoVersion:     ver.GetGoVersion(),
		KernelVersion: ver.GetKernelVersion(),
		ActiveSlot:    ver.GetActiveSlot(),
		Memory: &memoryInfo{
			TotalBytes:     mem.GetTotalBytes(),
			AvailableBytes: mem.GetAvailableBytes(),
			CachedBytes:    mem.GetCachedBytes(),
		},
		LoadAvg: &loadAvgInfo{Load1: load.GetLoad1(), Load5: load.GetLoad5(), Load15: load.GetLoad15()},
	}
	for _, c := range cpu.GetCpus() {
		resp.CPUs = append(resp.CPUs, cpuInfo{Processor: c.GetProcessor(), ModelName: c.GetModelName(), MHz: c.GetMhz()})
	}
	for _, d := range disks.GetDisks() {
		resp.Disks = append(resp.Disks, diskInfo{
			DeviceName:     d.GetDeviceName(),
			ReadCompleted:  d.GetReadCompleted(),
			WriteCompleted: d.GetWriteCompleted(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
