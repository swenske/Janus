// Package nodeproxy is each registered node's page and API on the
// Controller: served under /nodes/<id>/ on its main port, behind its
// accounts (dashboard/backend's gate), and relayed to the node over one
// shared gRPC connection per node. The Controller reaches a node that
// trusts its fleet with its own short-lived fleet certificate, acting
// for the signed-in account - the node checks that account's role
// itself (WithUser) -, and an older node with the service credential it
// got when the node was added.
package nodeproxy

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// staticFiles is the node page (dashboard/frontend's node app, built
// with relative asset paths: it's served under /nodes/<id>/).
//
//go:embed static
var staticFiles embed.FS

// Handler is node's page and API, for the Controller to serve under
// /nodes/<id>/ (the prefix stripped) behind its own gate.
func Handler(node *store.Node, st *store.Store) (http.Handler, error) {
	return newHandler(node, st)
}

// Forget closes node's connection - the node is gone, or its
// credentials changed.
func Forget(nodeID string) { closeNodeConn(nodeID) }

// newHandler is everything a node page serves, behind CSRF protection -
// the Controller's main handler has it too; kept here so this handler is
// never served without it. http.CrossOriginProtection rejects
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
	// Who the page acts for: the Controller's account, the node role the
	// node gives it, and the RPCs that role may call ("Service/Method",
	// internal/rbac) - the page shows only what it may do.
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		u := userOf(r.Context())
		writeJSONBody(w, http.StatusOK, map[string]any{"name": u.Name, "roles": nonNilStrings(u.Roles()), "may": nonNilStrings(u.May())})
	})
	mux.HandleFunc("GET /api/node", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, map[string]string{"id": node.ID, "name": node.Name, "address": node.Addr(), "controller_version": ControllerVersion, "locked_by": LockedBy(node)})
	})
	registerOpsRoutes(mux, node)
	registerLifecycleRoutes(mux, node)
	registerReleaseRoutes(mux)
	registerUpdateRoutes(mux, node)
	registerPcapRoutes(mux, node)
	registerSystemRoutes(mux, node)
	registerNetworkRoutes(mux, node, st)
	registerFirewallRoutes(mux, node)
	registerVRRPRoutes(mux, node)
	registerBGPRoutes(mux, node)
	registerConsulRoutes(mux, node)
	registerACMERoutes(mux, node)
	registerHAProxyFileRoutes(mux, node)
	registerAccessRoutes(mux, node)
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
	opts, err := dialOptions(node)
	if err != nil {
		return nil, err
	}
	// A rebooting node should be reachable again within seconds of
	// coming back, not after gRPC's default backoff (up to 2 minutes).
	backoffCfg := backoff.DefaultConfig
	backoffCfg.MaxDelay = 5 * time.Second
	c, err := grpc.NewClient(node.Addr(), append(opts,
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoffCfg, MinConnectTimeout: 5 * time.Second}))...)
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

// Conn is node's shared gRPC connection, for the Controller's own reads
// of it (its backups): Automation unless the context says who.
func Conn(node *store.Node) (*grpc.ClientConn, error) { return dialNode(node) }

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
