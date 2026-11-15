package clusters

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// refusingModules answers VRRP's and BGP's applies the way a node does
// when keepalived or BIRD refuses the configuration: no gRPC error,
// accepted false, the daemon's messages.
type refusingModules struct {
	janusv1alpha1.UnimplementedNetworkServiceServer
	accept atomic.Bool
}

func (f *refusingModules) VRRPApplyConfig(context.Context, *janusv1alpha1.VRRPApplyConfigRequest) (*janusv1alpha1.VRRPApplyConfigResponse, error) {
	if f.accept.Load() {
		return &janusv1alpha1.VRRPApplyConfigResponse{Accepted: true}, nil
	}
	return &janusv1alpha1.VRRPApplyConfigResponse{Errors: []string{
		"(keepalived.conf: Line 11) WARNING - interface eth0 for vrrp_instance pvt doesn't exist",
		"Non-existent interface specified in configuration",
	}}, nil
}

func (f *refusingModules) BGPApplyConfig(context.Context, *janusv1alpha1.BGPApplyConfigRequest) (*janusv1alpha1.BGPApplyConfigResponse, error) {
	if f.accept.Load() {
		return &janusv1alpha1.BGPApplyConfigResponse{Accepted: true}, nil
	}
	return &janusv1alpha1.BGPApplyConfigResponse{}, nil
}

// A configuration keepalived or BIRD refuses fails the apply - and the
// check on the first member before any is touched -, with the daemon's
// words: taken for a success, the engine journaled a version "applied"
// on members that ran no VRRP.
func TestModuleApplyRefused(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake := &refusingModules{}
	janusv1alpha1.RegisterNetworkServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	g := GRPC{Conn: func(context.Context, Member) (grpc.ClientConnInterface, error) { return conn, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := Member{Name: "pvt-a"}

	for _, validateOnly := range []bool{true, false} {
		err := g.ModuleApply(ctx, m, "vrrp", []byte("vrrp_instance pvt {}"), validateOnly)
		if err == nil || !strings.Contains(err.Error(), "keepalived.conf: (keepalived.conf: Line 11) WARNING - interface eth0") ||
			!strings.Contains(err.Error(), "; Non-existent interface specified in configuration") {
			t.Errorf("VRRP refused (validate only %v): %v", validateOnly, err)
		}
		err = g.ModuleApply(ctx, m, "bgp", []byte("router id 1.2.3.4;"), validateOnly)
		if err == nil || err.Error() != "bird.conf: refused, without a reason given" {
			t.Errorf("BGP refused (validate only %v): %v", validateOnly, err)
		}
	}
	fake.accept.Store(true)
	for _, d := range []string{"vrrp", "bgp"} {
		if err := g.ModuleApply(ctx, m, d, []byte("x"), false); err != nil {
			t.Errorf("%s accepted: %v", d, err)
		}
	}
}
