package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// janus_cluster: a cluster the Controller makes of new machines on its
// hypervisors (docs/guide/clusters.md "On new machines"), kept on hosts
// of their own by its anti-affinity - its members scaled in place, its
// auto-scaling set in place; a new name, placement or machine
// specification makes a new cluster.

type clusterResource struct{ c *client.Client }

func NewClusterResource() resource.Resource { return &clusterResource{} }

type clusterInterfaceModel struct {
	Network types.String `tfsdk:"network"`
	Name    types.String `tfsdk:"name"`
}

type clusterMachineModel struct {
	VCPUs      types.Int64             `tfsdk:"vcpus"`
	MemoryMiB  types.Int64             `tfsdk:"memory_mib"`
	Version    types.String            `tfsdk:"version"`
	Extensions types.Set               `tfsdk:"extensions"`
	HAProxy    types.String            `tfsdk:"haproxy"`
	Kernel     types.String            `tfsdk:"kernel"`
	Image      *imageModel             `tfsdk:"image"`
	Interfaces []clusterInterfaceModel `tfsdk:"interfaces"`
	DNS        types.List              `tfsdk:"dns"`
	NTP        types.List              `tfsdk:"ntp"`
}

// clusterAutoscaleModel is a cluster's horizontal auto-scaling
// (docs/guide/clusters.md "Scaling").
type clusterAutoscaleModel struct {
	Mode               types.String `tfsdk:"mode"`
	MinMembers         types.Int64  `tfsdk:"min_members"`
	MaxMembers         types.Int64  `tfsdk:"max_members"`
	MaxVCPUs           types.Int64  `tfsdk:"max_vcpus"`
	MaxMemoryMiB       types.Int64  `tfsdk:"max_memory_mib"`
	OutIdlePercent     types.Int64  `tfsdk:"out_idle_percent"`
	OutBusyPercent     types.Int64  `tfsdk:"out_busy_percent"`
	OutConnPercent     types.Int64  `tfsdk:"out_conn_percent"`
	OutAfterSeconds    types.Int64  `tfsdk:"out_after_seconds"`
	InIdlePercent      types.Int64  `tfsdk:"in_idle_percent"`
	InBusyPercent      types.Int64  `tfsdk:"in_busy_percent"`
	InConnPercent      types.Int64  `tfsdk:"in_conn_percent"`
	InAfterSeconds     types.Int64  `tfsdk:"in_after_seconds"`
	OutCooldownSeconds types.Int64  `tfsdk:"out_cooldown_seconds"`
	InCooldownSeconds  types.Int64  `tfsdk:"in_cooldown_seconds"`
	MaxStepsPerHour    types.Int64  `tfsdk:"max_steps_per_hour"`
}

// clusterBalancerModel is a cluster's balancer hook - or a RouterOS
// router whose routes the Controller keeps; Secret (the router user's
// password for one) is never read back.
type clusterBalancerModel struct {
	Kind              types.String `tfsdk:"kind"`
	URL               types.String `tfsdk:"url"`
	CACert            types.String `tfsdk:"ca_cert"`
	Fingerprint       types.String `tfsdk:"fingerprint"`
	RouterUser        types.String `tfsdk:"router_user"`
	Prefixes          types.List   `tfsdk:"prefixes"`
	Distance          types.Int64  `tfsdk:"distance"`
	RoutingTable      types.String `tfsdk:"routing_table"`
	AllowHTTP         types.Bool   `tfsdk:"allow_http"`
	CheckGateway      types.Bool   `tfsdk:"check_gateway"`
	DrainDelaySeconds types.Int64  `tfsdk:"drain_delay_seconds"`
	Secret            types.String `tfsdk:"secret"`
}

// kind is b's kind, the API's way: "" for a webhook.
func (b *clusterBalancerModel) kind() string {
	if k := b.Kind.ValueString(); k != "webhook" {
		return k
	}
	return ""
}

func (b *clusterBalancerModel) prefixes() []string {
	var out []string
	for _, e := range b.Prefixes.Elements() {
		if s, ok := e.(types.String); ok {
			out = append(out, s.ValueString())
		}
	}
	return out
}

