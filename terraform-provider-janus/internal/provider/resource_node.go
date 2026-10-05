package provider

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// janus_node: a Janus node the Controller creates on one of its
// hypervisors, admitted on its registration token - and changes in place
// (network, size, version, extensions) or replaces (name, hypervisor,
// image, interfaces' networks/names/MACs).

type nodeResource struct{ c *client.Client }

func NewNodeResource() resource.Resource { return &nodeResource{} }

type imageModel struct {
	URL    types.String `tfsdk:"url"`
	SHA256 types.String `tfsdk:"sha256"`
}

type interfaceModel struct {
	Network   types.String `tfsdk:"network"`
	Name      types.String `tfsdk:"name"`
	Mode      types.String `tfsdk:"mode"`
	Addresses types.List   `tfsdk:"addresses"`
	Gateway   types.String `tfsdk:"gateway"`
	MAC       types.String `tfsdk:"mac"`
}

type nodeModel struct {
	ID           types.String     `tfsdk:"id"`
	Name         types.String     `tfsdk:"name"`
	HypervisorID types.String     `tfsdk:"hypervisor_id"`
	VCPUs        types.Int64      `tfsdk:"vcpus"`
	MemoryMiB    types.Int64      `tfsdk:"memory_mib"`
	Version      types.String     `tfsdk:"version"`
	Extensions   types.Set        `tfsdk:"extensions"`
	Image        *imageModel      `tfsdk:"image"`
	Interfaces   []interfaceModel `tfsdk:"interfaces"`
	DNS          types.List       `tfsdk:"dns"`
	NTP          types.List       `tfsdk:"ntp"`
	LockUI       types.Bool       `tfsdk:"lock_ui"`
	Labels       types.Map        `tfsdk:"labels"`
	NodeID       types.String     `tfsdk:"node_id"`
	NodeAddress  types.String     `tfsdk:"node_address"`
	VMName       types.String     `tfsdk:"vm_name"`
	Schematic    types.String     `tfsdk:"schematic"`
	Timeouts     timeouts.Value   `tfsdk:"timeouts"`
}

func (r *nodeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_node"
}

