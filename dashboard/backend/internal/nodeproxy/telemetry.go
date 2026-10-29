package nodeproxy

import (
	"cmp"
	"context"
	"errors"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/dashboard/backend/internal/telemetry"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/schematic"
)

// TelemetryUser is who asks nodes what the usage report counts: a
// reader, its calls not in the nodes' audit.
var TelemetryUser = User{Name: "telemetry", Perms: []Perm{{Role: pki.RoleReader}}}

// TelemetryNode is what the usage report counts of node
// (internal/telemetry): its image and its size, never its name or
// address. A node that doesn't say its size counts without one.
func TelemetryNode(ctx context.Context, node *store.Node) (*telemetry.Node, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	ctx = WithUser(ctx, TelemetryUser)
	c := janusv1alpha1.NewSystemServiceClient(conn)
	v, err := c.Version(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, errors.New(status.Convert(err).Message())
	}
	n := &telemetry.Node{Version: v.GetVersion(), Arch: v.GetArch(), HAProxy: v.GetHaproxy().GetVariant(), Kernel: v.GetKernel().GetVariant(),
		Console: NodeSchematic(v).ConsoleChoice(), Schematic: cmp.Or(v.GetSchematicId(), schematic.DefaultID())}
	for _, e := range v.GetExtensions() {
		n.Extensions = append(n.Extensions, e.GetName())
	}
	if cpu, err := c.CPUInfo(ctx, &emptypb.Empty{}); err == nil {
		n.VCPUs = len(cpu.GetCpus())
	}
	if mem, err := c.Memory(ctx, &emptypb.Empty{}); err == nil {
		n.MemoryBytes = mem.GetTotalBytes()
	}
	return n, nil
}
