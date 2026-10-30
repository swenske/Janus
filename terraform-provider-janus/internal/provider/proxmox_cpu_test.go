package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// cpu_model: optional and computed on janus_proxmox_hypervisor - unset,
// the state gets the Controller's default -, computed on the data
// source.
func TestCPUModelSchemas(t *testing.T) {
	srv := providerserver.NewProtocol6(New("test")())()
	resp, err := srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	attr := func(s *tfprotov6.Schema, name string) *tfprotov6.SchemaAttribute {
		for _, a := range s.Block.Attributes {
			if a.Name == name {
				return a
			}
		}
		return nil
	}
	if a := attr(resp.ResourceSchemas["janus_proxmox_hypervisor"], "cpu_model"); a == nil || !a.Optional || !a.Computed || a.Required {
		t.Errorf("janus_proxmox_hypervisor's cpu_model: %+v", a)
	}
	if a := attr(resp.DataSourceSchemas["janus_hypervisor"], "cpu_model"); a == nil || !a.Computed || a.Optional {
		t.Errorf("the data source's cpu_model: %+v", a)
	}
	if a := attr(resp.ResourceSchemas["janus_hypervisor"], "cpu_model"); a != nil {
		t.Errorf("janus_hypervisor (libvirt) has a cpu_model: %+v", a)
	}
}

// The model goes to the Controller only when set - left out, the
// Controller gives its default to a new hypervisor and keeps the model
// of one it has -, and comes back into the state as the Controller says.
func TestProxmoxCPUModel(t *testing.T) {
	ctx := context.Background()
	nets, _ := types.ListValueFrom(ctx, types.StringType, []string{"vmbr0.10"})
	m := proxmoxHypervisorModel{
		Name: types.StringValue("pve01"), URL: types.StringValue("https://pve01:8006"), Node: types.StringValue("pve01"),
		TokenID: types.StringValue("janus-ctl@pve!controller"), TokenSecret: types.StringValue("s3cr3t"),
		Pool: types.StringValue("janus"), Storage: types.StringValue("local-lvm"), ImageStorage: types.StringValue("janus-images"),
		Networks: nets, CPUModel: types.StringUnknown(), Labels: types.MapNull(types.StringType),
	}
	var diags diag.Diagnostics
	raw, _ := json.Marshal(proxmoxRequest(ctx, &m, &diags))
	if strings.Contains(string(raw), "cpu_model") {
		t.Errorf("an unset model sent: %s", raw)
	}
	h := &client.Hypervisor{ID: "h1", Name: "pve01", Kind: "proxmox", Proxmox: &client.ProxmoxConfig{URL: "https://pve01:8006", CPUModel: "x86-64-v2-AES"}}
	fromProxmox(h, &m)
	if m.CPUModel.ValueString() != "x86-64-v2-AES" {
		t.Errorf("state after an unset model: %v", m.CPUModel)
	}

	m.CPUModel = types.StringValue("x86-64-v3")
	req := proxmoxRequest(ctx, &m, &diags)
	if diags.HasError() || req.Proxmox.CPUModel != "x86-64-v3" {
		t.Fatalf("request: %+v %v", req.Proxmox, diags)
	}
	h.Proxmox.CPUModel = "x86-64-v3"
	fromProxmox(h, &m)
	if m.CPUModel.ValueString() != "x86-64-v3" {
		t.Errorf("state: %v", m.CPUModel)
	}
	// Changed outside Terraform: the state shows it, a plan puts it back.
	h.Proxmox.CPUModel = "host"
	fromProxmox(h, &m)
	if m.CPUModel.ValueString() != "host" {
		t.Errorf("state after a change elsewhere: %v", m.CPUModel)
	}
	// A Controller without the setting: known, null.
	h.Proxmox.CPUModel = ""
	fromProxmox(h, &m)
	if !m.CPUModel.IsNull() {
		t.Errorf("an older Controller's: %v", m.CPUModel)
	}
}

// The data source reads the model of a Proxmox hypervisor, null for a
// libvirt one.
func TestHypervisorDataSourceCPUModel(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hypervisors" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[
			{"id":"h1","name":"kvm01","kind":"libvirt","libvirt":{"host":"kvm01","user":"janus-ctl","pool":"janus","networks":["lan"]},"labels":{}},
			{"id":"h2","name":"pve01","kind":"proxmox","proxmox":{"url":"https://pve01:8006","node":"pve01","pool":"janus","networks":["vmbr0.10"],"cpu_model":"x86-64-v3"},"labels":{}}
		]`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "tok", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ds := &hypervisorDataSource{c: c}
	var sr datasource.SchemaResponse
	ds.Schema(ctx, datasource.SchemaRequest{}, &sr)
	typ := sr.Schema.Type().TerraformType(ctx).(tftypes.Object)
	read := func(name string) types.String {
		t.Helper()
		vals := map[string]tftypes.Value{}
		for k, at := range typ.AttributeTypes {
			vals[k] = tftypes.NewValue(at, nil)
		}
		vals["name"] = tftypes.NewValue(tftypes.String, name)
		cfg := tfsdk.Config{Schema: sr.Schema, Raw: tftypes.NewValue(typ, vals)}
		resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(typ, nil)}}
		ds.Read(ctx, datasource.ReadRequest{Config: cfg}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", name, resp.Diagnostics)
		}
		var m hypervisorDataModel
		resp.Diagnostics.Append(resp.State.Get(ctx, &m)...)
		return m.CPUModel
	}
	if got := read("pve01"); got.ValueString() != "x86-64-v3" {
		t.Errorf("pve01: %v", got)
	}
	if got := read("kvm01"); !got.IsNull() {
		t.Errorf("kvm01: %v", got)
	}
}
