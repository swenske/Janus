package main

import (
	"strings"
	"testing"

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
