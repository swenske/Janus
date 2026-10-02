package nodeproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
	"github.com/swenske/Janus/internal/pki"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// The node's own network configuration (NetworkService.NetworkConfig*,
// NetworkStatus). Applying is a trial on the node's side: it reverts by
// itself unless confirmed in time over an address the new configuration
// keeps. The Controller reaching the node there *is* the proof the
// operator needs, so it confirms by itself (unless asked not to) - and,
// when the node moved, records its new address.

// pendingConfirm is where to reach a node whose configuration is on
// trial, by node ID.
var pendingConfirm sync.Map

type pendingTrial struct {
	candidates []string
	revertAt   time.Time
}

var protoOut = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

func writeProto(w http.ResponseWriter, m proto.Message) {
	data, err := protoOut.Marshal(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

type networkApplyResult struct {
	Stages       []string `json:"stages"`
	Addresses    []string `json:"addresses"`
	RevertAtUnix int64    `json:"revert_at_unix"`
	Confirmed    bool     `json:"confirmed"`
	ConfirmedVia string   `json:"confirmed_via,omitempty"`
	// The node's gRPC address after the change, as now stored.
	Address    string   `json:"address"`
	Candidates []string `json:"candidates,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func registerNetworkRoutes(mux *http.ServeMux, node *store.Node, st *store.Store) {
	empty := &emptypb.Empty{}

	mux.HandleFunc("GET /api/network/status", func(w http.ResponseWriter, r *http.Request) {
		conn, err := dialNode(node)
		if err != nil {
			http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), unaryTimeout)
		defer cancel()
		resp, err := janusv1alpha1.NewNetworkServiceClient(conn).NetworkStatus(ctx, empty)
		if err != nil {
			http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
			return
		}
		writeProto(w, resp)
	})

	mux.HandleFunc("GET /api/network/config", func(w http.ResponseWriter, r *http.Request) {
		conn, err := dialNode(node)
		if err != nil {
			http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), unaryTimeout)
		defer cancel()
		resp, err := janusv1alpha1.NewNetworkServiceClient(conn).NetworkConfigGet(ctx, empty)
		if err != nil {
			http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
			return
		}
		// The configuration in its compact form (as janusctl network get
		// prints it) - the UI diffs it against the edited one.
		cfg, err := netconfig.Marshal(resp.GetConfig())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"config": json.RawMessage(cfg), "is_default": resp.GetIsDefault()})
	})

	mux.HandleFunc("POST /api/network/apply", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Config                json.RawMessage `json:"config"`
			ConfirmTimeoutSeconds uint32          `json:"confirm_timeout_seconds"`
			NoConfirm             bool            `json:"no_confirm"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if len(req.Config) == 0 {
			req.Config = json.RawMessage("{}")
		}
		cfg, err := netconfig.Parse(req.Config)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, httpStatus, err := applyNetwork(r.Context(), node, cfg, req.ConfirmTimeoutSeconds)
		if err != nil {
			http.Error(w, err.Error(), httpStatus)
			return
		}
		if !req.NoConfirm {
			confirmNetwork(node, st, res)
		}
		writeJSONBody(w, http.StatusOK, res)
	})

	mux.HandleFunc("POST /api/network/confirm", func(w http.ResponseWriter, r *http.Request) {
		res := &networkApplyResult{Address: node.Addr()}
		if p, ok := pendingConfirm.Load(node.ID); ok {
			res.Candidates = p.(pendingTrial).candidates
			res.RevertAtUnix = p.(pendingTrial).revertAt.Unix()
		} else {
			res.Candidates = []string{node.Addr()}
			res.RevertAtUnix = time.Now().Add(5 * time.Second).Unix()
		}
		confirmNetwork(node, st, res)
		if !res.Confirmed {
			http.Error(w, res.Error, http.StatusBadGateway)
			return
		}
		writeJSONBody(w, http.StatusOK, res)
	})
}

// applyNetwork puts cfg on trial on the node. The stream may break once
// the node drops the address this connection uses - after the
// "applying" stage, that's expected, not a failure.
func applyNetwork(ctx context.Context, node *store.Node, cfg *janusv1alpha1.NetworkConfig, timeoutSeconds uint32) (*networkApplyResult, int, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("dial node: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream, err := janusv1alpha1.NewNetworkServiceClient(conn).NetworkConfigApply(ctx, &janusv1alpha1.NetworkConfigApplyRequest{Config: cfg, ConfirmTimeoutSeconds: timeoutSeconds})
	if err != nil {
		return nil, grpcHTTPStatus(err), errors.New(status.Convert(err).Message())
	}
	res := &networkApplyResult{Address: node.Addr()}
	applying := false
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !connectionLost(err) {
				return nil, grpcHTTPStatus(err), errors.New(status.Convert(err).Message())
			}
			if applying {
				res.Stages = append(res.Stages, "connection lost while applying (expected if the node's address changed)")
			} else {
				// The node writes "applying" just before it drops the
				// address this connection uses: that last message can be
				// lost with it. Or the request never reached the node (a
				// connection from before it rebooted). Confirming tells
				// which - a node with nothing on trial says so.
				res.Stages = append(res.Stages, "connection lost - checking whether the node applied it")
			}
			break
		}
		res.Stages = append(res.Stages, fmt.Sprintf("%s: %s", msg.GetStage(), msg.GetMessage()))
		if msg.GetStage() == "applying" {
			applying = true
		}
		if msg.GetRevertAtUnix() != 0 {
			res.RevertAtUnix = msg.GetRevertAtUnix()
		}
		if len(msg.GetAddresses()) > 0 {
			res.Addresses = msg.GetAddresses()
		}
	}
	if res.RevertAtUnix == 0 { // the node's own deadline was lost with the connection
		if timeoutSeconds == 0 {
			timeoutSeconds = 30
		}
		res.RevertAtUnix = time.Now().Add(time.Duration(timeoutSeconds) * time.Second).Unix()
	}
	res.Candidates = confirmCandidates(node.Addr(), cfg, res.Addresses)
	pendingConfirm.Store(node.ID, pendingTrial{candidates: res.Candidates, revertAt: time.Unix(res.RevertAtUnix, 0)})
	return res, http.StatusOK, nil
}

