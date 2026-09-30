package api

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
)

// haproxyStopTimeout is how long a soft stop (in-flight connections
// finishing) gets before haproxy is terminated.
const haproxyStopTimeout = 10 * time.Second

// ServiceList reports the managed services: janusd itself and haproxy.
// Optional modules (bird, keepalived) appear once an image ships them.
func (s *System) ServiceList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ServiceListResponse, error) {
	return &janusv1alpha1.ServiceListResponse{Services: []*janusv1alpha1.ServiceInfo{s.janusdInfo(), s.haproxyInfo()}}, nil
}

func (s *System) ServiceStart(_ context.Context, req *janusv1alpha1.ServiceRequest) (*janusv1alpha1.ServiceResponse, error) {
	switch req.GetId() {
	case "janusd":
		return &janusv1alpha1.ServiceResponse{Service: s.janusdInfo()}, nil
	case "haproxy":
		if err := s.needHAProxy(); err != nil {
			return nil, err
		}
		if !s.HAProxy.Running() {
			if err := s.HAProxy.Reload(); err != nil {
				return nil, status.Errorf(codes.Internal, "start haproxy: %v", err)
			}
			events.Publish("service.started", map[string]string{"id": "haproxy"})
		}
		return &janusv1alpha1.ServiceResponse{Service: s.haproxyInfo()}, nil
	}
	return nil, unknownService(req.GetId())
}

// ServiceStop soft-stops haproxy: it stops accepting connections and
// finishes the ones in flight (up to haproxyStopTimeout). janusd can't be
// stopped - the node would be unreachable; see Restart instead.
func (s *System) ServiceStop(_ context.Context, req *janusv1alpha1.ServiceRequest) (*janusv1alpha1.ServiceResponse, error) {
	switch req.GetId() {
	case "janusd":
		return nil, status.Error(codes.FailedPrecondition, "stopping janusd would leave the node unreachable - use Restart to restart it")
	case "haproxy":
		if err := s.needHAProxy(); err != nil {
			return nil, err
		}
		if err := s.HAProxy.Stop(haproxyStopTimeout); err != nil {
			return nil, status.Errorf(codes.Internal, "stop haproxy: %v", err)
		}
		events.Publish("service.stopped", map[string]string{"id": "haproxy"})
		return &janusv1alpha1.ServiceResponse{Service: s.haproxyInfo()}, nil
	}
	return nil, unknownService(req.GetId())
}

// ServiceRestart restarts haproxy seamlessly (a new process takes over
// the listening sockets, the old one finishes its connections), or
// starts it if it was stopped. For janusd it's the same as Restart.
func (s *System) ServiceRestart(ctx context.Context, req *janusv1alpha1.ServiceRequest) (*janusv1alpha1.ServiceResponse, error) {
	switch req.GetId() {
	case "janusd":
		if _, err := s.Restart(ctx, &emptypb.Empty{}); err != nil {
			return nil, err
		}
		return &janusv1alpha1.ServiceResponse{Service: &janusv1alpha1.ServiceInfo{Id: "janusd", State: "restarting", Health: "unknown"}}, nil
	case "haproxy":
		if err := s.needHAProxy(); err != nil {
			return nil, err
		}
		if err := s.HAProxy.Reload(); err != nil {
			return nil, status.Errorf(codes.Internal, "restart haproxy: %v", err)
		}
		events.Publish("service.restarted", map[string]string{"id": "haproxy"})
		return &janusv1alpha1.ServiceResponse{Service: s.haproxyInfo()}, nil
	}
	return nil, unknownService(req.GetId())
}

func (s *System) janusdInfo() *janusv1alpha1.ServiceInfo {
	// Answering at all is the health check.
	return &janusv1alpha1.ServiceInfo{Id: "janusd", State: "running", Health: "healthy"}
}

func (s *System) haproxyInfo() *janusv1alpha1.ServiceInfo {
	info := &janusv1alpha1.ServiceInfo{Id: "haproxy", State: "stopped", Health: "unknown"}
	if s.HAProxy == nil || !s.HAProxy.Running() {
		return info
	}
	info.State = "running"
	info.Health = "unhealthy"
	// Healthy means it answers on its stats socket, the same check
	// wait_for_health upgrades rely on.
	if _, err := s.HAProxy.ShowInfo(); err == nil {
		info.Health = "healthy"
	}
	return info
}

func (s *System) needHAProxy() error {
	if s.HAProxy == nil {
		return status.Error(codes.FailedPrecondition, "haproxy isn't managed by this janusd")
	}
	return nil
}

func unknownService(id string) error {
	switch id {
	case "bird", "keepalived":
		return status.Errorf(codes.NotFound, "%s isn't in this node's image", id)
	}
	return status.Errorf(codes.NotFound, "unknown service %q (managed services: janusd, haproxy)", id)
}
