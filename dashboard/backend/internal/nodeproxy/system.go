package nodeproxy

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	pkcs12 "software.sslmate.com/src/go-pkcs12"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

const (
	unaryTimeout = 10 * time.Second
	// A soft stop waits up to 10s node-side for in-flight connections.
	serviceTimeout = 30 * time.Second
	// How much of a file the UI previews inline; downloads are whole.
	readPreviewBytes = 1 << 20
	sseHeartbeat     = 15 * time.Second
)

func registerSystemRoutes(mux *http.ServeMux, node *store.Node) {
	sys := func(fn func(context.Context, janusv1alpha1.SystemServiceClient, *http.Request) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewSystemServiceClient(c), r)
			})
		}
	}
	empty := &emptypb.Empty{}

	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) { handleMetrics(w, r, node) })
	mux.HandleFunc("GET /api/system/overview", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		h, err := c.Hostname(ctx, empty)
		if err != nil {
			return nil, err
		}
		st, err := c.SystemStat(ctx, empty)
		if err != nil {
			return nil, err
		}
		v, err := c.Version(ctx, empty)
		if err != nil {
			return nil, err
		}
		return map[string]any{"hostname": h.GetHostname(), "version": v, "system": st}, nil
	}))
	mux.HandleFunc("GET /api/system/processes", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		return c.Processes(ctx, empty)
	}))
	mux.HandleFunc("GET /api/system/mounts", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		return c.Mounts(ctx, empty)
	}))
	mux.HandleFunc("GET /api/system/disks", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		return c.DiskStats(ctx, empty)
	}))
	mux.HandleFunc("GET /api/system/netdev", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		return c.NetworkDeviceStats(ctx, empty)
	}))
	mux.HandleFunc("GET /api/system/netstat", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		return c.Netstat(ctx, empty)
	}))
	mux.HandleFunc("GET /api/system/services", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		return c.ServiceList(ctx, empty)
	}))
	mux.HandleFunc("POST /api/system/services/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		unary(w, r, node, serviceTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			c := janusv1alpha1.NewSystemServiceClient(conn)
			req := &janusv1alpha1.ServiceRequest{Id: r.PathValue("id")}
			switch r.PathValue("action") {
			case "start":
				return c.ServiceStart(ctx, req)
			case "stop":
				return c.ServiceStop(ctx, req)
			case "restart":
				return c.ServiceRestart(ctx, req)
			}
			return nil, status.Errorf(codes.InvalidArgument, "unknown action %q (want start, stop or restart)", r.PathValue("action"))
		})
	})
	mux.HandleFunc("POST /api/system/power", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action    string `json:"action"` // reboot, shutdown, restart, reset
			WipeState bool   `json:"wipe_state"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		unary(w, r, node, unaryTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			c := janusv1alpha1.NewSystemServiceClient(conn)
			var err error
			switch req.Action {
			case "reboot":
				_, err = c.Reboot(ctx, &janusv1alpha1.RebootRequest{})
			case "shutdown":
				_, err = c.Shutdown(ctx, empty)
			case "restart":
				_, err = c.Restart(ctx, empty)
			case "reset":
				_, err = c.Reset(ctx, &janusv1alpha1.ResetRequest{WipeState: req.WipeState, WipeEphemeral: true})
			default:
				err = status.Errorf(codes.InvalidArgument, "unknown action %q", req.Action)
			}
			return struct{}{}, err
		})
	})
	mux.HandleFunc("GET /api/network/modules", func(w http.ResponseWriter, r *http.Request) {
		unary(w, r, node, unaryTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			c := janusv1alpha1.NewNetworkServiceClient(conn)
			state := func(st janusv1alpha1.ModuleState, err error) map[string]string {
				if err != nil {
					return map[string]string{"state": "error", "message": status.Convert(err).Message()}
				}
				return map[string]string{"state": strings.ToLower(strings.TrimPrefix(st.String(), "MODULE_STATE_"))}
			}
			bgp, err := c.BGPStatus(ctx, empty)
			b := state(bgp.GetState(), err)
			vrrp, err := c.VRRPStatus(ctx, empty)
			v := state(vrrp.GetState(), err)
			fw, err := c.FirewallList(ctx, empty)
			f := state(fw.GetState(), err)
			return map[string]any{"bgp": b, "vrrp": v, "firewall": f}, nil
		})
	})

	// --- files ---
	mux.HandleFunc("GET /api/files/list", func(w http.ResponseWriter, r *http.Request) {
		unary(w, r, node, unaryTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			stream, err := janusv1alpha1.NewSystemServiceClient(conn).List(ctx, &janusv1alpha1.ListRequest{Root: r.URL.Query().Get("path"), Recursive: r.URL.Query().Get("recursive") == "true"})
			if err != nil {
				return nil, err
			}
			entries := []*janusv1alpha1.FileInfo{}
			for {
				f, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return entries, nil
				}
				if err != nil {
					return nil, err
				}
				entries = append(entries, f)
			}
		})
	})
	mux.HandleFunc("GET /api/files/du", func(w http.ResponseWriter, r *http.Request) {
		unary(w, r, node, 60*time.Second, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			stream, err := janusv1alpha1.NewSystemServiceClient(conn).DiskUsage(ctx, &janusv1alpha1.DiskUsageRequest{Paths: r.URL.Query()["path"], Recursive: r.URL.Query().Get("recursive") == "true"})
			if err != nil {
				return nil, err
			}
			entries := []*janusv1alpha1.DiskUsageInfo{}
			for {
				e, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return entries, nil
				}
				if err != nil {
					return nil, err
				}
				entries = append(entries, e)
			}
		})
	})
	mux.HandleFunc("GET /api/files/read", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		download := r.URL.Query().Get("download") == "true"
		limit := int64(readPreviewBytes)
		if download {
			limit = -1
		}
		relayData(w, r, node, "application/octet-stream", attachmentName(download, path.Base(p)), limit, func(ctx context.Context, c janusv1alpha1.SystemServiceClient) (dataStream, error) {
			return c.Read(ctx, &janusv1alpha1.ReadRequest{Path: p})
		})
	})
	mux.HandleFunc("GET /api/files/copy", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		relayData(w, r, node, "application/x-tar", attachmentName(true, path.Base(p)+".tar"), -1, func(ctx context.Context, c janusv1alpha1.SystemServiceClient) (dataStream, error) {
			return c.Copy(ctx, &janusv1alpha1.CopyRequest{RootPath: p})
		})
	})
	mux.HandleFunc("GET /api/dmesg", func(w http.ResponseWriter, r *http.Request) {
		relayData(w, r, node, "text/plain; charset=utf-8", attachmentName(r.URL.Query().Get("download") == "true", "dmesg.txt"), -1, func(ctx context.Context, c janusv1alpha1.SystemServiceClient) (dataStream, error) {
			return c.Dmesg(ctx, &janusv1alpha1.DmesgRequest{})
		})
	})

	// --- live streams (Server-Sent Events) ---
	mux.HandleFunc("GET /api/stream/logs", func(w http.ResponseWriter, r *http.Request) {
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		streamLines(w, r, node, func(ctx context.Context, c janusv1alpha1.SystemServiceClient) (dataStream, error) {
			return c.Logs(ctx, &janusv1alpha1.LogsRequest{Id: r.URL.Query().Get("id"), Follow: true, TailLines: int32(tail)})
		})
	})
	mux.HandleFunc("GET /api/stream/dmesg", func(w http.ResponseWriter, r *http.Request) {
		streamLines(w, r, node, func(ctx context.Context, c janusv1alpha1.SystemServiceClient) (dataStream, error) {
			return c.Dmesg(ctx, &janusv1alpha1.DmesgRequest{Follow: true})
		})
	})
	mux.HandleFunc("GET /api/stream/events", func(w http.ResponseWriter, r *http.Request) { streamEvents(w, r, node) })

	// --- HAProxy extras ---
	mux.HandleFunc("POST /api/haproxy/validate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Config string `json:"config"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		unary(w, r, node, unaryTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			return janusv1alpha1.NewHAProxyServiceClient(conn).ValidateConfig(ctx, &janusv1alpha1.ValidateConfigRequest{Config: []byte(req.Config)})
		})
	})
	mux.HandleFunc("POST /api/haproxy/reload", func(w http.ResponseWriter, r *http.Request) {
		unary(w, r, node, unaryTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			return janusv1alpha1.NewHAProxyServiceClient(conn).Reload(ctx, empty)
		})
	})
	mux.HandleFunc("GET /api/haproxy/stats", func(w http.ResponseWriter, r *http.Request) {
		unary(w, r, node, unaryTimeout, func(ctx context.Context, conn *grpc.ClientConn) (any, error) {
			resp, err := janusv1alpha1.NewHAProxyServiceClient(conn).Stats(ctx, empty)
			if err != nil {
				return nil, err
			}
			return parseStatCSV(resp.GetRawCsv())
		})
	})

	// --- access: issue a client certificate ---
	mux.HandleFunc("POST /api/pki/client", func(w http.ResponseWriter, r *http.Request) { handleIssueClient(w, r, node) })
}