func (r *nodeResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A Janus node the Controller creates on one of its hypervisors. Its hardware (vCPUs, memory, interfaces), network, version and extensions change in place; a new name, hypervisor or image makes a new node.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "The machine's ID on the Controller.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true, Description: "The node's hostname; its virtual machine is named after it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"hypervisor_id": schema.StringAttribute{
				Required: true, Description: "The hypervisor to create it on (janus_hypervisor's id).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"vcpus": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(2),
				Description: "vCPUs. A change shuts the node down cleanly and starts it again.",
			},
			"memory_mib": schema.Int64Attribute{
				Optional: true, Computed: true, Default: int64default.StaticInt64(1024),
				Description: "Memory, MiB. A change shuts the node down cleanly and starts it again.",
			},
			"version": schema.StringAttribute{
				Optional: true, Computed: true,
				Description:   "The Janus release it runs (vYYYY.MM.DD[-N]). Unset: the newest when created, then whatever it runs. A change updates the node in place (A/B, checked healthy).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"extensions": schema.SetAttribute{
				Optional: true, ElementType: types.StringType,
				Description: "The image factory's extensions (keepalived, bird, nftables...). A change updates the node in place to an image built with them.",
			},
			"image": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Another disk image instead of the release's: a mirror, a development build. Replaces version and extensions; a change makes a new node.",
				Attributes: map[string]schema.Attribute{
					"url":    schema.StringAttribute{Required: true, Description: "The qcow2 image's URL."},
					"sha256": schema.StringAttribute{Required: true, Description: "Its SHA-256."},
				},
				PlanModifiers: []planmodifier.Object{objectplanmodifier.RequiresReplace()},
			},
			"interfaces": schema.ListNestedAttribute{
				Required:    true,
				Description: "Its network interfaces, matched by MAC address and named on the node. All of it changes in place: addresses, gateways and modes on trial then confirmed; an interface added, removed or moved to another network with a clean restart. An interface keeps its MAC by its name; the one the Controller reaches the node through can't be removed or moved.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"network": schema.StringAttribute{Required: true, Description: "One of the hypervisor's networks."},
					"name":    schema.StringAttribute{Required: true, Description: "The interface's name on the node (mgmt, front...)."},
					"mode": schema.StringAttribute{
						Optional: true, Computed: true, Default: stringdefault.StaticString("dhcp"),
						Description: "static, dhcp or none (up, no address). Prefer static: the kernel's DHCP lease is never renewed.",
					},
					"addresses": schema.ListAttribute{Optional: true, ElementType: types.StringType, Description: "Static mode: addresses in CIDR form."},
					"gateway":   schema.StringAttribute{Optional: true, Description: "Static mode: the default gateway through this interface."},
					"mac": schema.StringAttribute{
						Optional: true, Computed: true,
						Description: "Its MAC address; chosen by the Controller when unset, and kept by the interface's name afterwards.",
					},
				}},
				PlanModifiers: []planmodifier.List{interfaceMACs{}},
			},
			"dns": schema.ListAttribute{Optional: true, ElementType: types.StringType, Description: "DNS servers; unset: DHCP's."},
			"ntp": schema.ListAttribute{Optional: true, ElementType: types.StringType, Description: "NTP servers (at most two); unset: DHCP's, else pool.ntp.org."},
			"lock_ui": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				Description: "Lock the node against changes from the Controller's pages (its hardware, network, version and extensions, destroying it) - they'd be undone by the next apply. Its pages still show it, restart it, open its console. Released on the Controller, it's locked again by the next apply.",
			},
			"labels": schema.MapAttribute{
				ElementType: types.StringType, Optional: true,
				Description: "The node's labels (team = \"web\"): the Controller's grants and scoped tokens pick nodes by them. Unset, Terraform leaves them alone; set, it owns them - an empty map clears them.",
			},
			"node_id": schema.StringAttribute{
				Computed: true, Description: "The node's ID on the Controller once it registered.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"node_address": schema.StringAttribute{Computed: true, Description: "The node's gRPC address (ip:9505)."},
			"vm_name": schema.StringAttribute{
				Computed: true, Description: "Its virtual machine's name on the hypervisor.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"schematic": schema.StringAttribute{Computed: true, Description: "The image schematic it runs (docs/image-factory.md)."},
			"timeouts":  timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

// interfaceMACs keeps each interface's MAC address from one plan to the
// next by the interface's name (or, renamed, by its position on the same
// network): with positions alone, removing the first interface would
// give the second one the first one's MAC - and the Controller would
// remove the wrong one. An interface with no MAC to keep is a new one:
// the Controller chooses it.
type interfaceMACs struct{}

func (interfaceMACs) Description(context.Context) string {
	return "keeps each interface's MAC address by its name"
}

func (m interfaceMACs) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (interfaceMACs) PlanModifyList(ctx context.Context, req planmodifier.ListRequest, resp *planmodifier.ListResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsUnknown() || req.PlanValue.IsNull() {
		return
	}
	var state, plan []interfaceModel
	resp.Diagnostics.Append(req.StateValue.ElementsAs(ctx, &state, false)...)
	resp.Diagnostics.Append(req.PlanValue.ElementsAs(ctx, &plan, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	keepMACs(state, plan)
	v, d := types.ListValueFrom(ctx, req.PlanValue.ElementType(ctx), plan)
	resp.Diagnostics.Append(d...)
	resp.PlanValue = v
}

// keepMACs gives each planned interface without a MAC of its own the one
// it has now: the interface with its name, else the one at its position
// on the same network; none - a new interface - stays unknown.
func keepMACs(state, plan []interfaceModel) {
	used := map[int]bool{}
	known := func(v types.String) bool { return !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" }
	for i := range plan {
		if known(plan[i].MAC) {
			for j := range state {
				if state[j].MAC.Equal(plan[i].MAC) {
					used[j] = true
				}
			}
		}
	}
	for i := range plan {
		if known(plan[i].MAC) {
			continue
		}
		j := -1
		for k := range state {
			if !used[k] && state[k].Name.Equal(plan[i].Name) {
				j = k
				break
			}
		}
		if j < 0 && i < len(state) && !used[i] && state[i].Network.Equal(plan[i].Network) {
			j = i
		}
		if j < 0 {
			plan[i].MAC = types.StringUnknown()
			continue
		}
		used[j] = true
		plan[i].MAC = state[j].MAC
	}
}

func (r *nodeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *nodeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// --- the model <-> the API ---

func stringsOf(ctx context.Context, v interface {
	IsNull() bool
	IsUnknown() bool
	ElementsAs(context.Context, any, bool) diag.Diagnostics
}, diags *diag.Diagnostics) []string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(v.ElementsAs(ctx, &out, false)...)
	return out
}

func strOrEmpty(v types.String) string {
	if v.IsNull() || v.IsUnknown() {
		return ""
	}
	return v.ValueString()
}

func specFromPlan(ctx context.Context, m *nodeModel, diags *diag.Diagnostics) client.MachineSpec {
	spec := client.MachineSpec{
		Name:         m.Name.ValueString(),
		HypervisorID: m.HypervisorID.ValueString(),
		VCPUs:        int(m.VCPUs.ValueInt64()),
		MemoryMiB:    int(m.MemoryMiB.ValueInt64()),
		Version:      strOrEmpty(m.Version),
		Extensions:   stringsOf(ctx, m.Extensions, diags),
		DNS:          stringsOf(ctx, m.DNS, diags),
		NTP:          stringsOf(ctx, m.NTP, diags),
		ManagedBy:    "terraform",
		Locked:       m.LockUI.ValueBool(),
	}
	if m.Image != nil {
		spec.Image = &client.ImageSource{URL: m.Image.URL.ValueString(), SHA256: m.Image.SHA256.ValueString()}
	}
	spec.NICs = nicsFromPlan(ctx, m.Interfaces, diags)
	return spec
}

func nicsFromPlan(ctx context.Context, ifaces []interfaceModel, diags *diag.Diagnostics) []client.NIC {
	out := make([]client.NIC, 0, len(ifaces))
	for _, i := range ifaces {
		out = append(out, client.NIC{
			Network:   i.Network.ValueString(),
			Name:      i.Name.ValueString(),
			MAC:       strOrEmpty(i.MAC),
			Mode:      strOrEmpty(i.Mode),
			Addresses: stringsOf(ctx, i.Addresses, diags),
			Gateway:   strOrEmpty(i.Gateway),
		})
	}
	return out
}

// listOf is values as a list - null when empty and the prior value was
// null too, so an unset attribute stays unset.
func listOf(values []string, prior types.List) types.List {
	if len(values) == 0 && (prior.IsNull() || prior.IsUnknown() || len(prior.Elements()) > 0) {
		return types.ListNull(types.StringType)
	}
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	l, _ := types.ListValue(types.StringType, elems)
	return l
}

func setOf(values []string, prior types.Set) types.Set {
	if len(values) == 0 && (prior.IsNull() || prior.IsUnknown() || len(prior.Elements()) > 0) {
		return types.SetNull(types.StringType)
	}
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	s, _ := types.SetValue(types.StringType, elems)
	return s
}

func strOrNull(v string, prior types.String) types.String {
	if v == "" && (prior.IsNull() || prior.IsUnknown()) {
		return types.StringNull()
	}
	return types.StringValue(v)
}

// fromMachine fills the model from what the Controller says, keeping
// how the configuration wrote what it left unset.
func fromMachine(m *client.Machine, s *nodeModel) {
	s.ID = types.StringValue(m.ID)
	s.Name = types.StringValue(m.Spec.Name)
	s.HypervisorID = types.StringValue(m.Spec.HypervisorID)
	s.VCPUs = types.Int64Value(int64(m.Spec.VCPUs))
	s.MemoryMiB = types.Int64Value(int64(m.Spec.MemoryMiB))
	switch {
	case m.Version != "":
		s.Version = types.StringValue(m.Version)
	case m.Spec.Version != "":
		s.Version = types.StringValue(m.Spec.Version)
	default:
		s.Version = types.StringNull()
	}
	s.Extensions = setOf(m.Spec.Extensions, s.Extensions)
	if m.Spec.Image != nil {
		s.Image = &imageModel{URL: types.StringValue(m.Spec.Image.URL), SHA256: types.StringValue(m.Spec.Image.SHA256)}
	} else {
		s.Image = nil
	}
	prior := s.Interfaces
	s.Interfaces = make([]interfaceModel, 0, len(m.Spec.NICs))
	for i, n := range m.Spec.NICs {
		var p interfaceModel
		if i < len(prior) {
			p = prior[i]
		} else {
			p = interfaceModel{Addresses: types.ListNull(types.StringType), Gateway: types.StringNull()}
		}
		s.Interfaces = append(s.Interfaces, interfaceModel{
			Network:   types.StringValue(n.Network),
			Name:      types.StringValue(n.Name),
			Mode:      types.StringValue(n.Mode),
			Addresses: listOf(n.Addresses, p.Addresses),
			Gateway:   strOrNull(n.Gateway, p.Gateway),
			MAC:       types.StringValue(n.MAC),
		})
	}
	s.DNS = listOf(m.Spec.DNS, s.DNS)
	s.NTP = listOf(m.Spec.NTP, s.NTP)
	s.LockUI = types.BoolValue(m.Spec.Locked)
	s.NodeID = strOrNull(m.NodeID, types.StringNull())
	s.NodeAddress = strOrNull(m.NodeAddress, types.StringNull())
	s.VMName = strOrNull(m.VMName, types.StringNull())
	s.Schematic = strOrNull(m.Schematic, types.StringNull())
}

// updateFor is the PATCH that takes state to plan - nil when nothing
// changes in place.
func updateFor(ctx context.Context, state, plan *nodeModel, diags *diag.Diagnostics) *client.MachineUpdate {
	u := &client.MachineUpdate{}
	changed := false
	if !plan.VCPUs.Equal(state.VCPUs) {
		v := int(plan.VCPUs.ValueInt64())
		u.VCPUs, changed = &v, true
	}
	if !plan.MemoryMiB.Equal(state.MemoryMiB) {
		v := int(plan.MemoryMiB.ValueInt64())
		u.MemoryMiB, changed = &v, true
	}
	if !plan.Version.IsUnknown() && !plan.Version.IsNull() && !plan.Version.Equal(state.Version) {
		v := plan.Version.ValueString()
		u.Version, changed = &v, true
	}
	pe, se := stringsOf(ctx, plan.Extensions, diags), stringsOf(ctx, state.Extensions, diags)
	slices.Sort(pe)
	slices.Sort(se)
	if !slices.Equal(pe, se) {
		u.Extensions, changed = &pe, true
	}
	pn, sn := nicsFromPlan(ctx, plan.Interfaces, diags), nicsFromPlan(ctx, state.Interfaces, diags)
	for i := range pn {
		if pn[i].MAC == "" && i < len(sn) {
			pn[i].MAC = sn[i].MAC
		}
	}
	if !slices.EqualFunc(pn, sn, func(a, b client.NIC) bool {
		return a.Network == b.Network && a.Name == b.Name && a.MAC == b.MAC && a.Mode == b.Mode && slices.Equal(a.Addresses, b.Addresses) && a.Gateway == b.Gateway
	}) {
		u.NICs, changed = &pn, true
	}
	if pd, sd := stringsOf(ctx, plan.DNS, diags), stringsOf(ctx, state.DNS, diags); !slices.Equal(pd, sd) {
		if pd == nil {
			pd = []string{}
		}
		u.DNS, changed = &pd, true
	}
	if pt, st := stringsOf(ctx, plan.NTP, diags), stringsOf(ctx, state.NTP, diags); !slices.Equal(pt, st) {
		if pt == nil {
			pt = []string{}
		}
		u.NTP, changed = &pt, true
	}
	if !plan.LockUI.Equal(state.LockUI) {
		v := plan.LockUI.ValueBool()
		u.Locked, changed = &v, true
	}
	if changed {
		by := "terraform"
		u.ManagedBy = &by // an imported node becomes Terraform's
	}
	if !changed {
		return nil
	}
	return u
}

// --- CRUD ---

func (r *nodeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan nodeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := plan.Timeouts.Create(ctx, 30*time.Minute)
	resp.Diagnostics.Append(d...)
	spec := specFromPlan(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	m, err := r.c.CreateMachine(ctx, spec)
	if err != nil {
		resp.Diagnostics.AddError("Creating the node", err.Error())
		return
	}
	// Recorded at once: a node whose creation fails below is tainted,
	// destroyed and created again by the next apply.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), m.ID)...)
	m, err = r.c.WaitMachine(ctx, m.ID, func(m *client.Machine) bool { return m.Phase == "ready" || m.Phase == "failed" })
	if err == nil && m == nil {
		err = fmt.Errorf("the machine disappeared")
	}
	if err != nil {
		resp.Diagnostics.AddError("Creating the node", err.Error())
		return
	}
	fromMachine(m, &plan)
	if m.Phase != "failed" {
		r.putLabels(ctx, plan.NodeID.ValueString(), plan.Labels, &resp.Diagnostics)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if m.Phase == "failed" {
		resp.Diagnostics.AddError("Creating the node", fmt.Sprintf("%s (its console on the Controller may say why)", m.Error))
	}
}

func (r *nodeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state nodeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m, err := r.c.RefreshMachine(ctx, state.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the node", err.Error())
		return
	}
	fromMachine(m, &state)
	if !state.Labels.IsNull() && state.NodeID.ValueString() != "" {
		n, err := r.c.FindNode(ctx, state.NodeID.ValueString())
		switch {
		case err == nil:
			l, d := types.MapValueFrom(ctx, types.StringType, n.Labels)
			resp.Diagnostics.Append(d...)
			state.Labels = l
		case !client.IsNotFound(err):
			resp.Diagnostics.AddError("Reading the node's labels", err.Error())
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// putLabels gives the node labels - when Terraform owns them (set).
func (r *nodeResource) putLabels(ctx context.Context, nodeID string, labels types.Map, diags *diag.Diagnostics) {
	if labels.IsNull() || labels.IsUnknown() || nodeID == "" {
		return
	}
	l := map[string]string{}
	diags.Append(labels.ElementsAs(ctx, &l, false)...)
	if diags.HasError() {
		return
	}
	if _, err := r.c.SetNodeLabels(ctx, nodeID, l); err != nil {
		diags.AddError("Setting the node's labels", err.Error())
	}
}

func (r *nodeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state nodeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := plan.Timeouts.Update(ctx, 30*time.Minute)
	resp.Diagnostics.Append(d...)
	u := updateFor(ctx, &state, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := state.ID.ValueString()
	if u != nil {
		if _, err := r.c.UpdateMachine(ctx, id, *u); err != nil {
			resp.Diagnostics.AddError("Changing the node", err.Error())
			return
		}
	}
	m, err := r.c.WaitMachine(ctx, id, func(m *client.Machine) bool { return m.Phase != "updating" })
	if err == nil && m == nil {
		err = fmt.Errorf("the machine disappeared")
	}
	if err != nil {
		resp.Diagnostics.AddError("Changing the node", err.Error())
		return
	}
	plan.Timeouts = state.Timeouts
	fromMachine(m, &plan)
	// Unset, they're no longer Terraform's: left as they are.
	if !plan.Labels.IsNull() && !plan.Labels.Equal(state.Labels) {
		r.putLabels(ctx, plan.NodeID.ValueString(), plan.Labels, &resp.Diagnostics)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if m.Error != "" {
		resp.Diagnostics.AddError("Changing the node", m.Error)
	}
}

func (r *nodeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state nodeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := state.Timeouts.Delete(ctx, 10*time.Minute)
	resp.Diagnostics.Append(d...)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id := state.ID.ValueString()
	if err := r.c.DeleteMachine(ctx, id); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Destroying the node", err.Error())
		return
	}
	m, err := r.c.WaitMachine(ctx, id, func(m *client.Machine) bool { return m.Phase == "failed" })
	if err != nil {
		resp.Diagnostics.AddError("Destroying the node", err.Error())
		return
	}
	if m != nil {
		resp.Diagnostics.AddError("Destroying the node", m.Error)
	}
}
