package rbac

import (
	"slices"
	"testing"
)

// TestEffectsCoverEveryRPC: each RPC with a role has an effect, and the
// other way round - a new RPC can't be forgotten: the Read-only switch
// would offer or hide it by chance.
func TestEffectsCoverEveryRPC(t *testing.T) {
	for m := range Required {
		if _, ok := Effect[trim(m)]; !ok {
			t.Errorf("%s has no effect (read or change)", m)
		}
	}
	for m, e := range Effect {
		if _, ok := Required["/janus.v1alpha1."+m]; !ok {
			t.Errorf("Effect has a stale entry %s", m)
		}
		if e != EffectRead && e != EffectChange {
			t.Errorf("%s: unknown effect %q", m, e)
		}
	}
	if EffectOf("/test.Service/Unknown") != EffectChange {
		t.Error("an unclassified RPC isn't a change")
	}
	// What the issue (#527) decided: diagnosis reads, every apply changes.
	for m, want := range map[string]string{
		"SystemService/Probe":                       EffectRead,
		"/janus.v1alpha1.SystemService/Probe":       EffectRead,
		"SystemService/PacketCapture":               EffectRead,
		"SystemService/Logs":                        EffectRead,
		"HAProxyService/ValidateConfig":             EffectRead,
		"HAProxyService/ApplyConfig":                EffectChange,
		"HAProxyService/ServerSetState":             EffectChange,
		"NetworkService/FirewallApplyRuleset":       EffectChange,
		"NetworkService/NetworkConfigConfirm":       EffectChange,
		"LifecycleService/Upgrade":                  EffectChange,
		"SystemService/GenerateClientConfiguration": EffectChange,
	} {
		if got := EffectOf(m); got != want {
			t.Errorf("%s: %s, want %s", m, got, want)
		}
	}
	got := Reading([]string{"HAProxyService/Reload", "/janus.v1alpha1.HAProxyService/Stats", "SystemService/Probe", "Nope/Unknown"})
	if !slices.Equal(got, []string{"HAProxyService/Stats", "SystemService/Probe"}) {
		t.Errorf("Reading: %v", got)
	}
}