// unary runs call on node's shared connection and writes the result as
// JSON, or the gRPC error with a matching HTTP status and just its
// message.
func unary(w http.ResponseWriter, r *http.Request, node *store.Node, timeout time.Duration, call func(context.Context, *grpc.ClientConn) (any, error)) {
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	resp, err := call(ctx, conn)
	if err != nil {
		http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
		return
	}
	writeJSONBody(w, http.StatusOK, resp)
}

func grpcHTTPStatus(err error) int {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.PermissionDenied, codes.Unauthenticated:
		return http.StatusForbidden
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

type dataStream interface {
	Recv() (*janusv1alpha1.Data, error)
}

func attachmentName(download bool, name string) string {
	if !download {
		return ""
	}
	return unsafeFilenameChars.ReplaceAllString(name, "_")
}

// relayData streams a Data-returning RPC as an HTTP body. The first
// message is awaited before any header goes out, so a node-side error
// becomes a real HTTP error. limit < 0 means no limit.
func relayData(w http.ResponseWriter, r *http.Request, node *store.Node, contentType, attachment string, limit int64, open func(context.Context, janusv1alpha1.SystemServiceClient) (dataStream, error)) {
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream, err := open(ctx, janusv1alpha1.NewSystemServiceClient(conn))
	if err != nil {
		http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
		return
	}
	first, err := stream.Recv()
	if err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	if attachment != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, attachment))
	}
	flusher, _ := w.(http.Flusher)
	var written int64
	for msg := first; msg != nil; {
		b := msg.GetBytes()
		if limit >= 0 && written+int64(len(b)) > limit {
			b = b[:limit-written]
		}
		if _, err := w.Write(b); err != nil {
			return
		}
		written += int64(len(b))
		if flusher != nil {
			flusher.Flush()
		}
		if limit >= 0 && written >= limit {
			return // preview truncated; the stream is cancelled on return
		}
		if msg, err = stream.Recv(); err != nil {
			return
		}
	}
}

