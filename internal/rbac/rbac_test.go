package rbac

import (
	"slices"
	"testing"

	"github.com/swenske/Janus/internal/pki"
)

func TestAllowing(t *testing.T) {
	reader := Allowing([]string{pki.RoleReader})
	operator := Allowing([]string{pki.RoleOperator})
	admin := Allowing([]string{pki.RoleAdmin})
	if len(admin) != len(Required) {
		t.Errorf("admin: %d of %d", len(admin), len(Required))
	}
	for _, c := range []struct {
		list []string
		m    string
		want bool
	}{
		{reader, "HAProxyService/ShowInfo", true},
		{reader, "HAProxyService/ApplyConfig", false},
		{operator, "HAProxyService/ApplyConfig", true},
		{operator, "NetworkService/FirewallApplyRuleset", false},
		{operator, "LifecycleService/Upgrade", false},
		{admin, "LifecycleService/Upgrade", true},
	} {
		if slices.Contains(c.list, c.m) != c.want {
			t.Errorf("%s: %v", c.m, !c.want)
		}
	}
	if Allowed("/test.Service/Unknown", []string{pki.RoleOperator}) || !Allowed("/test.Service/Unknown", []string{pki.RoleAdmin}) {
		t.Error("an unclassified RPC isn't admin-only")
	}
	if Allowing(nil) != nil || Allowing([]string{pki.RoleController}) != nil {
		t.Error("no role, or the Controller's own: nothing")
	}
}

// TestDomainsCoverEveryRPC: each RPC with a role has a domain, and the
// other way round - a new RPC can't be forgotten in either table.
func TestDomainsCoverEveryRPC(t *testing.T) {
	for m := range Required {
		if _, ok := Domain[trim(m)]; !ok {
			t.Errorf("%s has no domain", m)
		}
	}
	for m, d := range Domain {
		if _, ok := Required["/janus.v1alpha1."+m]; !ok {
			t.Errorf("Domain has a stale entry %s", m)
		}
		if d != DomainObserve && !slices.Contains(NodeDomains, d) {
			t.Errorf("%s: unknown domain %q", m, d)
		}
	}
	if DomainOf("/test.Service/Unknown") != DomainSystem {
		t.Error("an unplaced RPC isn't the widest domain")
	}
	for _, c := range []struct {
		method  string
		domains []string
		ok      bool
	}{
		{"HAProxyService/ApplyConfig", []string{DomainHAProxy}, true},
		{"/janus.v1alpha1.HAProxyService/ApplyConfig", []string{DomainHAProxy}, true},
		{"SystemService/Reboot", []string{DomainHAProxy}, false},
		{"SystemService/Stats", []string{DomainHAProxy}, true},
		{"NetworkService/NetworkConfigApply", nil, true},
		{"LifecycleService/Upgrade", []string{DomainServices, DomainNetwork}, false},
	} {
		if InDomains(c.method, c.domains) != c.ok {
			t.Errorf("%s in %v: %v", c.method, c.domains, !c.ok)
		}
	}
}
