package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

type fakeCatalog struct {
	cat *client.Catalog
	err error
}

func (f fakeCatalog) Catalog(context.Context) (*client.Catalog, error) { return f.cat, f.err }

// planOf is r's plan with every attribute null but those given.
func planOf(t *testing.T, r resource.Resource, set map[string]tftypes.Value) tfsdk.Plan {
	t.Helper()
	ctx := context.Background()
	var sr resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	typ := sr.Schema.Type().TerraformType(ctx).(tftypes.Object)
	vals := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	for k, v := range set {
		vals[k] = v
	}
	return tfsdk.Plan{Schema: sr.Schema, Raw: tftypes.NewValue(typ, vals)}
}

func extSet(names ...string) tftypes.Value {
	if names == nil {
		return tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, nil)
	}
	var vs []tftypes.Value
	for _, n := range names {
		vs = append(vs, tftypes.NewValue(tftypes.String, n))
	}
	return tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, vs)
}

var birdCatalog = &client.Catalog{CatalogVersion: "v2026.10.29", Extensions: []client.CatalogEntry{
	{Name: "bird"}, {Name: "prometheus-bird-exporter", Requires: []string{"bird"}},
}}

const wantRequirement = `extension "prometheus-bird-exporter" requires the "bird" extension: choose it too`

// janus_node's extensions: an extension without the one it requires is a
// plan-time error on the attribute, with the image factory's message.
func TestNodeExtensionRequirements(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		exts tftypes.Value
		src  catalogSource
		want bool
	}{
		{"alone", extSet("prometheus-bird-exporter"), fakeCatalog{cat: birdCatalog}, true},
		{"with bird", extSet("prometheus-bird-exporter", "bird"), fakeCatalog{cat: birdCatalog}, false},
		{"none", extSet(), fakeCatalog{cat: birdCatalog}, false},
		{"unknown yet", tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, tftypes.UnknownValue), fakeCatalog{cat: birdCatalog}, false},
		{"no catalog", extSet("prometheus-bird-exporter"), fakeCatalog{err: errors.New("no image factory")}, false},
	} {
		var diags diag.Diagnostics
		plan := planOf(t, &nodeResource{}, map[string]tftypes.Value{"extensions": tc.exts})
		checkExtensionRequirements(ctx, tc.src, plan, tfsdk.State{Raw: tftypes.NewValue(plan.Raw.Type(), nil), Schema: plan.Schema}, path.Root("extensions"), &diags)
		if diags.HasError() != tc.want {
			t.Errorf("%s: %v", tc.name, diags)
		}
		if tc.want && diags.Errors()[0].Detail() != wantRequirement {
			t.Errorf("%s: %q", tc.name, diags.Errors()[0].Detail())
		}
	}
}

// Wired: janus_node and janus_cluster ask the Controller's catalog.
func TestModifyPlanAsksTheController(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/catalog" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"catalog_version":"v2026.10.29","extensions":[{"name":"bird"},{"name":"prometheus-bird-exporter","requires":["bird"]}]}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "tok", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	node := &nodeResource{c: c}
	resp := &resource.ModifyPlanResponse{}
	alone := planOf(t, node, map[string]tftypes.Value{"extensions": extSet("prometheus-bird-exporter")})
	node.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: alone}, resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Detail() != wantRequirement {
		t.Errorf("janus_node: %v", resp.Diagnostics)
	}
	// A set the state already has (made before the requirement existed)
	// isn't refused on every plan.
	resp = &resource.ModifyPlanResponse{}
	node.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: alone, State: tfsdk.State(alone)}, resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("janus_node unchanged: %v", resp.Diagnostics)
	}

	cl := &clusterResource{c: c}
	var sr resource.SchemaResponse
	cl.Schema(ctx, resource.SchemaRequest{}, &sr)
	mt := sr.Schema.Type().TerraformType(ctx).(tftypes.Object).AttributeTypes["machine"].(tftypes.Object)
	mv := map[string]tftypes.Value{}
	for name, at := range mt.AttributeTypes {
		mv[name] = tftypes.NewValue(at, nil)
	}
	mv["extensions"] = extSet("prometheus-bird-exporter")
	resp = &resource.ModifyPlanResponse{}
	cl.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: planOf(t, cl, map[string]tftypes.Value{"machine": tftypes.NewValue(mt, mv)})}, resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Detail() != wantRequirement {
		t.Errorf("janus_cluster: %v", resp.Diagnostics)
	}
	mv["extensions"] = extSet("prometheus-bird-exporter", "bird")
	resp = &resource.ModifyPlanResponse{}
	cl.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: planOf(t, cl, map[string]tftypes.Value{"machine": tftypes.NewValue(mt, mv)})}, resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("janus_cluster with bird: %v", resp.Diagnostics)
	}
}