// sseWriter serializes writes to a Server-Sent Events response, so the
// heartbeat and the relayed stream never interleave.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSE(w http.ResponseWriter, r *http.Request) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	s := &sseWriter{w: w, flusher: flusher}
	go func() {
		t := time.NewTicker(sseHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				if s.write(": ping\n\n") != nil {
					return // the browser went away
				}
			}
		}
	}()
	return s, true
}

func (s *sseWriter) write(frame string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := io.WriteString(s.w, frame); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// send writes one event; a multi-line payload becomes several data:
// lines, which the browser joins back with "\n".
func (s *sseWriter) send(event, payload string) error {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	for _, line := range strings.Split(payload, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return s.write(b.String())
}

// streamLines relays a followed Data stream (Logs, Dmesg) as SSE, one
// event per received chunk. A node-side error is sent as a "failure"
// event, since the 200 has already gone out (not "error", which
// EventSource already uses for its own connection errors).
func streamLines(w http.ResponseWriter, r *http.Request, node *store.Node, open func(context.Context, janusv1alpha1.SystemServiceClient) (dataStream, error)) {
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	stream, err := open(r.Context(), janusv1alpha1.NewSystemServiceClient(conn))
	if err != nil {
		http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
		return
	}
	sse, ok := newSSE(w, r)
	if !ok {
		return
	}
	_ = sse.write(": connected\n\n")
	for {
		msg, err := stream.Recv()
		if err != nil {
			if r.Context().Err() == nil && !errors.Is(err, io.EOF) {
				_ = sse.send("failure", status.Convert(err).Message())
			}
			return
		}
		if err := sse.send("", strings.TrimRight(string(msg.GetBytes()), "\n")); err != nil {
			return
		}
	}
}

func streamEvents(w http.ResponseWriter, r *http.Request, node *store.Node) {
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	stream, err := janusv1alpha1.NewSystemServiceClient(conn).Events(r.Context(), &janusv1alpha1.EventsRequest{SinceId: since})
	if err != nil {
		http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
		return
	}
	sse, ok := newSSE(w, r)
	if !ok {
		return
	}
	_ = sse.write(": connected\n\n")
	for {
		e, err := stream.Recv()
		if err != nil {
			if r.Context().Err() == nil && !errors.Is(err, io.EOF) {
				_ = sse.send("failure", status.Convert(err).Message())
			}
			return
		}
		payload := json.RawMessage("null")
		if len(e.GetPayload()) > 0 && json.Valid(e.GetPayload()) {
			payload = e.GetPayload()
		}
		data, _ := json.Marshal(map[string]any{"id": e.GetId(), "time_ns": e.GetUnixTimeNs(), "type": e.GetType(), "payload": payload})
		if err := sse.send("", string(data)); err != nil {
			return
		}
	}
}

// metricsSample is one poll of everything the live charts plot; the
// browser keeps the history and turns counters into rates.
type metricsSample struct {
	TimeMs   int64                                     `json:"time_ms"`
	System   *janusv1alpha1.SystemStatResponse         `json:"system,omitempty"`
	Memory   *janusv1alpha1.MemoryResponse             `json:"memory,omitempty"`
	Load     *janusv1alpha1.LoadAvgResponse            `json:"load,omitempty"`
	Network  *janusv1alpha1.NetworkDeviceStatsResponse `json:"network,omitempty"`
	Services *janusv1alpha1.StatsResponse              `json:"services,omitempty"`
	HAProxy  *janusv1alpha1.ShowInfoResponse           `json:"haproxy,omitempty"`
	Errors   map[string]string                         `json:"errors,omitempty"`
}

func handleMetrics(w http.ResponseWriter, r *http.Request, node *store.Node) {
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	hap := janusv1alpha1.NewHAProxyServiceClient(conn)
	empty := &emptypb.Empty{}

	out := metricsSample{TimeMs: time.Now().UnixMilli(), Errors: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	fetches := 0 // "unreachable" means every fetch below failed, however many there are
	fetch := func(name string, call func() error) {
		fetches++
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := call(); err != nil {
				mu.Lock()
				out.Errors[name] = status.Convert(err).Message()
				mu.Unlock()
			}
		}()
	}
	fetch("system", func() (err error) { out.System, err = sys.SystemStat(ctx, empty); return })
	fetch("memory", func() (err error) { out.Memory, err = sys.Memory(ctx, empty); return })
	fetch("load", func() (err error) { out.Load, err = sys.LoadAvg(ctx, empty); return })
	fetch("network", func() (err error) { out.Network, err = sys.NetworkDeviceStats(ctx, empty); return })
	fetch("services", func() (err error) { out.Services, err = sys.Stats(ctx, empty); return })
	fetch("haproxy", func() (err error) { out.HAProxy, err = hap.ShowInfo(ctx, empty); return })
	wg.Wait()
	if len(out.Errors) == fetches {
		http.Error(w, "node unreachable: "+out.Errors["system"], http.StatusBadGateway)
		return
	}
	writeJSONBody(w, http.StatusOK, out)
}

// statTable is "show stat" as a header plus rows, for the UI to pick
// columns from by name.
type statTable struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

func parseStatCSV(raw []byte) (*statTable, error) {
	raw = bytes.TrimPrefix(bytes.TrimSpace(raw), []byte("# "))
	rd := csv.NewReader(bytes.NewReader(raw))
	rd.FieldsPerRecord = -1
	records, err := rd.ReadAll()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "parse show stat: %v", err)
	}
	if len(records) == 0 {
		return &statTable{Columns: []string{}, Rows: [][]string{}}, nil
	}
	return &statTable{Columns: records[0], Rows: append([][]string{}, records[1:]...)}, nil
}

