package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// The policy goes to the Controller with its unset numbers at 0 - its
// defaults - and comes back the same; its pause isn't a change; off or
// none is no block.
func TestClusterAutoscaling(t *testing.T) {
	a := &clusterAutoscaleModel{Mode: types.StringValue("on"), MinMembers: types.Int64Value(2), MaxMembers: types.Int64Value(5)}
	for _, f := range a.fields(&client.ClusterAutoscale{}) {
		*f.v = types.Int64Null()
	}
	a.OutAfterSeconds = types.Int64Value(120)
	p := a.policy()
	if p.Mode != "on" || p.MinMembers != 2 || p.MaxMembers != 5 || p.OutAfterSeconds != 120 || p.InAfterSeconds != 0 {
		t.Fatalf("policy: %+v", p)
	}
	back := clusterAutoscaleFrom(p)
	if !back.OutAfterSeconds.Equal(types.Int64Value(120)) || !back.InAfterSeconds.IsNull() || !sameClusterPolicy(back.policy(), p) {
		t.Errorf("back: %+v", back)
	}
	paused := *p
	paused.PausedBy = "root"
	if !sameClusterPolicy(&paused, p) || sameClusterPolicy(nil, p) || !sameClusterPolicy(nil, nil) {
		t.Error("sameClusterPolicy")
	}
	if clusterAutoscaleFrom(&client.ClusterAutoscale{Mode: "off", MinMembers: 2, MaxMembers: 3}) != nil || clusterAutoscaleFrom(nil) != nil {
		t.Error("off reads as a block")
	}
}

// What the Controller says goes into the state - the member count too,
// unless auto-scaling decides it: then current_members says it.
func TestFromCluster(t *testing.T) {
	ctx := context.Background()
	cl := &client.Cluster{ID: "c1", Name: "edge", Placement: &client.ClusterPlacement{AntiAffinity: "required", Hypervisors: []string{"h1", "h2"}},
		Members:   []client.ClusterMember{{Name: "edge-1", MachineID: "m1"}, {Name: "edge-2", MachineID: "m2"}, {Name: "edge-3", MachineID: "m3"}},
		Autoscale: &client.ClusterAutoscale{Mode: "on", MinMembers: 2, MaxMembers: 4}}
	var diags diag.Diagnostics
	s := clusterModel{Members: types.Int64Value(2), Autoscaling: &clusterAutoscaleModel{Mode: types.StringValue("on")}}
	fromCluster(ctx, cl, &s, &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if s.Members.ValueInt64() != 2 || s.CurrentMembers.ValueInt64() != 3 || len(s.MachineIDs.Elements()) != 3 || s.AntiAffinity.ValueString() != "required" {
		t.Errorf("auto-scaled: members %v, current %v, machines %v", s.Members, s.CurrentMembers, s.MachineIDs)
	}
	s = clusterModel{Members: types.Int64Value(2)}
	fromCluster(ctx, cl, &s, &diags)
	if s.Members.ValueInt64() != 3 {
		t.Errorf("not auto-scaled, a member added elsewhere isn't drift: %v", s.Members)
	}
}

// The machine block is a machine's specification, its interfaces by DHCP.
func TestClusterSpec(t *testing.T) {
	var diags diag.Diagnostics
	m := &clusterMachineModel{VCPUs: types.Int64Value(1), MemoryMiB: types.Int64Value(512), Version: types.StringValue("v2026.10.24-6"),
		Extensions: types.SetNull(types.StringType), DNS: types.ListNull(types.StringType), NTP: types.ListNull(types.StringType),
		Interfaces: []clusterInterfaceModel{{Network: types.StringValue("lab-mgmt"), Name: types.StringValue("eth0")}}}
	spec := clusterSpec(context.Background(), m, &diags)
	if diags.HasError() || spec.VCPUs != 1 || spec.MemoryMiB != 512 || spec.Version != "v2026.10.24-6" || len(spec.NICs) != 1 || spec.NICs[0].Mode != "dhcp" || spec.Name != "" || spec.HypervisorID != "" {
		t.Errorf("spec: %+v %v", spec, diags)
	}
}