// request is b as the API takes it, the secret sent when secret says.
func (b *clusterBalancerModel) request(secret *string) client.ClusterBalancer {
	return client.ClusterBalancer{
		Kind: b.kind(), URL: b.URL.ValueString(), CACert: b.CACert.ValueString(), Fingerprint: b.Fingerprint.ValueString(),
		User: b.RouterUser.ValueString(), Prefixes: b.prefixes(), Distance: int(b.Distance.ValueInt64()), RoutingTable: b.RoutingTable.ValueString(),
		AllowHTTP: b.AllowHTTP.ValueBool(), CheckGateway: b.CheckGateway.ValueBool(), DrainDelaySeconds: int(b.DrainDelaySeconds.ValueInt64()), Secret: secret,
	}
}

// sameBalancer: a and b ask the Controller for the same hook, their
// secrets aside.
func sameBalancer(a, b *clusterBalancerModel) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	x, y := a.request(nil), b.request(nil)
	return x.Kind == y.Kind && x.URL == y.URL && x.CACert == y.CACert && strings.EqualFold(x.Fingerprint, y.Fingerprint) && x.User == y.User &&
		slices.Equal(x.Prefixes, y.Prefixes) && x.Distance == y.Distance && x.RoutingTable == y.RoutingTable && x.AllowHTTP == y.AllowHTTP && x.CheckGateway == y.CheckGateway &&
		x.DrainDelaySeconds == y.DrainDelaySeconds
}

// optString is v as a model value: null when empty and prior left it
// unset.
func optString(v string, prior types.String) types.String {
	if v == "" && prior.IsNull() {
		return types.StringNull()
	}
	return types.StringValue(v)
}

// clusterBalancerFrom is the Controller's hook as a model, prior's
// secret kept when the Controller has one - nil when it has no hook;
// what the configuration left unset stays unset.
func clusterBalancerFrom(ctx context.Context, b *client.ClusterBalancer, prior *clusterBalancerModel, diags *diag.Diagnostics) *clusterBalancerModel {
	if b == nil {
		return nil
	}
	if prior == nil {
		prior = &clusterBalancerModel{Kind: types.StringNull(), CACert: types.StringNull(), Fingerprint: types.StringNull(), RouterUser: types.StringNull(),
			Prefixes: types.ListNull(types.StringType), Distance: types.Int64Null(), RoutingTable: types.StringNull(), AllowHTTP: types.BoolNull(),
			CheckGateway: types.BoolNull(), DrainDelaySeconds: types.Int64Null(), Secret: types.StringNull()}
	}
	m := &clusterBalancerModel{
		Kind: optString(b.Kind, prior.Kind), URL: types.StringValue(b.URL), CACert: optString(b.CACert, prior.CACert),
		Fingerprint: optString(b.Fingerprint, prior.Fingerprint), RouterUser: optString(b.User, prior.RouterUser),
		RoutingTable: optString(b.RoutingTable, prior.RoutingTable), Prefixes: prior.Prefixes, Distance: prior.Distance,
		AllowHTTP: prior.AllowHTTP, CheckGateway: prior.CheckGateway, DrainDelaySeconds: prior.DrainDelaySeconds, Secret: types.StringNull(),
	}
	if b.Kind == "" && prior.Kind.ValueString() == "webhook" {
		m.Kind = prior.Kind
	}
	if prior.Fingerprint.ValueString() != "" && strings.EqualFold(prior.Fingerprint.ValueString(), b.Fingerprint) {
		m.Fingerprint = prior.Fingerprint // its case, colons as written
	}
	if len(b.Prefixes) > 0 || !prior.Prefixes.IsNull() {
		l, d := types.ListValueFrom(ctx, types.StringType, b.Prefixes)
		diags.Append(d...)
		m.Prefixes = l
	}
	if b.Distance != 0 || !prior.Distance.IsNull() {
		m.Distance = types.Int64Value(int64(b.Distance))
	}
	if b.AllowHTTP || !prior.AllowHTTP.IsNull() {
		m.AllowHTTP = types.BoolValue(b.AllowHTTP)
	}
	if b.CheckGateway || !prior.CheckGateway.IsNull() {
		m.CheckGateway = types.BoolValue(b.CheckGateway)
	}
	if b.DrainDelaySeconds != 0 || !prior.DrainDelaySeconds.IsNull() {
		m.DrainDelaySeconds = types.Int64Value(int64(b.DrainDelaySeconds))
	}
	if b.HasSecret {
		m.Secret = types.StringValue("") // set elsewhere: a plan with a secret shows the change
		if !prior.Secret.IsNull() {
			m.Secret = prior.Secret
		}
	}
	return m
}

