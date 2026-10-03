package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// janus_proxmox_hypervisor: a Proxmox VE node the Controller creates
// nodes on, through its API with a token prepared as docs/hypervisors.md
// says. The API is trusted once certificate_fingerprint is given - the
// fingerprint an operator read on the node itself - or ca_cert, the CA
// that signs its certificate. The token's secret is written to the
// Controller, never read back: it's kept in the state, sensitive.

type proxmoxHypervisorResource struct{ c *client.Client }

func NewProxmoxHypervisorResource() resource.Resource { return &proxmoxHypervisorResource{} }

type proxmoxHypervisorModel struct {
	ID                     types.String `tfsdk:"id"`
	Name                   types.String `tfsdk:"name"`
	URL                    types.String `tfsdk:"url"`
	Node                   types.String `tfsdk:"node"`
	TokenID                types.String `tfsdk:"token_id"`
	TokenSecret            types.String `tfsdk:"token_secret"`
	Pool                   types.String `tfsdk:"pool"`
	Storage                types.String `tfsdk:"storage"`
	ImageStorage           types.String `tfsdk:"image_storage"`
	Networks               types.List   `tfsdk:"networks"`
	NamePrefix             types.String `tfsdk:"name_prefix"`
	VMIDs                  types.String `tfsdk:"vmids"`
	ControllerAddress      types.String `tfsdk:"controller_address"`
	CACert                 types.String `tfsdk:"ca_cert"`
	CertificateFingerprint types.String `tfsdk:"certificate_fingerprint"`
	Trusted                types.Bool   `tfsdk:"trusted"`
}

func (r *proxmoxHypervisorResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_proxmox_hypervisor"
}

func (r *proxmoxHypervisorResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A Proxmox VE node the Controller creates its nodes on, through its API with a token scoped to a pool (docs/hypervisors.md: preparing a Proxmox VE node).",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":          schema.StringAttribute{Required: true, Description: "Its name on the Controller."},
			"url":           schema.StringAttribute{Required: true, Description: "The API: https://host[:8006]."},
			"node":          schema.StringAttribute{Required: true, Description: "The node machines are created on."},
			"token_id":      schema.StringAttribute{Required: true, Description: "The API token: user@realm!name."},
			"token_secret":  schema.StringAttribute{Required: true, Sensitive: true, Description: "The API token's secret - written to the Controller, never read back."},
			"pool":          schema.StringAttribute{Required: true, Description: "The resource pool its machines go in."},
			"storage":       schema.StringAttribute{Required: true, Description: "The storage for the machines' disks."},
			"image_storage": schema.StringAttribute{Required: true, Description: "The directory storage (import and iso content) for the images and NoCloud volumes."},
			"networks": schema.ListAttribute{
				Required: true, ElementType: types.StringType,
				Description: "The networks its nodes may use: a bridge (vmbr0) or a VLAN on one (vmbr0.20).",
			},
			"name_prefix":        schema.StringAttribute{Optional: true, Description: "Its virtual machines' name prefix; unset: janus-."},
			"vmids":              schema.StringAttribute{Optional: true, Description: "The VM IDs its machines take, first-last; unset: the cluster's next free one."},
			"controller_address": schema.StringAttribute{Optional: true, Description: "Where its nodes reach the Controller's registration port (host:port); unset: the Controller's own guess."},
			"ca_cert": schema.StringAttribute{
				Optional:    true,
				Description: "The CA certificate (PEM) that signs the API's: the API is trusted by it - a renewed certificate stays trusted.",
			},
			"certificate_fingerprint": schema.StringAttribute{
				Optional: true,
				Description: "The API certificate's SHA-256 fingerprint, read on the node (openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pveproxy-ssl.pem, or pve-ssl.pem): " +
					"the Controller trusts the API only if it presents that certificate. Unset, with no ca_cert: not trusted - no node can be created on it.",
			},
			"trusted": schema.BoolAttribute{Computed: true, Description: "Whether the API is trusted (a pinned certificate, or a CA)."},
		},
	}
}

func (r *proxmoxHypervisorResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if c, ok := req.ProviderData.(*client.Client); ok {
		r.c = c
	}
}

