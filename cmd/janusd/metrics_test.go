package main

import (
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/bgp"
	"github.com/swenske/Janus/internal/exporter"
	"github.com/swenske/Janus/internal/vrrp"
)

// keepalived's counters are exported per instance, each only when the
// dump carried it: a counter keepalived doesn't keep is absent, not 0.
func TestVRRPFamilies(t *testing.T) {
	n := func(v uint64) *uint64 { return &v }
	instances := []vrrp.Instance{
		{Name: "VI_1", State: "MASTER", Interface: "eth1", EffectivePriority: 150, BecameMaster: 2, Counters: vrrp.Counters{
			LeftMaster: n(1), AdvertsSent: n(1200), AdvertsReceived: n(3), PriorityZeroSent: n(1), PriorityZeroReceived: n(0),
			AdvertErrors: map[string]uint64{"packet_length": 0, "ttl": 4, "packet_type": 0, "address_list": 1, "authentication_type": 0},
		}},
		// From a dump without "stats": state and priority only.
		{Name: "VI_2", State: "BACKUP", Interface: "eth2", EffectivePriority: 100},
	}
	var b strings.Builder
	if err := exporter.Write(&b, vrrpFamilies(instances)); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`janus_vrrp_instance_state{instance="VI_1",interface="eth1",state="MASTER"} 1`,
		`janus_vrrp_instance_state{instance="VI_2",interface="eth2",state="BACKUP"} 1`,
		`janus_vrrp_instance_became_master_total{instance="VI_1"} 2`,
		`janus_vrrp_instance_left_master_total{instance="VI_1"} 1`,
		`janus_vrrp_instance_adverts_sent_total{instance="VI_1"} 1200`,
		`janus_vrrp_instance_adverts_received_total{instance="VI_1"} 3`,
		`janus_vrrp_instance_priority_zero_sent_total{instance="VI_1"} 1`,
		`janus_vrrp_instance_priority_zero_received_total{instance="VI_1"} 0`,
		`janus_vrrp_instance_advert_errors_total{instance="VI_1",error="ttl"} 4`,
		`janus_vrrp_instance_advert_errors_total{instance="VI_1",error="address_list"} 1`,
		"# TYPE janus_vrrp_instance_adverts_sent_total counter",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	// VI_2's dump had no counters: none is made up for it.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `instance="VI_2"`) && !strings.HasPrefix(line, "janus_vrrp_instance_state{") &&
			!strings.HasPrefix(line, "janus_vrrp_instance_effective_priority{") && !strings.HasPrefix(line, "janus_vrrp_instance_became_master_total{") {
			t.Errorf("a counter VI_2's dump didn't carry: %s", line)
		}
	}
	// No instance with counters: the families aren't written at all.
	b.Reset()
	if err := exporter.Write(&b, vrrpFamilies(instances[1:])); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "adverts_sent") || strings.Contains(b.String(), "advert_errors") {
		t.Errorf("families without samples written:\n%s", b.String())
	}
}

// BIRD's protocols: their state and since when, routes per channel and
// the route change stats - only what BIRD printed (bgp.ParseProtocols
// on BIRD 2.20.0's replies is internal/bgp's TestParseBIRD220).
func TestBGPFamilies(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	filtered := uint32(1)
	protocols := []bgp.Protocol{
		{Name: "peer_a", Proto: "BGP", State: "up", Since: "2026-10-28 23:53:40", BGPState: "Established", NeighborAddress: "192.168.78.1",
			Channels: []bgp.Channel{{Name: "ipv4", State: "UP", Imported: 2, Preferred: 2, Counted: true, Filtered: &filtered, Changes: []bgp.RouteChange{
				{Direction: "import", Kind: "updates", Outcome: "received", Count: 3},
				{Direction: "import", Kind: "updates", Outcome: "filtered", Count: 1},
				{Direction: "import", Kind: "withdraws", Outcome: "accepted", Count: 1},
			}}}},
		// Held down: its channel is down, BIRD printed no counts.
		{Name: "haproxy_anycast", Proto: "Static", State: "down", Since: "2026-10-28 23:53:43", HeldDown: true,
			Channels: []bgp.Channel{{Name: "ipv4", State: "DOWN"}}},
		// BIRD's default format past 20 hours: a date, no moment.
		{Name: "device1", Proto: "Device", State: "up", Since: "2026-10-20"},
	}
	var b strings.Builder
	if err := exporter.Write(&b, bgpFamilies(protocols, time.Date(2026, 10, 29, 0, 0, 0, 0, cet))); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`janus_bgp_protocol_state{protocol="peer_a",proto="BGP",state="up"} 1`,
		`janus_bgp_protocol_state{protocol="peer_a",proto="BGP",state="start"} 0`,
		`janus_bgp_protocol_state{protocol="haproxy_anycast",proto="Static",state="down"} 1`,
		`janus_bgp_protocol_state_since_timestamp_seconds{protocol="peer_a",proto="BGP"} 1.79322802e+09`,
		`janus_bgp_protocol_state_since_timestamp_seconds{protocol="haproxy_anycast",proto="Static"} 1.793228023e+09`,
		`janus_bgp_routes{protocol="peer_a",channel="ipv4",direction="imported"} 2`,
		`janus_bgp_routes{protocol="peer_a",channel="ipv4",direction="preferred"} 2`,
		`janus_bgp_routes{protocol="peer_a",channel="ipv4",direction="filtered"} 1`,
		`janus_bgp_route_changes_total{protocol="peer_a",channel="ipv4",direction="import",kind="updates",outcome="received"} 3`,
		`janus_bgp_route_changes_total{protocol="peer_a",channel="ipv4",direction="import",kind="withdraws",outcome="accepted"} 1`,
		"# TYPE janus_bgp_route_changes_total counter",
		`janus_bgp_protocol_held_down{protocol="haproxy_anycast"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		`janus_bgp_routes{protocol="haproxy_anycast",channel="ipv4",direction="preferred"}`,
		`janus_bgp_routes{protocol="haproxy_anycast",channel="ipv4",direction="filtered"}`,
		`janus_bgp_route_changes_total{protocol="haproxy_anycast"`,
		`janus_bgp_protocol_state_since_timestamp_seconds{protocol="device1"`,
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("made up: %s in:\n%s", unwanted, out)
		}
	}
}