type clusterModel struct {
	ID             types.String           `tfsdk:"id"`
	Name           types.String           `tfsdk:"name"`
	Hypervisors    types.List             `tfsdk:"hypervisors"`
	AntiAffinity   types.String           `tfsdk:"anti_affinity"`
	Members        types.Int64            `tfsdk:"members"`
	Machine        *clusterMachineModel   `tfsdk:"machine"`
	Autoscaling    *clusterAutoscaleModel `tfsdk:"autoscaling"`
	Balancer       *clusterBalancerModel  `tfsdk:"balancer"`
	CurrentMembers types.Int64            `tfsdk:"current_members"`
	MemberNames    types.List             `tfsdk:"member_names"`
	MachineIDs     types.List             `tfsdk:"machine_ids"`
	Timeouts       timeouts.Value         `tfsdk:"timeouts"`
}

// fields are the policy's optional numbers, with the API's field each is.
func (a *clusterAutoscaleModel) fields(p *client.ClusterAutoscale) []struct {
	v *types.Int64
	f *int
} {
	return []struct {
		v *types.Int64
		f *int
	}{
		{&a.MaxVCPUs, &p.MaxVCPUs}, {&a.MaxMemoryMiB, &p.MaxMemoryMiB},
		{&a.OutIdlePercent, &p.OutIdlePercent}, {&a.OutBusyPercent, &p.OutBusyPercent}, {&a.OutConnPercent, &p.OutConnPercent},
		{&a.OutAfterSeconds, &p.OutAfterSeconds}, {&a.InIdlePercent, &p.InIdlePercent}, {&a.InBusyPercent, &p.InBusyPercent},
		{&a.InConnPercent, &p.InConnPercent}, {&a.InAfterSeconds, &p.InAfterSeconds}, {&a.OutCooldownSeconds, &p.OutCooldownSeconds},
		{&a.InCooldownSeconds, &p.InCooldownSeconds}, {&a.MaxStepsPerHour, &p.MaxStepsPerHour},
	}
}

// policy is the model as the Controller takes it - unset numbers 0, its
// defaults.
func (a *clusterAutoscaleModel) policy() *client.ClusterAutoscale {
	if a == nil {
		return nil
	}
	p := &client.ClusterAutoscale{Mode: a.Mode.ValueString(), MinMembers: int(a.MinMembers.ValueInt64()), MaxMembers: int(a.MaxMembers.ValueInt64())}
	for _, f := range a.fields(p) {
		if !f.v.IsNull() && !f.v.IsUnknown() {
			*f.f = int(f.v.ValueInt64())
		}
	}
	return p
}

// clusterAutoscaleFrom is the Controller's policy as a model - nil when
// it has none or it is off, a number at 0 unset.
func clusterAutoscaleFrom(p *client.ClusterAutoscale) *clusterAutoscaleModel {
	if p == nil || p.Mode == "off" {
		return nil
	}
	a := &clusterAutoscaleModel{Mode: types.StringValue(p.Mode), MinMembers: types.Int64Value(int64(p.MinMembers)), MaxMembers: types.Int64Value(int64(p.MaxMembers))}
	for _, f := range a.fields(p) {
		*f.v = types.Int64Null()
		if *f.f != 0 {
			*f.v = types.Int64Value(int64(*f.f))
		}
	}
	return a
}

