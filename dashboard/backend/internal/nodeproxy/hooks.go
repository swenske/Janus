package nodeproxy

import (
	"fmt"
	"net/http"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Hooks dashboardd sets: a node's page knows nothing of the machine it
// may run in (docs/hypervisors.md).
var (
	// LockedBy names what manages node's machine ("terraform") when it's
	// locked against changes made from the Controller's pages, else "".
	LockedBy = func(*store.Node) string { return "" }
	// NodeChanged is called after a change made from node's page - a
	// network configuration, an update - so its machine's record is read
	// again from the node.
	NodeChanged = func(*store.Node) {}
)

// refuseLocked answers 423 when node's machine is locked: a change made
// here would be reverted by the next run of what manages it.
func refuseLocked(w http.ResponseWriter, node *store.Node) bool {
	by := LockedBy(node)
	if by == "" {
		return false
	}
	http.Error(w, fmt.Sprintf("this node is managed by %s and locked: a change made here would be undone by its next run - change it there, or release it on the Controller's Hypervisors tab", by), http.StatusLocked)
	return true
}
