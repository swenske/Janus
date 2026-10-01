package api

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Without their extension in the image, the modules report NOT_ENABLED
// and refuse a configuration.
func TestNetworkModules(t *testing.T) {
	n, ctx := &Network{}, context.Background()
	if st, err := n.BGPStatus(ctx, &emptypb.Empty{}); err != nil || st.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
		t.Errorf("BGPStatus = %v, %v", st, err)
	}
	if st, err := n.VRRPStatus(ctx, &emptypb.Empty{}); err != nil || st.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
		t.Errorf("VRRPStatus = %v, %v", st, err)
	}
	if fw, err := n.FirewallList(ctx, &emptypb.Empty{}); err != nil || fw.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
		t.Errorf("FirewallList = %v, %v", fw, err)
	}
	if _, err := n.BGPApplyConfig(ctx, &janusv1alpha1.BGPApplyConfigRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("BGPApplyConfig without bird = %v", err)
	}
	if _, err := n.BGPGetConfig(ctx, &emptypb.Empty{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("BGPGetConfig without bird = %v", err)
	}
	if _, err := n.VRRPApplyConfig(ctx, &janusv1alpha1.VRRPApplyConfigRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("VRRPApplyConfig without keepalived = %v", err)
	}
}

func TestServiceControlErrors(t *testing.T) {
	s, ctx := &System{}, context.Background()
	list, err := s.ServiceList(ctx, &emptypb.Empty{})
	if err != nil || len(list.GetServices()) != 2 || list.Services[1].GetState() != "stopped" {
		t.Errorf("ServiceList without haproxy = %v, %v", list, err)
	}
	if _, err := s.ServiceStop(ctx, &janusv1alpha1.ServiceRequest{Id: "janusd"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ServiceStop(janusd) = %v", err)
	}
	if _, err := s.ServiceStart(ctx, &janusv1alpha1.ServiceRequest{Id: "bird"}); status.Code(err) != codes.NotFound {
		t.Errorf("ServiceStart(bird) = %v", err)
	}
	if _, err := s.ServiceRestart(ctx, &janusv1alpha1.ServiceRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("ServiceRestart(nope) = %v", err)
	}
	if _, err := s.ServiceStart(ctx, &janusv1alpha1.ServiceRequest{Id: "haproxy"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("ServiceStart(haproxy) without a manager = %v", err)
	}
	if _, err := s.Reset(ctx, &janusv1alpha1.ResetRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Reset with nothing to wipe = %v", err)
	}
}