// sameClusterPolicy compares two policies, its pause aside.
func sameClusterPolicy(a, b *client.ClusterAutoscale) bool {
	if a == nil || b == nil {
		return a == b
	}
	x, y := *a, *b
	x.PausedBy, y.PausedBy = "", ""
	return x == y
}

// autoscaled reports whether the Controller decides the member count.
func (m *clusterModel) autoscaled() bool {
	return m.Autoscaling != nil && m.Autoscaling.Mode.ValueString() == "on"
}

func (r *clusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (r *clusterResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A cluster the Controller makes of new machines on its hypervisors, each on a physical host of its own by default (docs/guide/clusters.md). Its members and auto-scaling change in place; a new name, placement or machine specification makes a new cluster. Destroying it destroys its machines.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "The cluster's ID on the Controller.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true, Description: "The cluster's name; its machines are named after it (<name>-1, <name>-2...).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"hypervisors": schema.ListAttribute{
				Required: true, ElementType: types.StringType,
				Description:   "The hypervisors its machines may go to (janus_hypervisor's or janus_proxmox_hypervisor's ids), in order of preference. Two records reaching one physical host count as one.",
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
			"anti_affinity": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("required"),
				Validators:    []validator.String{oneOf{"required", "preferred", "off"}},
				Description:   "\"required\" (the default): each machine on a physical host of its own - refused before anything is made when there are too few; \"preferred\": hosts of their own first, then the least busy (the cluster's page says which share one); \"off\": the hypervisors in turn.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"members": schema.Int64Attribute{
				Required: true, Validators: []validator.Int64{between{1, 16}},
				Description: "How many members. A change scales the cluster in place: a member more is a new machine like the newest, placed; a member less the newest, drained, out, its machine destroyed. With autoscaling on, the count it starts with: the Controller decides afterwards (current_members says it), and a change here does nothing.",
			},
			"machine": schema.SingleNestedAttribute{
				Required:      true,
				Description:   "Every member's machine - a janus_node's, without its name and hypervisor. A change makes a new cluster (recycle the members from the Controller to move to a new release member by member).",
				PlanModifiers: []planmodifier.Object{objectplanmodifier.RequiresReplace()},
				Attributes: map[string]schema.Attribute{
					"vcpus":      schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(2), Description: "vCPUs."},
					"memory_mib": schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(1024), Description: "Memory, MiB."},
					"version":    schema.StringAttribute{Optional: true, Description: "The Janus release (vYYYY.MM.DD[-N]). Unset: the newest when each machine is made."},
					"extensions": schema.SetAttribute{Optional: true, ElementType: types.StringType, Description: "The image factory's extensions (bird, nftables...)."},
					"haproxy":    schema.StringAttribute{Optional: true, Description: "The HAProxy LTS branch (\"3.2\"). Unset: each release's default."},
					"kernel":     schema.StringAttribute{Optional: true, Description: "The kernel track (\"stable\", \"longterm\"). Unset: each release's default."},
					"image": schema.SingleNestedAttribute{
						Optional:    true,
						Description: "Another disk image instead of the release's. Replaces version, extensions, haproxy and kernel.",
						Attributes: map[string]schema.Attribute{
							"url":    schema.StringAttribute{Required: true, Description: "The qcow2 image's URL."},
							"sha256": schema.StringAttribute{Required: true, Description: "Its SHA-256."},
						},
					},
					"interfaces": schema.ListNestedAttribute{
						Required:    true,
						Description: "Its network interfaces, by DHCP: a static address can't go to every member.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"network": schema.StringAttribute{Required: true, Description: "A network every hypervisor given has."},
							"name":    schema.StringAttribute{Required: true, Description: "The interface's name on the node."},
						}},
					},
					"dns": schema.ListAttribute{Optional: true, ElementType: types.StringType, Description: "DNS servers; unset: DHCP's."},
					"ntp": schema.ListAttribute{Optional: true, ElementType: types.StringType, Description: "NTP servers; unset: DHCP's, else pool.ntp.org."},
				},
			},
			"autoscaling": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Horizontal auto-scaling: a member more when the load rises, one less when it falls (docs/guide/clusters.md \"Scaling\"). Not for a cluster managing VRRP. Removing it turns it off.",
				Attributes: map[string]schema.Attribute{
					"mode": schema.StringAttribute{
						Required: true, Validators: []validator.String{oneOf{"observe", "on", "off"}},
						Description: "\"observe\": journal what it would do - start with it; \"on\": scale; \"off\".",
					},
					"min_members":          schema.Int64Attribute{Required: true, Description: "The fewest members (2 at least)."},
					"max_members":          schema.Int64Attribute{Required: true, Description: "The most members (16 at most)."},
					"max_vcpus":            schema.Int64Attribute{Optional: true, Description: "The members' vCPUs in all, at most."},
					"max_memory_mib":       schema.Int64Attribute{Optional: true, Description: "The members' memory in all, MiB, at most."},
					"out_idle_percent":     schema.Int64Attribute{Optional: true, Description: "A member more when HAProxy idles less than this on average (default 30)..."},
					"out_busy_percent":     schema.Int64Attribute{Optional: true, Description: "... or the CPUs are busier than this (default 75)..."},
					"out_conn_percent":     schema.Int64Attribute{Optional: true, Description: "... or the connections above this share of maxconn (default 70)..."},
					"out_after_seconds":    schema.Int64Attribute{Optional: true, Description: "... over this long (default 300)."},
					"in_idle_percent":      schema.Int64Attribute{Optional: true, Description: "A member less when, with one fewer, HAProxy would idle at least this (default 50)..."},
					"in_busy_percent":      schema.Int64Attribute{Optional: true, Description: "... the CPUs stay below this (default 40)..."},
					"in_conn_percent":      schema.Int64Attribute{Optional: true, Description: "... and the connections below this share of maxconn (default 35)..."},
					"in_after_seconds":     schema.Int64Attribute{Optional: true, Description: "... throughout this long (default 1800)."},
					"out_cooldown_seconds": schema.Int64Attribute{Optional: true, Description: "At least this long after a member added (default 600)."},
					"in_cooldown_seconds":  schema.Int64Attribute{Optional: true, Description: "At least this long after a member taken out (default 1800)."},
					"max_steps_per_hour":   schema.Int64Attribute{Optional: true, Description: "Steps an hour, at most (default 2)."},
				},
			},
			"balancer": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "A balancer in front - a cloud load balancer, a router's ECMP routes: the hook telling it which members take the traffic, before one is drained or removed and once one serves; or a RouterOS router whose ECMP routes the Controller keeps itself (docs/guide/clusters.md \"A balancer in front\", \"RouterOS\"). Removing it removes the hook - a router's routes stay.",
				Attributes: map[string]schema.Attribute{
					"kind": schema.StringAttribute{
						Optional: true, Validators: []validator.String{oneOf{"webhook", "routeros"}},
						Description: "\"webhook\" (unset: one) or \"routeros\".",
					},
					"url":                 schema.StringAttribute{Required: true, Description: "The hook: an http(s) URL the members are POSTed to; routeros: the router's (https://router)."},
					"ca_cert":             schema.StringAttribute{Optional: true, Description: "The only CA its HTTPS certificate may chain to (PEM); unset: the system's."},
					"fingerprint":         schema.StringAttribute{Optional: true, Description: "Its HTTPS certificate's SHA-256, pinned - a router's self-signed one (RouterOS: /certificate print detail, fingerprint)."},
					"router_user":         schema.StringAttribute{Optional: true, Description: "routeros: the router's user the Controller signs in as - its password is secret."},
					"prefixes":            schema.ListAttribute{Optional: true, ElementType: types.StringType, Description: "routeros: the service prefixes the members carry (203.0.113.10/32): a route each per member in service."},
					"distance":            schema.Int64Attribute{Optional: true, Description: "routeros: the routes' distance (1)."},
					"routing_table":       schema.StringAttribute{Optional: true, Description: "routeros: the routes' routing table (main)."},
					"allow_http":          schema.BoolAttribute{Optional: true, Description: "routeros: a lab's router over plain HTTP - its password goes with each call."},
					"check_gateway":       schema.BoolAttribute{Optional: true, Description: "routeros: the router pings each member (check-gateway=ping) and leaves out one that doesn't answer, before any drain or check."},
					"drain_delay_seconds": schema.Int64Attribute{Optional: true, Description: "How long a member serves on once the balancer was told it goes (0 to 3600)."},
					"secret":              schema.StringAttribute{Optional: true, Sensitive: true, Description: "Signs each call (X-Janus-Signature-256: HMAC-SHA256 of the body); routeros: the router user's password. Write-only: never read back."},
				},
			},
			"current_members": schema.Int64Attribute{Computed: true, Description: "The members it has now - what auto-scaling made it."},
			"member_names":    schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Its members' names."},
			"machine_ids":     schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Its members' machines' IDs on the Controller."},
			"timeouts":        timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

