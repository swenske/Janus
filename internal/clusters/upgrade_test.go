package clusters

import (
	"strings"
	"testing"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// A member announces when its haproxy_* protocols are up and a BGP
// session established exports a route - or when it has no BGP gate.
func TestAnnouncing(t *testing.T) {
	running := janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	gate := func(state string, held bool) *janusv1alpha1.BGPProtocol {
		return &janusv1alpha1.BGPProtocol{Name: "haproxy_anycast", Protocol: "Static", State: state, HeldDown: held}
	}
	session := func(bgpState string, exported uint32) *janusv1alpha1.BGPProtocol {
		return &janusv1alpha1.BGPProtocol{Name: "router", Protocol: "BGP", State: "up", BgpState: bgpState,
			Channels: []*janusv1alpha1.BGPChannel{{Name: "ipv4", Exported: exported}}}
	}
	for _, tc := range []struct {
		name string
		st   *janusv1alpha1.BGPStatusResponse
		want string
	}{
		{"no BIRD", &janusv1alpha1.BGPStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED}, ""},
		{"announcing", &janusv1alpha1.BGPStatusResponse{State: running, Protocols: []*janusv1alpha1.BGPProtocol{gate("up", false), session("Established", 1)}}, ""},
		{"held down", &janusv1alpha1.BGPStatusResponse{State: running, Protocols: []*janusv1alpha1.BGPProtocol{gate("down", true), session("Established", 0)}}, "held down"},
		{"session coming back", &janusv1alpha1.BGPStatusResponse{State: running, Protocols: []*janusv1alpha1.BGPProtocol{gate("up", false), session("Active", 0)}}, "no BGP session"},
		{"established, nothing exported yet", &janusv1alpha1.BGPStatusResponse{State: running, Protocols: []*janusv1alpha1.BGPProtocol{gate("up", false), session("Established", 0)}}, "no BGP session"},
		{"no gate: BGP for something else", &janusv1alpha1.BGPStatusResponse{State: running, Protocols: []*janusv1alpha1.BGPProtocol{session("Active", 0)}}, ""},
	} {
		got := announcing(tc.st)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
