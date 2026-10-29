package main

import (
	"maps"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// A node's labels are its own (store.Node.Labels, PATCH /api/nodes/{id},
// janus_node.labels) and, for a node the Controller created on a
// hypervisor, that hypervisor's (dc=lgs, room=a, rack=12) under them:
// the node's own label wins on the same key. Nothing is copied: they're
// put together each time they're read, so a hypervisor's change follows
// at once and the node's own stay the only ones it - or Terraform -
// owns. Whatever decides something by labels - grants, scoped tokens and
// SSO rules (access.go), janusctl's scoped certificates, the log
// forwarding profiles - takes nodeLabels.

// labelSource is where a node's inherited labels come from.
type labelSource struct {
	Kind string `json:"kind"` // "hypervisor"
	ID   string `json:"id"`
	Name string `json:"name"`
}

// inheritedLabels are the labels node n inherits - its machine's
// hypervisor's, all of them, those its own override included - and
// where from; none for a node that isn't a machine of this Controller,
// or whose hypervisor has none.
func (a *app) inheritedLabels(n *store.Node) (map[string]string, *labelSource) {
	if n.MachineID == "" || a.machines == nil || a.hypervisors == nil {
		return nil, nil
	}
	hvID, ok := a.machines.HypervisorOf(n.MachineID)
	if !ok || hvID == "" {
		return nil, nil
	}
	h, ok := a.hypervisors.Get(hvID)
	if !ok || len(h.Labels) == 0 {
		return nil, nil
	}
	return h.Labels, &labelSource{Kind: "hypervisor", ID: h.ID, Name: h.Name}
}

// nodeLabels are node n's effective labels: those it inherits, its own
// on top - what grants, scopes and profiles match. A fresh map, nil
// when it has none.
func (a *app) nodeLabels(n *store.Node) map[string]string {
	own := n.LabelSet()
	inherited, _ := a.inheritedLabels(n)
	if len(inherited) == 0 {
		return own
	}
	out := maps.Clone(inherited)
	maps.Copy(out, own)
	return out
}