func (r *clusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("%T", req.ProviderData))
		return
	}
	r.c = c
}

// between refuses a number outside [min, max], at plan time.
type between struct{ min, max int64 }

func (b between) Description(context.Context) string {
	return fmt.Sprintf("from %d to %d", b.min, b.max)
}

func (b between) MarkdownDescription(ctx context.Context) string { return b.Description(ctx) }

func (b between) ValidateInt64(ctx context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if v := req.ConfigValue.ValueInt64(); v < b.min || v > b.max {
		resp.Diagnostics.AddAttributeError(req.Path, "Out of range", fmt.Sprintf("%d: %s", v, b.Description(ctx)))
	}
}

// --- the model <-> the API ---

// clusterSpec is the machine block as a machine specification.
func clusterSpec(ctx context.Context, m *clusterMachineModel, diags *diag.Diagnostics) client.MachineSpec {
	spec := client.MachineSpec{
		VCPUs: int(m.VCPUs.ValueInt64()), MemoryMiB: int(m.MemoryMiB.ValueInt64()),
		Version: strOrEmpty(m.Version), HAProxy: strOrEmpty(m.HAProxy), Kernel: strOrEmpty(m.Kernel),
		Extensions: stringsOf(ctx, m.Extensions, diags), DNS: stringsOf(ctx, m.DNS, diags), NTP: stringsOf(ctx, m.NTP, diags),
	}
	if m.Image != nil {
		spec.Image = &client.ImageSource{URL: m.Image.URL.ValueString(), SHA256: m.Image.SHA256.ValueString()}
	}
	for _, i := range m.Interfaces {
		spec.NICs = append(spec.NICs, client.NIC{Network: i.Network.ValueString(), Name: i.Name.ValueString(), Mode: "dhcp"})
	}
	return spec
}

