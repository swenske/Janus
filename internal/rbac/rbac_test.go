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
