package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/extensions"
)

// haproxyStopTimeout is how long a soft stop (in-flight connections
// finishing) gets before haproxy is terminated.
const haproxyStopTimeout = 10 * time.Second

// ServiceList reports the managed services: janusd itself, haproxy, and
// the services of the image's optional extensions.
func (s *System) ServiceList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ServiceListResponse, error) {
	list := []*janusv1alpha1.ServiceInfo{s.janusdInfo(), s.haproxyInfo()}
	if s.Extensions != nil {
		for _, id := range s.Extensions.ServiceIDs() {
			if info, err := s.extensionInfo(id); err == nil {
				list = append(list, info)
			}
		}
	}
	return &janusv1alpha1.ServiceListResponse{Services: list}, nil
}

func (s *System) extensionInfo(id string) (*janusv1alpha1.ServiceInfo, error) {
	st, err := s.Extensions.State(id)
	if err != nil {
		return nil, err
	}
	health := "unknown"
	switch st.State {
	case "running":
		health = "healthy"
	case "restarting":
		health = "unhealthy"
	case "waiting":
		health = "waiting"
	}
	return &janusv1alpha1.ServiceInfo{Id: id, State: st.State, Health: health, Extension: st.Extension, Description: st.Description}, nil
}

// extensionCall runs a start/stop/restart on an extension service, if id
// is one.
func (s *System) extensionCall(id string, call func(string) error) (*janusv1alpha1.ServiceResponse, bool, error) {
	if s.Extensions == nil || !s.Extensions.Has(id) {
		return nil, false, nil
	}
	if err := call(id); err != nil {
		if errors.Is(err, extensions.ErrDisabled) {
			return nil, true, status.Errorf(codes.FailedPrecondition, "%s: %v - enable it in its settings", id, err)
		}
		return nil, true, status.Errorf(codes.Internal, "%s: %v", id, err)
	}
	info, err := s.extensionInfo(id)
	if err != nil {
		return nil, true, status.Errorf(codes.Internal, "%s: %v", id, err)
	}
	return &janusv1alpha1.ServiceResponse{Service: info}, true, nil
}

func (s *System) ServiceStart(_ context.Context, req *janusv1alpha1.ServiceRequest) (*janusv1alpha1.ServiceResponse, error) {
	if s.Extensions != nil {
		if resp, ok, err := s.extensionCall(req.GetId(), s.Extensions.StartService); ok {
			return resp, err
		}
	}
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
	return nil, s.unknownService(req.GetId())
}

// ServiceStop soft-stops haproxy: it stops accepting connections and
// finishes the ones in flight (up to haproxyStopTimeout). janusd can't be
// stopped - the node would be unreachable; see Restart instead.
func (s *System) ServiceStop(_ context.Context, req *janusv1alpha1.ServiceRequest) (*janusv1alpha1.ServiceResponse, error) {
	if s.Extensions != nil {
		if resp, ok, err := s.extensionCall(req.GetId(), s.Extensions.StopService); ok {
			return resp, err
		}
	}
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
	return nil, s.unknownService(req.GetId())
}

// ServiceRestart restarts haproxy seamlessly (a new process takes over
// the listening sockets, the old one finishes its connections), or
// starts it if it was stopped. For janusd it's the same as Restart.
func (s *System) ServiceRestart(ctx context.Context, req *janusv1alpha1.ServiceRequest) (*janusv1alpha1.ServiceResponse, error) {
	if s.Extensions != nil {
		if resp, ok, err := s.extensionCall(req.GetId(), s.Extensions.RestartService); ok {
			return resp, err
		}
	}
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
	return nil, s.unknownService(req.GetId())
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

func (s *System) unknownService(id string) error {
	managed := []string{"janusd", "haproxy"}
	if s.Extensions != nil {
		managed = append(managed, s.Extensions.ServiceIDs()...)
	}
	return status.Errorf(codes.NotFound, "no service %q on this node (managed services: %s) - optional extensions are chosen when the image is built", id, strings.Join(managed, ", "))
}
