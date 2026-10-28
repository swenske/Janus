package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// catalogSource is what a plan's extensions are checked against: the
// Controller's client, or a test's catalog.
type catalogSource interface {
	Catalog(ctx context.Context) (*client.Catalog, error)
}

// checkExtensionRequirements refuses at plan time an extensions set
// naming one without another it requires (prometheus-bird-exporter
// without bird), with the image factory's message - not at apply, once
// the Controller refuses. Nothing is refused while the set isn't known
// yet, or when the Controller can't say (its image factory unreachable):
// the apply is refused then, with the same message. A set the state
// already has isn't asked about again.
func checkExtensionRequirements(ctx context.Context, src catalogSource, plan tfsdk.Plan, state tfsdk.State, at path.Path, diags *diag.Diagnostics) {
	if src == nil || plan.Raw.IsNull() {
		return
	}
	var set types.Set
	if d := plan.GetAttribute(ctx, at, &set); d.HasError() || set.IsNull() || set.IsUnknown() {
		return
	}
	if !state.Raw.IsNull() {
		var prior types.Set
		if d := state.GetAttribute(ctx, at, &prior); !d.HasError() && prior.Equal(set) {
			return
		}
	}
	var exts []string
	if d := set.ElementsAs(ctx, &exts, false); d.HasError() || len(exts) == 0 {
		return
	}
	cat, err := src.Catalog(ctx)
	if err != nil {
		return
	}
	if err := cat.CheckRequirements(exts); err != nil {
		diags.AddAttributeError(at, "Extension requirement", err.Error())
	}
}

var (
	_ resource.ResourceWithModifyPlan = &nodeResource{}
	_ resource.ResourceWithModifyPlan = &clusterResource{}
)

func (r *nodeResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if r.c == nil {
		return
	}
	checkExtensionRequirements(ctx, r.c, req.Plan, req.State, path.Root("extensions"), &resp.Diagnostics)
}

func (r *clusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if r.c == nil {
		return
	}
	checkExtensionRequirements(ctx, r.c, req.Plan, req.State, path.Root("machine").AtName("extensions"), &resp.Diagnostics)
}
