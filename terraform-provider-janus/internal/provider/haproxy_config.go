package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// janus_haproxy_config: a node's haproxy.cfg - applied through the
// Controller's relay to the node, which has HAProxy check it before a new
// process takes the sockets over without dropping connections. A token
// narrowed to the haproxy domain on the node's labels is enough: the
// company case where users terraform HAProxy and nothing else.

type haproxyConfigResource struct{ c *client.Client }

func NewHAProxyConfigResource() resource.Resource { return &haproxyConfigResource{} }

type haproxyConfigModel struct {
	ID     types.String `tfsdk:"id"`
	Node   types.String `tfsdk:"node"`
	Config types.String `tfsdk:"config"`
	SHA256 types.String `tfsdk:"sha256"`
}

func (r *haproxyConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_haproxy_config"
}

func (r *haproxyConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A node's haproxy.cfg. Applying it has HAProxy check it first, then take over without dropping connections; a configuration HAProxy refuses fails the apply and changes nothing. Destroying it leaves the node's configuration as it is.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "The node's ID.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"node": schema.StringAttribute{
				Required: true, Description: "The node, by ID or name - one the token reaches (janus_node's node_id, for one Terraform created).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"config": schema.StringAttribute{
				Required: true, Description: "haproxy.cfg, whole.",
			},
			"sha256": schema.StringAttribute{
				Computed: true, Description: "The applied configuration's SHA-256, as the node reports it.",
			},
		},
	}
}

func (r *haproxyConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if c, ok := req.ProviderData.(*client.Client); ok {
		r.c = c
	}
}

func (r *haproxyConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	n, err := r.c.FindNode(ctx, req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Importing the HAProxy configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), n.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("node"), req.ID)...)
}

// apply puts plan's configuration on its node, and reads it back.
func (r *haproxyConfigResource) apply(ctx context.Context, plan *haproxyConfigModel) error {
	id := plan.ID.ValueString()
	if id == "" {
		n, err := r.c.FindNode(ctx, plan.Node.ValueString())
		if err != nil {
			return err
		}
		id = n.ID
	}
	if err := r.c.ApplyHAProxyConfig(ctx, id, plan.Config.ValueString()); err != nil {
		return err
	}
	got, err := r.c.NodeHAProxyConfig(ctx, id)
	if err != nil {
		return fmt.Errorf("applied, but reading it back: %w", err)
	}
	plan.ID, plan.SHA256 = types.StringValue(id), types.StringValue(got.SHA256)
	return nil
}

func (r *haproxyConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan haproxyConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue("")
	if err := r.apply(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Applying the HAProxy configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *haproxyConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state haproxyConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.c.NodeHAProxyConfig(ctx, state.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading the HAProxy configuration", err.Error())
		return
	}
	// The configuration as the node has it: a change made elsewhere shows
	// in the next plan.
	state.Config, state.SHA256 = types.StringValue(got.Config), types.StringValue(got.SHA256)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *haproxyConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state haproxyConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	if err := r.apply(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Applying the HAProxy configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete leaves the node's configuration: HAProxy can't run without one,
// and the node keeps serving.
func (r *haproxyConfigResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {}
