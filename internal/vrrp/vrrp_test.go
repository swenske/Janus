package vrrp

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Captured from keepalived 2.3.4 (the extension's build) with one MASTER
// instance: `kill -$(keepalived --signum=JSON) <pid>`, /tmp/keepalived.json.
const dumpJSON = `[{"data":{"iname":"VI_1","dont_track_primary":0,"skip_check_adv_addr":0,"strict_mode":0,"vmac_ifname":"","ifp_ifname":"v0","master_priority":0,"last_transition":1790814169.381213,"garp_delay":5,"garp_refresh":0,"garp_rep":5,"garp_refresh_rep":1,"garp_lower_prio_delay":5,"garp_lower_prio_rep":5,"lower_prio_no_advert":0,"higher_prio_send_advert":0,"vrid":51,"base_priority":100,"effective_priority":100,"vipset":true,"promote_secondaries":false,"adver_int":1,"master_adver_int":1,"nopreempt":false,"preempt_delay":0,"state":2,"wantstate":2,"version":2,"smtp_alert":false,"notify_deleted":false,"vips":["10.9.0.100\/24 dev v0 scope global set"]},"stats":{"advert_rcvd":0,"advert_sent":2,"become_master":1,"release_master":0,"packet_len_err":0,"advert_interval_err":0,"ip_ttl_err":0,"invalid_type_rcvd":0,"addr_list_err":0,"invalid_authtype":0,"pri_zero_rcvd":0,"pri_zero_sent":0}}]`

func TestParseJSON(t *testing.T) {
	in, err := ParseJSON([]byte(dumpJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 {
		t.Fatalf("%d instances", len(in))
	}
	i := in[0]
	if i.Name != "VI_1" || i.State != "MASTER" || i.Interface != "v0" || i.VRID != 51 || i.Priority != 100 || i.EffectivePriority != 100 || i.BecameMaster != 1 {
		t.Fatalf("%+v", i)
	}
	if len(i.VirtualIPs) != 1 || i.VirtualIPs[0] != "10.9.0.100/24" {
		t.Fatalf("VIPs %q", i.VirtualIPs)
	}
	if i.LastTransition.Unix() != 1790814169 {
		t.Fatalf("last transition %v", i.LastTransition)
	}
	if _, err := ParseJSON([]byte("not json")); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestKeepHealth(t *testing.T) {
	RunDir = t.TempDir()
	var healthy atomic.Bool
	healthy.Store(true)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { KeepHealth(healthy.Load, 10*time.Millisecond, stop); close(done) }()
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			data, _ := os.ReadFile(HealthFile())
			if strings.TrimSpace(string(data)) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("health file %q, want %q", data, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("0")
	healthy.Store(false)
	wait("1")
	healthy.Store(true)
	wait("0")
	close(stop)
	<-done
	if left, _ := filepath.Glob(filepath.Join(RunDir, "*.tmp")); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}
