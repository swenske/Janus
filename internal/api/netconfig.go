package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
	"github.com/swenske/Janus/internal/netmgr"
)

// The node's own network configuration (NetworkService.NetworkConfig*,
// NetworkStatus) - see internal/netmgr for the engine, internal/netconfig
// for the model. The optional modules are in network.go.

func (n *Network) NetworkConfigGet(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.NetworkConfigGetResponse, error) {
	if n.Net == nil {
		return nil, status.Error(codes.Unavailable, "network management isn't running")
	}
	cfg, isDefault := n.Net.Config()
	return &janusv1alpha1.NetworkConfigGetResponse{Config: cfg, IsDefault: isDefault}, nil
}

func (n *Network) NetworkConfigApply(req *janusv1alpha1.NetworkConfigApplyRequest, stream grpc.ServerStreamingServer[janusv1alpha1.NetworkConfigApplyResponse]) error {
	if n.Net == nil {
		return status.Error(codes.Unavailable, "network management isn't running")
	}
	cfg := req.GetConfig()
	if cfg == nil {
		cfg = &janusv1alpha1.NetworkConfig{}
	}
	if err := stream.Send(&janusv1alpha1.NetworkConfigApplyResponse{Stage: "validating"}); err != nil {
		return err
	}
	if err := netconfig.Validate(cfg); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if !n.Net.Managed() {
		return netError(netmgr.ErrNotManaged)
	}
	timeout := time.Duration(req.GetConfirmTimeoutSeconds()) * time.Second
	if timeout == 0 {
		timeout = netconfig.DefaultConfirmTimeout
	}
	// Sent before applying: once the node's addresses change, this
	// stream may not survive (its own address can be one the new
	// configuration removes), and the caller still needs to know it has
	// to confirm, and by when.
	if err := stream.Send(&janusv1alpha1.NetworkConfigApplyResponse{
		Stage:        "applying",
		Message:      fmt.Sprintf("confirm within %s from an address the new configuration keeps, or it reverts", timeout),
		RevertAtUnix: time.Now().Add(timeout).Unix(),
	}); err != nil {
		return err
	}
	addrs, revertAt, err := n.Net.ApplyTrial(cfg, timeout)
	if err != nil {
		return netError(err)
	}
	// Best effort: see above.
	_ = stream.Send(&janusv1alpha1.NetworkConfigApplyResponse{
		Stage:        "awaiting-confirmation",
		Message:      "applied - call NetworkConfigConfirm before it reverts",
		Addresses:    addrs,
		RevertAtUnix: revertAt.Unix(),
	})
	return nil
}

func (n *Network) NetworkConfigConfirm(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.NetworkConfigConfirmResponse, error) {
	if n.Net == nil {
		return nil, status.Error(codes.Unavailable, "network management isn't running")
	}
	local, err := localAddr(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	via, err := n.Net.Confirm(local)
	if err != nil {
		return nil, netError(err)
	}
	return &janusv1alpha1.NetworkConfigConfirmResponse{ConfirmedVia: via}, nil
}

func (n *Network) NetworkStatus(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.NetworkStatusResponse, error) {
	if n.Net == nil {
		return nil, status.Error(codes.Unavailable, "network management isn't running")
	}
	st, err := n.Net.Status()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the network state: %v", err)
	}
	if n.Time != nil {
		st.Time = n.Time.Status()
	}
	return st, nil
}

// localAddr is the node-side address of the calling connection.
func localAddr(ctx context.Context) (netip.Addr, error) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.LocalAddr == nil {
		return netip.Addr{}, errors.New("no local address on this connection")
	}
	tcp, ok := p.LocalAddr.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, fmt.Errorf("unexpected local address %s", p.LocalAddr)
	}
	a, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("unexpected local address %s", p.LocalAddr)
	}
	return a.Unmap(), nil
}

func netError(err error) error {
	var invalid *netmgr.InvalidError
	var conflict *netmgr.ConflictError
	switch {
	case errors.As(err, &invalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.As(err, &conflict),
		errors.Is(err, netmgr.ErrNotManaged),
		errors.Is(err, netmgr.ErrTrialPending),
		errors.Is(err, netmgr.ErrNoTrial):
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}
