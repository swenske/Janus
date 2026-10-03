package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// janus_hypervisor: a libvirt host the Controller creates nodes on (a
// Proxmox VE node is janus_proxmox_hypervisor). It's
// trusted - its SSH host key pinned - once host_key_fingerprint is given:
// the fingerprint an operator read on the host itself
// (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub), never whatever the
// host presents. authorized_key is the line its account needs.

type hypervisorResource struct{ c *client.Client }

func NewHypervisorResource() resource.Resource { return &hypervisorResource{} }

type hypervisorModel struct {
	ID                 types.String `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	Host               types.String `tfsdk:"host"`
	User               types.String `tfsdk:"user"`
	Socket             types.String `tfsdk:"socket"`
	Pool               types.String `tfsdk:"pool"`
	Networks           types.List   `tfsdk:"networks"`
	NamePrefix         types.String `tfsdk:"name_prefix"`
	ControllerAddress  types.String `tfsdk:"controller_address"`
	HostKeyFingerprint types.String `tfsdk:"host_key_fingerprint"`
	AuthorizedKey      types.String `tfsdk:"authorized_key"`
	Trusted            types.Bool   `tfsdk:"trusted"`
}

func (r *hypervisorResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hypervisor"
}

func (r *hypervisorResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A libvirt/KVM host the Controller creates its nodes on (docs/hypervisors.md: preparing the host).",
		Attributes: map[string]schema.Attribute{
			"id":                 schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":               schema.StringAttribute{Required: true, Description: "Its name on the Controller."},
			"host":               schema.StringAttribute{Required: true, Description: "Its SSH server, host or host:port."},
			"user":               schema.StringAttribute{Required: true, Description: "The account the Controller logs in as (janus-ctl)."},
			"socket":             schema.StringAttribute{Optional: true, Description: "libvirt's socket on the host; unset: /var/run/libvirt/libvirt-sock."},
			"pool":               schema.StringAttribute{Required: true, Description: "The storage pool (type dir) images and disks go to."},
			"networks":           schema.ListAttribute{Required: true, ElementType: types.StringType, Description: "The libvirt networks its nodes may use."},
			"name_prefix":        schema.StringAttribute{Optional: true, Description: "Its virtual machines' name prefix; unset: janus-."},
			"controller_address": schema.StringAttribute{Optional: true, Description: "Where its nodes reach the Controller's registration port (host:port); unset: the Controller's own guess."},
			"host_key_fingerprint": schema.StringAttribute{
				Optional:    true,
				Description: "The host's SSH key fingerprint, read on the host (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub): the Controller trusts the host only if it presents that key. Unset: not trusted - no node can be created on it.",
			},
			"authorized_key": schema.StringAttribute{
				Computed: true, Description: "The line to add to the account's ~/.ssh/authorized_keys.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"trusted": schema.BoolAttribute{Computed: true, Description: "Whether its host key is pinned."},
		},
	}
}

func (r *hypervisorResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if c, ok := req.ProviderData.(*client.Client); ok {
		r.c = c
	}
}

func (r *hypervisorResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func hypervisorRequest(ctx context.Context, m *hypervisorModel, diags *diag.Diagnostics) client.HypervisorRequest {
	return client.HypervisorRequest{
		Name: m.Name.ValueString(), Kind: "libvirt", ControllerAddress: strOrEmpty(m.ControllerAddress),
		Libvirt: &client.LibvirtConfig{
			Host: m.Host.ValueString(), User: m.User.ValueString(), Socket: strOrEmpty(m.Socket), Pool: m.Pool.ValueString(),
			Networks: stringsOf(ctx, m.Networks, diags), NamePrefix: strOrEmpty(m.NamePrefix),
		},
	}
}

func fromHypervisor(h *client.Hypervisor, m *hypervisorModel) {
	m.ID = types.StringValue(h.ID)
	m.Name = types.StringValue(h.Name)
	m.ControllerAddress = strOrNull(h.ControllerAddress, m.ControllerAddress)
	if l := h.Libvirt; l != nil {
		m.Host, m.User, m.Pool = types.StringValue(l.Host), types.StringValue(l.User), types.StringValue(l.Pool)
		m.Socket = strOrNull(l.Socket, m.Socket)
		m.NamePrefix = strOrNull(l.NamePrefix, m.NamePrefix)
		m.Networks = listOf(l.Networks, m.Networks)
	}
	m.AuthorizedKey = types.StringValue(h.AuthorizedKey)
	m.Trusted = types.BoolValue(h.Trusted)
	// The fingerprint the Controller pinned - another one than the
	// configuration's (the host moved) shows as a change to trust again.
	m.HostKeyFingerprint = strOrNull(h.HostKeyFingerprint, m.HostKeyFingerprint)
}

// trust pins the host key if the configuration gives a fingerprint the
// Controller hasn't pinned yet.
func (r *hypervisorResource) trust(ctx context.Context, h *client.Hypervisor, want types.String) (*client.Hypervisor, error) {
	if want.IsNull() || want.IsUnknown() || want.ValueString() == "" || (h.Trusted && h.HostKeyFingerprint == want.ValueString()) {
		return h, nil
	}
	return r.c.TrustHypervisor(ctx, h.ID, want.ValueString())
}

func (r *hypervisorResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hypervisorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	body := hypervisorRequest(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	h, err := r.c.CreateHypervisor(ctx, body)
	if err != nil {
		resp.Diagnostics.AddError("Adding the hypervisor", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), h.ID)...)
	trusted, err := r.trust(ctx, h, plan.HostKeyFingerprint)
	if err != nil {
		resp.Diagnostics.AddError("Trusting the hypervisor's host key", err.Error())
		trusted = h
	}
	fromHypervisor(trusted, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hypervisorResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hypervisorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	h, err := r.c.Hypervisor(ctx, state.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the hypervisor", err.Error())
		return
	}
	if h.Kind != "" && h.Kind != "libvirt" {
		resp.Diagnostics.AddError("Not a libvirt hypervisor", "hypervisor "+h.ID+" is a "+h.Kind+" one: janus_proxmox_hypervisor manages those")
		return
	}
	fromHypervisor(h, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hypervisorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state hypervisorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	body := hypervisorRequest(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	h, err := r.c.UpdateHypervisor(ctx, state.ID.ValueString(), body)
	if err != nil {
		resp.Diagnostics.AddError("Changing the hypervisor", err.Error())
		return
	}
	trusted, err := r.trust(ctx, h, plan.HostKeyFingerprint)
	if err != nil {
		resp.Diagnostics.AddError("Trusting the hypervisor's host key", err.Error())
		trusted = h
	}
	fromHypervisor(trusted, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hypervisorResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hypervisorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteHypervisor(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Removing the hypervisor", err.Error())
	}
}

// --- data source: an existing hypervisor, by name ---

type hypervisorDataSource struct{ c *client.Client }

func NewHypervisorDataSource() datasource.DataSource { return &hypervisorDataSource{} }

type hypervisorDataModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Kind          types.String `tfsdk:"kind"`
	Host          types.String `tfsdk:"host"`
	Pool          types.String `tfsdk:"pool"`
	Networks      types.List   `tfsdk:"networks"`
	Trusted       types.Bool   `tfsdk:"trusted"`
	AuthorizedKey types.String `tfsdk:"authorized_key"`
}

func (d *hypervisorDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hypervisor"
}

func (d *hypervisorDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = dsschema.Schema{
		Description: "A hypervisor already on the Controller, found by name - libvirt or Proxmox VE.",
		Attributes: map[string]dsschema.Attribute{
			"name":           dsschema.StringAttribute{Required: true},
			"id":             dsschema.StringAttribute{Computed: true},
			"kind":           dsschema.StringAttribute{Computed: true, Description: "libvirt or proxmox."},
			"host":           dsschema.StringAttribute{Computed: true, Description: "Its SSH host (libvirt) or API URL (Proxmox VE)."},
			"pool":           dsschema.StringAttribute{Computed: true, Description: "Its storage pool (libvirt) or resource pool (Proxmox VE)."},
			"networks":       dsschema.ListAttribute{Computed: true, ElementType: types.StringType},
			"trusted":        dsschema.BoolAttribute{Computed: true},
			"authorized_key": dsschema.StringAttribute{Computed: true},
		},
	}
}

func (d *hypervisorDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if c, ok := req.ProviderData.(*client.Client); ok {
		d.c = c
	}
}

func (d *hypervisorDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m hypervisorDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	all, err := d.c.Hypervisors(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Reading the hypervisors", err.Error())
		return
	}
	for _, h := range all {
		if h.Name != m.Name.ValueString() {
			continue
		}
		m.ID, m.Kind, m.Trusted, m.AuthorizedKey = types.StringValue(h.ID), types.StringValue(h.Kind), types.BoolValue(h.Trusted), types.StringValue(h.AuthorizedKey)
		m.Host, m.Pool, m.Networks = types.StringNull(), types.StringNull(), types.ListNull(types.StringType)
		if l := h.Libvirt; l != nil {
			m.Host, m.Pool = types.StringValue(l.Host), types.StringValue(l.Pool)
			m.Networks = listOf(l.Networks, types.ListNull(types.StringType))
		}
		if p := h.Proxmox; p != nil {
			m.Host, m.Pool = types.StringValue(p.URL), types.StringValue(p.Pool)
			m.Networks = listOf(p.Networks, types.ListNull(types.StringType))
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}
	resp.Diagnostics.AddError("No such hypervisor", fmt.Sprintf("the Controller has no hypervisor named %q", m.Name.ValueString()))
}
