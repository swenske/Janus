package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

// The schemas, as Terraform gets them: the framework checks them then.
func TestSchemas(t *testing.T) {
	srv := providerserver.NewProtocol6(New("test")())()
	resp, err := srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		t.Errorf("%s: %s: %s", d.Severity, d.Summary, d.Detail)
	}
	for _, name := range []string{"janus_node", "janus_hypervisor"} {
		if _, ok := resp.ResourceSchemas[name]; !ok {
			t.Errorf("no %s resource", name)
		}
	}
	if _, ok := resp.DataSourceSchemas["janus_hypervisor"]; !ok {
		t.Error("no janus_hypervisor data source")
	}
}

func strList(v ...string) types.List {
	if v == nil {
		return types.ListNull(types.StringType)
	}
	l, _ := types.ListValueFrom(context.Background(), types.StringType, v)
	return l
}

func iface(network, name, mac, mode string, addrs ...string) interfaceModel {
	return interfaceModel{
		Network: types.StringValue(network), Name: types.StringValue(name), MAC: types.StringValue(mac),
		Mode: types.StringValue(mode), Addresses: strList(addrs...), Gateway: types.StringNull(),
	}
}

func TestInterfacesStructureChanged(t *testing.T) {
	state := []interfaceModel{iface("lan", "mgmt", "52:54:00:00:00:01", "static", "10.0.0.5/24")}
	same := []interfaceModel{iface("lan", "mgmt", "52:54:00:00:00:01", "static", "10.0.0.6/24")}
	unsetMAC := []interfaceModel{iface("lan", "mgmt", "", "dhcp")}
	unsetMAC[0].MAC = types.StringUnknown()
	for name, tc := range map[string]struct {
		plan []interfaceModel
		want bool
	}{
		"address only":    {same, false},
		"mode, MAC unset": {unsetMAC, false},
		"network":         {[]interfaceModel{iface("dmz", "mgmt", "52:54:00:00:00:01", "static")}, true},
		"name":            {[]interfaceModel{iface("lan", "eth0", "52:54:00:00:00:01", "static")}, true},
		"MAC":             {[]interfaceModel{iface("lan", "mgmt", "52:54:00:00:00:02", "static")}, true},
		"one more":        {append(same, iface("dmz", "front", "", "dhcp")), true},
	} {
		if got := interfacesStructureChanged(state, tc.plan); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestUpdateFor(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics
	state := &nodeModel{
		VCPUs: types.Int64Value(2), MemoryMiB: types.Int64Value(1024), Version: types.StringValue("v2026.10.02-4"),
		Extensions: types.SetNull(types.StringType),
		Interfaces: []interfaceModel{iface("lan", "mgmt", "52:54:00:00:00:01", "static", "10.0.0.5/24")},
		DNS:        strList(), NTP: strList(),
	}
	same := *state
	if u := updateFor(ctx, state, &same, &diags); u != nil {
		t.Errorf("no change gave %+v", u)
	}

	plan := *state
	plan.MemoryMiB = types.Int64Value(2048)
	plan.Version = types.StringValue("v2026.10.05")
	plan.Interfaces = []interfaceModel{iface("lan", "mgmt", "", "static", "10.0.0.6/24")}
	plan.Interfaces[0].MAC = types.StringUnknown()
	plan.NTP = strList("10.0.0.1")
	u := updateFor(ctx, state, &plan, &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if u == nil || u.VCPUs != nil || *u.MemoryMiB != 2048 || *u.Version != "v2026.10.05" || u.Extensions != nil || u.DNS != nil {
		t.Fatalf("update %+v", u)
	}
	if nics := *u.NICs; nics[0].MAC != "52:54:00:00:00:01" || nics[0].Addresses[0] != "10.0.0.6/24" {
		t.Errorf("NICs %+v (the known MAC must be kept)", nics)
	}
	if (*u.NTP)[0] != "10.0.0.1" {
		t.Errorf("NTP %v", *u.NTP)
	}

	// An unset version after creation means "keep whatever it runs".
	plan = *state
	plan.Version = types.StringUnknown()
	if u := updateFor(ctx, state, &plan, &diags); u != nil {
		t.Errorf("unknown version gave %+v", u)
	}
}

// What the Controller returns becomes the state without a spurious
// difference: unset lists stay unset, the version is the one it runs.
func TestFromMachine(t *testing.T) {
	m := &client.Machine{
		ID: "m1", Phase: "ready", Version: "v2026.10.02-4", Schematic: "a055fbb6", NodeID: "n1", NodeAddress: "10.0.0.5:9505", VMName: "janus-lb1",
		Spec: client.MachineSpec{Name: "lb1", HypervisorID: "h1", VCPUs: 2, MemoryMiB: 1024,
			NICs: []client.NIC{{Network: "lan", Name: "mgmt", MAC: "52:54:00:00:00:01", Mode: "dhcp"}}},
	}
	s := nodeModel{
		Extensions: types.SetNull(types.StringType), DNS: strList(), NTP: strList(),
		Interfaces: []interfaceModel{{Addresses: strList(), Gateway: types.StringNull()}},
	}
	fromMachine(m, &s)
	if s.Version.ValueString() != "v2026.10.02-4" || !s.Extensions.IsNull() || !s.DNS.IsNull() || !s.Interfaces[0].Addresses.IsNull() || !s.Interfaces[0].Gateway.IsNull() {
		t.Errorf("state %+v", s)
	}
	if s.Interfaces[0].MAC.ValueString() != "52:54:00:00:00:01" || s.NodeAddress.ValueString() != "10.0.0.5:9505" {
		t.Errorf("computed %+v", s)
	}
	// An empty list written as such stays an empty list.
	s.DNS = strList([]string{}...)
	empty, _ := types.ListValueFrom(context.Background(), types.StringType, []string{})
	s.DNS = empty
	fromMachine(m, &s)
	if s.DNS.IsNull() {
		t.Error("an empty dns list became null")
	}
}
