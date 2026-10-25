package provider

import (
	"context"
	"slices"
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

// The hook as read back: the secret kept (never read), what the
// configuration left unset unset, a secret set elsewhere showing; a
// RouterOS router's settings.
func TestClusterBalancerFrom(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics
	if clusterBalancerFrom(ctx, nil, nil, &diags) != nil {
		t.Error("no hook: a model")
	}
	null := func() *clusterBalancerModel {
		return &clusterBalancerModel{Kind: types.StringNull(), URL: types.StringValue("https://lb/h"), CACert: types.StringNull(), Fingerprint: types.StringNull(),
			RouterUser: types.StringNull(), Prefixes: types.ListNull(types.StringType), Distance: types.Int64Null(), RoutingTable: types.StringNull(),
			AllowHTTP: types.BoolNull(), CheckGateway: types.BoolNull(), DrainDelaySeconds: types.Int64Null(), Secret: types.StringNull()}
	}
	prior := null()
	prior.Secret = types.StringValue("s3")
	got := clusterBalancerFrom(ctx, &client.ClusterBalancer{URL: "https://lb/h", HasSecret: true}, prior, &diags)
	if !sameBalancer(got, prior) || got.Secret.ValueString() != "s3" || !got.CACert.IsNull() || !got.DrainDelaySeconds.IsNull() || !got.Prefixes.IsNull() || !got.Kind.IsNull() {
		t.Errorf("read back: %+v", got)
	}
	// Changed on the Controller: the plan shows it.
	got = clusterBalancerFrom(ctx, &client.ClusterBalancer{URL: "https://lb/other", DrainDelaySeconds: 5}, prior, &diags)
	if sameBalancer(got, prior) || got.DrainDelaySeconds.ValueInt64() != 5 || !got.Secret.IsNull() {
		t.Errorf("changed elsewhere: %+v", got)
	}
	// A secret set elsewhere, none in the configuration: "" - a change.
	got = clusterBalancerFrom(ctx, &client.ClusterBalancer{URL: "https://lb/h", HasSecret: true}, null(), &diags)
	if got.Secret.IsNull() || got.Secret.Equal(null().Secret) {
		t.Errorf("a secret set elsewhere: %+v", got)
	}
	if !sameBalancer(nil, nil) || sameBalancer(prior, nil) {
		t.Error("sameBalancer with nil")
	}
	// "webhook" written: the Controller's "" is the same.
	hook := null()
	hook.Kind = types.StringValue("webhook")
	if got := clusterBalancerFrom(ctx, &client.ClusterBalancer{URL: "https://lb/h"}, hook, &diags); got.Kind.ValueString() != "webhook" || !sameBalancer(got, hook) {
		t.Errorf("kind webhook: %+v", got)
	}
	// A router: its settings as written, the fingerprint's case kept.
	ros := null()
	ros.Kind, ros.URL, ros.RouterUser = types.StringValue("routeros"), types.StringValue("https://rtr"), types.StringValue("janus-hook")
	ros.Fingerprint = types.StringValue("AB:CD")
	ros.Prefixes, _ = types.ListValueFrom(ctx, types.StringType, []string{"203.0.113.10/32"})
	ros.Secret = types.StringValue("pw")
	got = clusterBalancerFrom(ctx, &client.ClusterBalancer{Kind: "routeros", URL: "https://rtr", User: "janus-hook", Fingerprint: "ab:cd", Prefixes: []string{"203.0.113.10/32"}, HasSecret: true}, ros, &diags)
	if !sameBalancer(got, ros) || got.Fingerprint.ValueString() != "AB:CD" || !got.Distance.IsNull() || !got.RoutingTable.IsNull() || got.Secret.ValueString() != "pw" {
		t.Errorf("a router: %+v", got)
	}
	req := got.request(nil)
	if req.Kind != "routeros" || req.User != "janus-hook" || !slices.Equal(req.Prefixes, []string{"203.0.113.10/32"}) || req.Secret != nil {
		t.Errorf("its request: %+v", req)
	}
	if hook.request(nil).Kind != "" {
		t.Error(`"webhook" sent as itself, not ""`)
	}
	if diags.HasError() {
		t.Error(diags)
	}
}