// fromCluster sets what the Controller says into s: the member count
// (unless auto-scaling decides it), the policy, the members.
func fromCluster(ctx context.Context, cl *client.Cluster, s *clusterModel, diags *diag.Diagnostics) {
	s.ID, s.Name = types.StringValue(cl.ID), types.StringValue(cl.Name)
	if cl.Placement != nil {
		s.AntiAffinity = types.StringValue(cl.Placement.AntiAffinity)
		h, d := types.ListValueFrom(ctx, types.StringType, cl.Placement.Hypervisors)
		diags.Append(d...)
		s.Hypervisors = h
	}
	s.CurrentMembers = types.Int64Value(int64(len(cl.Members)))
	if !s.autoscaled() {
		s.Members = s.CurrentMembers
	}
	s.Autoscaling = clusterAutoscaleFrom(cl.Autoscale)
	var names, machines []string
	for _, m := range cl.Members {
		names, machines = append(names, m.Name), append(machines, m.MachineID)
	}
	n, d := types.ListValueFrom(ctx, types.StringType, names)
	diags.Append(d...)
	mi, d := types.ListValueFrom(ctx, types.StringType, machines)
	diags.Append(d...)
	s.MemberNames, s.MachineIDs = n, mi
}

// wait waits until no operation runs on cluster id, then says what the
// last one left as an error.
func (r *clusterResource) wait(ctx context.Context, id string) (*client.Cluster, error) {
	cl, err := r.c.WaitCluster(ctx, id, (*client.Cluster).Idle)
	if err == nil && cl == nil {
		err = fmt.Errorf("the cluster disappeared")
	}
	if err != nil {
		return nil, err
	}
	if cl.Error != "" {
		return cl, fmt.Errorf("%s", cl.Error)
	}
	return cl, nil
}

