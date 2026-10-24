package api

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A node's state holds its private keys: never through the Controller's
// relay, and only where there is STATE.
func TestStateTransferAllowed(t *testing.T) {
	s := &System{StateRoot: t.TempDir()}
	relayed := Caller{Name: "alice", Roles: []string{"os:admin"}, Via: "janus-controller"}
	if got := status.Code(s.stateTransferAllowed(relayed)); got != codes.PermissionDenied {
		t.Errorf("relayed by the Controller: %v", got)
	}
	direct := Caller{Name: "admin", Roles: []string{"os:admin"}}
	if err := s.stateTransferAllowed(direct); err != nil {
		t.Errorf("the node's own admin: %v", err)
	}
	fleet := Caller{Name: "alice", Roles: []string{"os:admin"}, Fleet: true}
	if err := s.stateTransferAllowed(fleet); err != nil {
		t.Errorf("a fleet admin calling the node itself: %v", err)
	}
	if got := status.Code((&System{}).stateTransferAllowed(direct)); got != codes.FailedPrecondition {
		t.Errorf("no STATE: %v", got)
	}
}