// confirmCandidates lists where the node can be reached after the
// change: its current address first (valid whenever the configuration
// keeps it), then each address it reported, or - when that report was
// lost with the old address - the configuration's static ones.
func confirmCandidates(current string, cfg *janusv1alpha1.NetworkConfig, reported []string) []string {
	_, port, err := net.SplitHostPort(current)
	if err != nil {
		port = "9505"
	}
	out := []string{current}
	add := func(cidr string) {
		p, err := netip.ParsePrefix(cidr)
		if err != nil || !p.Addr().IsGlobalUnicast() {
			return
		}
		ep := net.JoinHostPort(p.Addr().String(), port)
		if !slices.Contains(out, ep) {
			out = append(out, ep)
		}
	}
	for _, a := range reported {
		add(a)
	}
	if len(reported) == 0 {
		for _, iface := range cfg.GetInterfaces() {
			for _, a := range iface.GetAddresses() {
				add(a)
			}
		}
	}
	return out
}

// confirmNetwork tries each candidate until the node accepts the
// confirmation or the trial is about to revert. When the node answered
// on a new address, that address is stored and the shared connection
// reset.
func confirmNetwork(node *store.Node, st *store.Store, res *networkApplyResult) {
	ep, ok := confirmLoop(res, func(ep string) (string, error) { return confirmAt(node, ep) })
	if ok || res.Error != "" {
		pendingConfirm.Delete(node.ID)
	}
	if ok {
		if ep != node.Addr() {
			if st == nil {
				res.Error = "confirmed, but the new address couldn't be recorded"
			} else if err := st.SetAddress(node.ID, ep); err != nil {
				res.Error = fmt.Sprintf("confirmed, but recording the new address failed: %v", err)
			} else {
				log.Printf("node %s (%s) moved to %s after a network reconfiguration", node.Name, node.ID, ep)
			}
			closeNodeConn(node.ID)
		}
		res.Address = node.Addr()
	}
}

// confirmLoop tries each candidate with confirm until one accepts (its
// endpoint, true) or the trial is about to revert. A node that answers
// it has nothing on trial ends it at once: the change never reached it,
// or it already went back.
func confirmLoop(res *networkApplyResult, confirm func(ep string) (string, error)) (string, bool) {
	deadline := time.Unix(res.RevertAtUnix, 0).Add(-time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		for _, ep := range res.Candidates {
			via, err := confirm(ep)
			if status.Code(err) == codes.FailedPrecondition {
				res.Error = fmt.Sprintf("the node has nothing awaiting confirmation (%s): it didn't receive this change, or already reverted it - nothing changed, apply it again", status.Convert(err).Message())
				return "", false
			}
			if err != nil {
				lastErr = fmt.Errorf("%s: %s", ep, status.Convert(err).Message())
				continue
			}
			res.Confirmed, res.ConfirmedVia = true, via
			return ep, true
		}
		time.Sleep(time.Second)
	}
	if lastErr == nil {
		lastErr = errors.New("the trial already reverted")
	}
	res.Error = fmt.Sprintf("couldn't confirm before the node reverts (%v) - it goes back to its previous configuration by itself", lastErr)
	return "", false
}

// connectionLost reports an error that says nothing of what the node did:
// the connection broke, or this side gave up waiting.
func connectionLost(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return false
}

// confirmAt calls NetworkConfigConfirm over a fresh connection to
// endpoint, with the node's stored credential.
func confirmAt(node *store.Node, endpoint string) (string, error) {
	tlsConfig, err := pki.ClientTLSConfig(node.CACertPEM, node.ServiceCertPEM, node.ServiceKeyPEM)
	if err != nil {
		return "", err
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	resp, err := janusv1alpha1.NewNetworkServiceClient(conn).NetworkConfigConfirm(ctx, &emptypb.Empty{}, grpc.WaitForReady(true))
	if err != nil {
		return "", err // a status: FailedPrecondition is "nothing on trial"
	}
	return resp.GetConfirmedVia(), nil
}