// ImportState: by ID. The token's secret isn't readable: the next plan
// writes the configuration's.
func (r *proxmoxHypervisorResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func proxmoxRequest(ctx context.Context, m *proxmoxHypervisorModel, diags *diag.Diagnostics) client.HypervisorRequest {
	return client.HypervisorRequest{
		Name: m.Name.ValueString(), Kind: "proxmox", ControllerAddress: strOrEmpty(m.ControllerAddress), TokenSecret: strOrEmpty(m.TokenSecret),
		Proxmox: &client.ProxmoxConfig{
			URL: m.URL.ValueString(), Node: m.Node.ValueString(), TokenID: m.TokenID.ValueString(),
			Pool: m.Pool.ValueString(), Storage: m.Storage.ValueString(), ImageStorage: m.ImageStorage.ValueString(),
			Networks: stringsOf(ctx, m.Networks, diags), NamePrefix: strOrEmpty(m.NamePrefix), VMIDs: strOrEmpty(m.VMIDs),
			CACert: strOrEmpty(m.CACert),
		},
	}
}

// fromProxmox fills the model from what the Controller says. The
// secret stays the state's; a fingerprint the configuration wrote in
// another case stays as it wrote it.
func fromProxmox(h *client.Hypervisor, m *proxmoxHypervisorModel) {
	m.ID = types.StringValue(h.ID)
	m.Name = types.StringValue(h.Name)
	m.ControllerAddress = strOrNull(h.ControllerAddress, m.ControllerAddress)
	if p := h.Proxmox; p != nil {
		m.URL, m.Node, m.TokenID = types.StringValue(p.URL), types.StringValue(p.Node), types.StringValue(p.TokenID)
		m.Pool, m.Storage, m.ImageStorage = types.StringValue(p.Pool), types.StringValue(p.Storage), types.StringValue(p.ImageStorage)
		m.Networks = listOf(p.Networks, m.Networks)
		m.NamePrefix = strOrNull(p.NamePrefix, m.NamePrefix)
		m.VMIDs = strOrNull(p.VMIDs, m.VMIDs)
		if strings.TrimSpace(m.CACert.ValueString()) != strings.TrimSpace(p.CACert) {
			m.CACert = strOrNull(p.CACert, m.CACert)
		}
	}
	m.Trusted = types.BoolValue(h.Trusted)
	if !strings.EqualFold(m.CertificateFingerprint.ValueString(), h.HostKeyFingerprint) {
		// Pinned another one (the API moved), or none: a change to trust.
		m.CertificateFingerprint = strOrNull(h.HostKeyFingerprint, m.CertificateFingerprint)
	}
}

// trust pins the API's certificate if the configuration gives a
// fingerprint the Controller hasn't pinned yet.
func (r *proxmoxHypervisorResource) trust(ctx context.Context, h *client.Hypervisor, want types.String) (*client.Hypervisor, error) {
	fp := strOrEmpty(want)
	if fp == "" || strings.EqualFold(h.HostKeyFingerprint, fp) {
		return h, nil
	}
	return r.c.TrustHypervisor(ctx, h.ID, fp)
}

func (r *proxmoxHypervisorResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan proxmoxHypervisorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	body := proxmoxRequest(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	h, err := r.c.CreateHypervisor(ctx, body)
	if err != nil {
		resp.Diagnostics.AddError("Adding the Proxmox VE hypervisor", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), h.ID)...)
	trusted, err := r.trust(ctx, h, plan.CertificateFingerprint)
	if err != nil {
		resp.Diagnostics.AddError("Trusting the API's certificate", err.Error())
		trusted = h
	}
	fromProxmox(trusted, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *proxmoxHypervisorResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state proxmoxHypervisorModel
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
		resp.Diagnostics.AddError("Reading the Proxmox VE hypervisor", err.Error())
		return
	}
	if h.Kind != "proxmox" {
		resp.Diagnostics.AddError("Not a Proxmox VE hypervisor", "hypervisor "+h.ID+" is a "+h.Kind+" one: janus_hypervisor manages those")
		return
	}
	fromProxmox(h, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *proxmoxHypervisorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state proxmoxHypervisorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	body := proxmoxRequest(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	h, err := r.c.UpdateHypervisor(ctx, state.ID.ValueString(), body)
	if err != nil {
		resp.Diagnostics.AddError("Changing the Proxmox VE hypervisor", err.Error())
		return
	}
	trusted, err := r.trust(ctx, h, plan.CertificateFingerprint)
	if err != nil {
		resp.Diagnostics.AddError("Trusting the API's certificate", err.Error())
		trusted = h
	}
	fromProxmox(trusted, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *proxmoxHypervisorResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state proxmoxHypervisorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteHypervisor(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Removing the Proxmox VE hypervisor", err.Error())
	}
}