// handleIssueClient issues a new client certificate for this node
// (SystemService.GenerateClientConfiguration) and hands it to the
// browser as a password-protected .pfx (the same format used to add a
// node or import into a browser) or as PEM files in JSON.
func handleIssueClient(w http.ResponseWriter, r *http.Request, node *store.Node) {
	var req struct {
		Role     string `json:"role"`     // os:admin or os:reader
		Format   string `json:"format"`   // pfx or pem
		Password string `json:"password"` // pfx only
		Name     string `json:"name"`     // used in the download's file name
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role != "os:admin" && req.Role != "os:reader" {
		http.Error(w, "role must be os:admin or os:reader", http.StatusBadRequest)
		return
	}
	if req.Format != "pfx" && req.Format != "pem" {
		http.Error(w, "format must be pfx or pem", http.StatusBadRequest)
		return
	}
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), unaryTimeout)
	defer cancel()
	resp, err := janusv1alpha1.NewSystemServiceClient(conn).GenerateClientConfiguration(ctx, &janusv1alpha1.GenerateClientConfigurationRequest{Roles: []string{req.Role}})
	if err != nil {
		http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if req.Format == "pem" {
		writeJSONBody(w, http.StatusOK, map[string]string{"ca": string(resp.GetCa()), "crt": string(resp.GetCrt()), "key": string(resp.GetKey())})
		return
	}
	pfx, err := encodePFX(resp.GetCa(), resp.GetCrt(), resp.GetKey(), req.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := req.Name
	if name == "" {
		name = strings.TrimPrefix(req.Role, "os:")
	}
	w.Header().Set("Content-Type", "application/x-pkcs12")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, unsafeFilenameChars.ReplaceAllString("janus-"+node.Name+"-"+name, "_")+".pfx"))
	_, _ = w.Write(pfx)
}

func encodePFX(caPEM, certPEM, keyPEM []byte, password string) ([]byte, error) {
	parse := func(p []byte) (*x509.Certificate, error) {
		block, _ := pem.Decode(p)
		if block == nil {
			return nil, errors.New("no PEM block")
		}
		return x509.ParseCertificate(block.Bytes)
	}
	ca, err := parse(caPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA: %w", err)
	}
	cert, err := parse(certPEM)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("parse key: no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if key, err = x509.ParseECPrivateKey(block.Bytes); err != nil {
			return nil, fmt.Errorf("parse key: %w", err)
		}
	}
	return pkcs12.Modern.Encode(key, cert, []*x509.Certificate{ca}, password)
}
