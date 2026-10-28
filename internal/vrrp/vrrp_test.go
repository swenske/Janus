package vrrp

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// keepalived 2.4.3's own dumps (the extension's build), from two
// enforcing nodes of hack/qemu-vrrp-test.sh (VRRP_DUMP_DIR) after a
// failover and a failback: A master, B backup.
func TestParseJSONKeepalived243(t *testing.T) {
	read := func(name string) Instance {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		in, err := ParseJSON(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(in) != 1 || in[0].Name != "VI_1" || in[0].Interface != "eth1" || in[0].VRID != 51 {
			t.Fatalf("%s: %+v", name, in)
		}
		return in[0]
	}
	val := func(p *uint64) uint64 {
		t.Helper()
		if p == nil {
			t.Fatal("a counter keepalived 2.4.3 reports is missing")
		}
		return *p
	}
	errs := func(in Instance) {
		t.Helper()
		// Every error the build counts, nothing it doesn't: no
		// advert_interval_err (never counted), no authentication counter
		// (--disable-vrrp-auth).
		if len(in.Counters.AdvertErrors) != len(AdvertErrorNames) {
			t.Fatalf("advert errors %v", in.Counters.AdvertErrors)
		}
		for _, e := range AdvertErrorNames {
			if v, ok := in.Counters.AdvertErrors[e.Name]; !ok || v != 0 {
				t.Fatalf("advert error %s: %d, %v", e.Name, v, ok)
			}
		}
	}

	// A, priority 150: master, then FAULT (its HAProxy stopped - B
	// took over and A heard B's adverts), then master again.
	a := read("keepalived-2.4.3-master.json")
	if a.State != "MASTER" || a.EffectivePriority != 150 || len(a.VirtualIPs) != 1 || a.VirtualIPs[0] != "192.168.77.100/24" {
		t.Fatalf("master %+v", a)
	}
	c := a.Counters
	if a.BecameMaster != 2 || val(c.LeftMaster) != 1 || val(c.AdvertsSent) != 8 || val(c.AdvertsReceived) != 4 ||
		val(c.PriorityZeroSent) != 1 || val(c.PriorityZeroReceived) != 0 {
		t.Fatalf("master: became %d, left %d, sent %d, received %d, priority 0 sent %d, received %d", a.BecameMaster,
			val(c.LeftMaster), val(c.AdvertsSent), val(c.AdvertsReceived), val(c.PriorityZeroSent), val(c.PriorityZeroReceived))
	}
	errs(a)

	// B, priority 100: backup, master while A was in FAULT, backup again.
	// A gave the addresses up with two priority 0 adverts and counted one
	// (keepalived counts the one sent when it restores the interface, not
	// the one sent entering FAULT).
	b := read("keepalived-2.4.3-backup.json")
	if b.State != "BACKUP" || b.EffectivePriority != 100 {
		t.Fatalf("backup %+v", b)
	}
	c = b.Counters
	if b.BecameMaster != 1 || val(c.LeftMaster) != 1 || val(c.AdvertsSent) != 9 || val(c.AdvertsReceived) != 8 ||
		val(c.PriorityZeroSent) != 0 || val(c.PriorityZeroReceived) != 2 {
		t.Fatalf("backup: became %d, left %d, sent %d, received %d, priority 0 sent %d, received %d", b.BecameMaster,
			val(c.LeftMaster), val(c.AdvertsSent), val(c.AdvertsReceived), val(c.PriorityZeroSent), val(c.PriorityZeroReceived))
	}
	errs(b)
}

// A counter the dump doesn't carry is nil - never a 0 -, and a value of
// another kind costs that counter only.
func TestParseJSONCounters(t *testing.T) {
	in, err := ParseJSON([]byte(`[{"data":{"iname":"VI_1","state":1},"stats":{"advert_rcvd":7,"advert_sent":"x","become_master":2,"ip_ttl_err":3,"auth_failure":9}}]`))
	if err != nil {
		t.Fatal(err)
	}
	c := in[0].Counters
	if c.AdvertsReceived == nil || *c.AdvertsReceived != 7 || in[0].BecameMaster != 2 {
		t.Fatalf("%+v", in[0])
	}
	if c.AdvertsSent != nil || c.LeftMaster != nil || c.PriorityZeroSent != nil || c.PriorityZeroReceived != nil {
		t.Fatalf("absent or malformed counters read: %+v", c)
	}
	if len(c.AdvertErrors) != 1 || c.AdvertErrors["ttl"] != 3 {
		t.Fatalf("advert errors %v", c.AdvertErrors)
	}
	// An older dump without "stats" at all.
	in, err = ParseJSON([]byte(`[{"data":{"iname":"VI_1","state":2}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if c := in[0].Counters; c.AdvertsSent != nil || c.AdvertErrors != nil || in[0].BecameMaster != 0 {
		t.Fatalf("%+v", in[0])
	}
}

func TestKeepHealth(t *testing.T) {
	RunDir = t.TempDir()
	var healthy atomic.Bool
	healthy.Store(true)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { KeepHealth(healthy.Load, nil, 10*time.Millisecond, stop); close(done) }()
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

// A change signalled through changed is acted on at once, not at the next
// poll: a deliberate stop of HAProxy gives the virtual IPs up before its
// listeners close.
func TestKeepHealthWakesOnChange(t *testing.T) {
	RunDir = t.TempDir()
	var healthy atomic.Bool
	healthy.Store(true)
	var mu sync.Mutex
	ch := make(chan struct{})
	changed := func() <-chan struct{} { mu.Lock(); defer mu.Unlock(); return ch }
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { KeepHealth(healthy.Load, changed, time.Hour, stop); close(done) }()
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			data, _ := os.ReadFile(HealthFile())
			if strings.TrimSpace(string(data)) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("health file %q, want %q - polling is hourly here", data, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("0")
	healthy.Store(false)
	mu.Lock()
	close(ch)
	ch = make(chan struct{})
	mu.Unlock()
	wait("1")
	close(stop)
	<-done
}

func TestUnsupported(t *testing.T) {
	config := `global_defs {
    disable_local_igmp      # VMACs' companion
}
vrrp_instance VI_1 {
    interface eth1
    @node-a use_vmac vrrp.51
    use_ipvlan{
    # use_vmac in a comment
    ! use_vmac in the other kind
    virtual_router_id 51
}
`
	errs := Unsupported(config)
	want := []string{
		"(keepalived.conf: Line 2) disable_local_igmp isn't supported",
		"(keepalived.conf: Line 6) use_vmac isn't supported",
		"(keepalived.conf: Line 7) use_ipvlan isn't supported",
	}
	if len(errs) != len(want) {
		t.Fatalf("Unsupported = %q, want %d lines", errs, len(want))
	}
	for i, w := range want {
		if !strings.HasPrefix(errs[i], w) {
			t.Errorf("line %d: %q, want it to start with %q", i, errs[i], w)
		}
	}
	if errs := Unsupported("vrrp_instance VI_1 {\n    notify_master \"#use_vmac\"\n    virtual_router_id 51\n}\n"); len(errs) != 0 {
		t.Errorf("a standard configuration: %q", errs)
	}

	// Check refuses before keepalived runs: no binary needed.
	m := New(nil)
	if errs, err := m.Check(config); err == nil || len(errs) != 3 {
		t.Errorf("Check = %q, %v", errs, err)
	}
}