// --- CRUD ---

func (r *clusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := plan.Timeouts.Create(ctx, 40*time.Minute)
	resp.Diagnostics.Append(d...)
	m := client.ClusterMachines{
		Count: int(plan.Members.ValueInt64()), Spec: clusterSpec(ctx, plan.Machine, &resp.Diagnostics),
		Hypervisors: stringsOf(ctx, plan.Hypervisors, &resp.Diagnostics), AntiAffinity: plan.AntiAffinity.ValueString(),
	}
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cl, err := r.c.CreateCluster(ctx, plan.Name.ValueString(), m)
	if err != nil {
		resp.Diagnostics.AddError("Creating the cluster", err.Error())
		return
	}
	// Recorded at once: a cluster whose creation fails below is tainted,
	// destroyed - its machines too - and made again by the next apply.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), cl.ID)...)
	cl, err = r.wait(ctx, cl.ID)
	if err != nil {
		resp.Diagnostics.AddError("Creating the cluster", err.Error())
		return
	}
	if p := plan.Autoscaling.policy(); p != nil {
		if cl, err = r.c.SetClusterAutoscale(ctx, cl.ID, *p); err != nil {
			resp.Diagnostics.AddError("Setting the cluster's auto-scaling", err.Error())
			return
		}
	}
	if b := plan.Balancer; b != nil {
		var secret *string
		if !b.Secret.IsNull() {
			s := b.Secret.ValueString()
			secret = &s
		}
		if cl, err = r.c.SetClusterBalancer(ctx, cl.ID, b.request(secret)); err != nil {
			resp.Diagnostics.AddError("Setting the cluster's balancer hook", err.Error())
			return
		}
		warnBalancer(cl, &resp.Diagnostics)
	}
	autoscaling, balancer := plan.Autoscaling, plan.Balancer
	fromCluster(ctx, cl, &plan, &resp.Diagnostics)
	plan.Autoscaling, plan.Balancer = autoscaling, balancer
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// warnBalancer says when the hook, just set, wasn't reached: the
// settings are kept, the Controller calls again.
func warnBalancer(cl *client.Cluster, diags *diag.Diagnostics) {
	for _, p := range cl.Problems {
		if strings.Contains(p, "balancer in front wasn't told") {
			diags.AddWarning("The balancer hook wasn't reached", p)
		}
	}
}

func (r *clusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cl, err := r.c.Cluster(ctx, state.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the cluster", err.Error())
		return
	}
	prior, priorBalancer := state.Autoscaling, state.Balancer
	fromCluster(ctx, cl, &state, &resp.Diagnostics)
	state.Balancer = clusterBalancerFrom(ctx, cl.Balancer, priorBalancer, &resp.Diagnostics)
	// Thresholds left unset stay unset: the Controller's 0 is its default.
	if prior != nil && state.Autoscaling != nil && sameClusterPolicy(prior.policy(), state.Autoscaling.policy()) {
		state.Autoscaling = prior
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *clusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := plan.Timeouts.Update(ctx, 40*time.Minute)
	resp.Diagnostics.Append(d...)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := state.ID.ValueString()

	// The policy first: turned off or observing, the count below is
	// Terraform's to set without pausing it.
	want := plan.Autoscaling.policy()
	if want == nil && state.Autoscaling != nil {
		off := *state.Autoscaling.policy()
		off.Mode = "off"
		want = &off
	}
	if want != nil && !sameClusterPolicy(want, state.Autoscaling.policy()) {
		if _, err := r.c.SetClusterAutoscale(ctx, id, *want); err != nil {
			resp.Diagnostics.AddError("Setting the cluster's auto-scaling", err.Error())
			return
		}
	}
	switch b := plan.Balancer; {
	case b == nil && state.Balancer != nil:
		if err := r.c.DeleteClusterBalancer(ctx, id); err != nil {
			resp.Diagnostics.AddError("Removing the cluster's balancer hook", err.Error())
			return
		}
	case b != nil && (!sameBalancer(b, state.Balancer) || !b.Secret.Equal(state.Balancer.Secret)):
		var secret *string
		if state.Balancer == nil || !b.Secret.Equal(state.Balancer.Secret) {
			s := b.Secret.ValueString() // null: "" - removed
			secret = &s
		}
		cl, err := r.c.SetClusterBalancer(ctx, id, b.request(secret))
		if err != nil {
			resp.Diagnostics.AddError("Setting the cluster's balancer hook", err.Error())
			return
		}
		warnBalancer(cl, &resp.Diagnostics)
	}
	if !plan.autoscaled() && plan.Members.ValueInt64() != state.CurrentMembers.ValueInt64() {
		if err := r.c.ScaleCluster(ctx, id, int(plan.Members.ValueInt64())); err != nil {
			resp.Diagnostics.AddError("Scaling the cluster", err.Error())
			return
		}
	}
	cl, err := r.wait(ctx, id)
	if cl != nil {
		autoscaling, members, balancer := plan.Autoscaling, plan.Members, plan.Balancer
		plan.Timeouts = state.Timeouts
		fromCluster(ctx, cl, &plan, &resp.Diagnostics)
		plan.Autoscaling, plan.Members, plan.Balancer = autoscaling, members, balancer
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
	if err != nil {
		resp.Diagnostics.AddError("Changing the cluster", err.Error())
	}
}

func (r *clusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := state.Timeouts.Delete(ctx, 20*time.Minute)
	resp.Diagnostics.Append(d...)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := state.ID.ValueString()

	// Its machines, as the Controller has them now - auto-scaling's too.
	machines := stringsOf(ctx, state.MachineIDs, &resp.Diagnostics)
	cl, err := r.c.Cluster(ctx, id)
	switch {
	case err == nil:
		machines = nil
		for _, m := range cl.Members {
			if m.MachineID != "" {
				machines = append(machines, m.MachineID)
			}
		}
		if _, err := r.c.WaitCluster(ctx, id, (*client.Cluster).Idle); err != nil {
			resp.Diagnostics.AddError("Destroying the cluster", err.Error())
			return
		}
		if err := r.c.DeleteCluster(ctx, id); err != nil && !client.IsNotFound(err) {
			resp.Diagnostics.AddError("Destroying the cluster", err.Error())
			return
		}
		if cl, err := r.c.WaitCluster(ctx, id, func(*client.Cluster) bool { return false }); err != nil {
			resp.Diagnostics.AddError("Destroying the cluster", err.Error())
			return
		} else if cl != nil {
			resp.Diagnostics.AddError("Destroying the cluster", "it is still there: "+cl.Error)
			return
		}
	case !client.IsNotFound(err):
		resp.Diagnostics.AddError("Destroying the cluster", err.Error())
		return
	}
	for _, mid := range machines {
		if err := r.c.DeleteMachine(ctx, mid); err != nil && !client.IsNotFound(err) {
			resp.Diagnostics.AddError("Destroying the cluster's machine "+mid, err.Error())
			continue
		}
		m, err := r.c.WaitMachine(ctx, mid, func(m *client.Machine) bool { return m.Phase == "failed" })
		if err != nil {
			resp.Diagnostics.AddError("Destroying the cluster's machine "+mid, err.Error())
		} else if m != nil {
			resp.Diagnostics.AddError("Destroying the cluster's machine "+mid, m.Error)
		}
	}
}
