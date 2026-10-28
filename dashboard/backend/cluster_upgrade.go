package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/schematic"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Updating a cluster in place (docs/guide/clusters.md "Updating a
// cluster", the engine's Upgrade): each member gets its own A/B update,
// one at a time, drained before and undrained once back with its stick
// tables. A member that is one of this Controller's machines goes through
// the machine's update path - its record says updating, its spec's
// version and image follow once it is back -, any other node gets the
// release's bundle (or the image factory's for its schematic) itself.
// PATCH /api/machines/{id} changing a member's version or image goes
// through the same, for that member alone.

// clusterUpgradeRequest is POST /api/clusters/{id}/upgrade.
type clusterUpgradeRequest struct {
	// Members are updated in this order; none: every one, a VRRP
	// cluster's MASTERs last.
	Members []string `json:"members,omitempty"`
	// Version: the release they go to.
	Version string `json:"version"`
	// Extensions, HAProxy, Kernel: another image (with NewImage); unset:
	// each member's own.
	Extensions *[]string `json:"extensions,omitempty"`
	HAProxy    *string   `json:"haproxy,omitempty"`
	Kernel     *string   `json:"kernel,omitempty"`
	// NewImage: the members may come back on another image than the one
	// they run.
	NewImage bool `json:"new_image,omitempty"`
	// RebootMode: "kexec" or "firmware"; empty: a machine's own update
	// reboot mode, the firmware for any other node.
	RebootMode string `json:"reboot_mode,omitempty"`
}

// The engine's waits (vars: the tests shorten them).
var (
	clusterUpgradePoll = 2 * time.Second
	clusterUpgradeBack = clusters.UpgradeTimeout
)

// memberUpgrade is a member's part of an update.
type memberUpgrade struct {
	node *store.Node
	// machineID: the Controller's machine its node runs on; "" for none.
	machineID string
	target    *schematic.Schematic
	// change: its image changes.
	change bool
	mode   janusv1alpha1.RebootMode
}

// upgradePlan is an update checked against its cluster.
type upgradePlan struct {
	version string
	members map[string]*memberUpgrade
	opts    clusters.UpgradeOptions
}

// requestError is a refusal and its HTTP status.
type requestError struct {
	code int
	msg  string
}

func (e *requestError) Error() string { return e.msg }

func badRequest(format string, args ...any) *requestError {
	return &requestError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// planClusterUpgrade checks req against cluster c - whatever can be
// checked before anything is touched: the members, the release, each
// member's image and its bundle, the cluster's health.
func (a *app) planClusterUpgrade(ctx context.Context, c *clusters.Cluster, req clusterUpgradeRequest) (*upgradePlan, *requestError) {
	if !versionRe.MatchString(req.Version) {
		return nil, badRequest("version %q isn't a Janus release (vYYYY.MM.DD[-N])", req.Version)
	}
	switch req.RebootMode {
	case "", "firmware", "kexec":
	default:
		return nil, badRequest("reboot_mode %q: firmware or kexec", req.RebootMode)
	}
	order, err := a.clusters.UpgradeOrder(ctx, c, req.Members)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	p := &upgradePlan{version: req.Version, members: map[string]*memberUpgrade{}}
	shared := ""
	for _, m := range order {
		node, ok := a.store.Get(m.NodeID)
		if !ok {
			return nil, badRequest("%s: its node is no longer one of the Controller's", m.Name)
		}
		mu := &memberUpgrade{node: node, mode: janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT}
		var own *schematic.Schematic
		if mach := a.machineOf(m.NodeID); mach != nil {
			if mach.Spec.Image != nil {
				return nil, badRequest("%s's machine was created from an image URL: it can't be updated in place - recycle it", m.Name)
			}
			if mach.Phase != machines.PhaseReady || a.runner.busy(mach.ID) {
				return nil, &requestError{http.StatusConflict, fmt.Sprintf("%s's machine is %s: wait for it to be ready", m.Name, mach.Phase)}
			}
			mu.machineID, own = mach.ID, mach.Spec.Schematic()
			if mach.Spec.UpdateRebootMode == "kexec" {
				mu.mode = janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC
			}
		} else {
			v, err := a.clusters.Nodes.Version(ctx, m)
			if err != nil {
				return nil, badRequest("%s doesn't answer: %v", m.Name, err)
			}
			own = nodeproxy.NodeSchematic(v)
		}
		switch req.RebootMode {
		case "kexec":
			mu.mode = janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC
		case "firmware":
			mu.mode = janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT
		}
		if err := own.Normalize(); err != nil {
			return nil, badRequest("%s's image: %v", m.Name, err)
		}
		target := &schematic.Schematic{Customization: own.Customization}
		target.Customization.Extensions = slices.Clone(own.Customization.Extensions)
		if req.Extensions != nil {
			target.Customization.Extensions = slices.Clone(*req.Extensions)
		}
		if req.HAProxy != nil {
			target.Customization.HAProxy = *req.HAProxy
		}
		if req.Kernel != nil {
			target.Customization.Kernel = *req.Kernel
		}
		if err := target.Normalize(); err != nil {
			return nil, badRequest("%v", err)
		}
		mu.target, mu.change = target, target.ID() != own.ID()
		if mu.change && !req.NewImage {
			return nil, badRequest("%s's image would change (extensions, HAProxy branch or kernel track): set new_image to update it onto another image", m.Name)
		}
		if req.NewImage {
			if shared != "" && target.ID() != shared {
				return nil, badRequest("the members would end up on different images (%s's is %s): update them separately", m.Name, clusters.ShortID(target.ID()))
			}
			shared = target.ID()
		}
		// The bundle: one the image factory still builds is waited for.
		if _, state, err := nodes.ResolveBundle(ctx, req.Version, target); err != nil && state != "building" {
			return nil, badRequest("%s: no update to Janus %s for its image (schematic %s): %v", m.Name, req.Version, clusters.ShortID(target.ID()), err)
		}
		p.members[m.Name] = mu
	}
	// Healthy first: checked for the first member the update touches.
	for _, m := range order {
		v, err := a.clusters.Nodes.Version(ctx, m)
		if err != nil {
			return nil, badRequest("%s doesn't answer: %v", m.Name, err)
		}
		if v.GetVersion() == req.Version && (shared == "" || v.GetSchematicId() == shared) {
			continue
		}
		if err := a.clusters.UpgradeCheck(ctx, c, m); err != nil {
			return nil, badRequest("the cluster isn't healthy: %v", err)
		}
		break
	}
	for _, m := range order {
		p.opts.Members = append(p.opts.Members, m.Name)
	}
	p.opts.Release, p.opts.Schematic = req.Version, shared
	p.opts.Poll, p.opts.BackTimeout = clusterUpgradePoll, clusterUpgradeBack
	return p, nil
}

// upgradeJob is the update p of cluster id, as the cluster's operation.
func (a *app) upgradeJob(id string, p *upgradePlan, actor string) func(ctx context.Context) error {
	started := map[string]bool{}
	p.opts.Update = func(ctx context.Context, m clusters.Member) (string, error) {
		mu := p.members[m.Name]
		if mu.machineID != "" {
			started[m.Name] = true
		}
		return a.upgradeMemberNode(ctx, id, m, mu, p.version, actor)
	}
	p.opts.Done = func(m clusters.Member, err error) {
		if mu := p.members[m.Name]; mu != nil && started[m.Name] {
			a.upgradedMachine(mu, p.version, err)
		}
	}
	return func(ctx context.Context) error {
		return a.clusters.Upgrade(ctx, id, p.opts, actor)
	}
}

// upgradeMemberNode starts member m's node's own update, returning once
// it reboots: through its machine's update path when it is one, so the
// machine's record says so.
func (a *app) upgradeMemberNode(ctx context.Context, id string, m clusters.Member, mu *memberUpgrade, version, actor string) (string, error) {
	var b *nodeproxy.Bundle
	var err error
	if mu.machineID != "" {
		c, _ := a.clusters.Store.Get(id)
		name := id
		if c != nil {
			name = c.Name
		}
		if err := a.runner.phase(mu.machineID, machines.PhaseUpdating, "updating: cluster %s's update to Janus %s", name, version); err != nil {
			return "", err
		}
		b, err = a.runner.resolveBundle(ctx, mu.machineID, version, mu.target)
	} else {
		b, err = a.clusterBundle(ctx, id, m.Name, version, mu.target, actor)
	}
	if err != nil {
		return "", err
	}
	if mu.machineID != "" {
		a.runner.logEvent(mu.machineID, "updating the node to Janus %s, schematic %.8s", b.Version, b.Schematic)
	}
	uctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	defer cancel()
	src := &janusv1alpha1.ImageSource{Reference: b.BaseURL, Sha256: b.SHA256, AllowSchematicChange: mu.change}
	how, err := nodes.Upgrade(uctx, mu.node, src, mu.mode)
	if err != nil {
		return "", err
	}
	if how == "" {
		how = "rebooting"
	}
	if mu.machineID != "" {
		a.runner.logEvent(mu.machineID, "the node is rebooting into Janus %s - %s", b.Version, how)
	}
	return how, nil
}

// clusterBundle finds the update bundle for a member that isn't a
// machine, waiting while the image factory builds it.
func (a *app) clusterBundle(ctx context.Context, id, name, version string, sc *schematic.Schematic, actor string) (*nodeproxy.Bundle, error) {
	deadline := time.Now().Add(factoryBuildTimeout)
	reported := false
	for {
		b, state, err := nodes.ResolveBundle(ctx, version, sc)
		if err == nil {
			return b, nil
		}
		if state != "building" {
			return nil, fmt.Errorf("find the update: %w", err)
		}
		if !reported {
			a.clusters.Journal(id, actor, "upgrade", name, 0, "the image factory is building Janus %s for %s", version, name)
			reported = true
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the image factory hasn't built the update within %s", factoryBuildTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// upgradedMachine records how a member machine's update ended - its spec
// follows once it serves the target - and reads it back from its node.
func (a *app) upgradedMachine(mu *memberUpgrade, version string, err error) {
	_, _ = a.machines.Update(mu.machineID, func(m *machines.Machine) error {
		m.Phase = machines.PhaseReady
		if err != nil {
			m.Error = "update failed: " + err.Error()
			m.Log("%s", m.Error)
			return nil
		}
		m.Error = ""
		m.Spec.Version = version
		m.Spec.Extensions, m.Spec.HAProxy, m.Spec.Kernel = mu.target.Customization.Extensions, mu.target.Customization.HAProxy, mu.target.Customization.Kernel
		m.Log("the node runs Janus %s - updated", version)
		return nil
	})
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = a.runner.syncAs(sctx, mu.machineID, "read back from the node after the update")
}

func (a *app) handleClusterUpgrade(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req clusterUpgradeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if a.clusters.isBusy(c.ID) {
		writeError(w, http.StatusConflict, "an operation already runs on cluster "+c.Name+": wait for it to end")
		return
	}
	ctx, cancel := context.WithTimeout(nodeproxy.ForCluster(r.Context(), c.ID), 2*time.Minute)
	p, rerr := a.planClusterUpgrade(ctx, c, req)
	cancel()
	if rerr != nil {
		writeError(w, rerr.code, rerr.msg)
		return
	}
	actor := requestPrincipal(r).User
	a.runCluster(w, c, "upgrading", actor, a.upgradeJob(c.ID, p, actor))
}

// memberMachineUpgrade takes a machine's update p - a version or image
// change - through its cluster's update when its node is a member,
// answering for it (true); false: it is no member.
func (a *app) memberMachineUpgrade(w http.ResponseWriter, r *http.Request, m *machines.Machine, p *updatePlan) bool {
	if a.clusters == nil || m.NodeID == "" {
		return false
	}
	id := a.clusters.Store.ClusterOf(m.NodeID)
	c, ok := a.clusters.Store.Get(id)
	if id == "" || !ok {
		return false
	}
	member := c.MemberOf(m.NodeID)
	if p.hardware || p.network {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s is member %s of cluster %s: change its version alone - it goes through the cluster's update, which drains it first - and its hardware or network apart", m.Spec.Name, member.Name, c.Name))
		return true
	}
	if a.clusters.isBusy(c.ID) {
		writeError(w, http.StatusConflict, "an operation already runs on cluster "+c.Name+": wait for it to end")
		return true
	}
	// What only the record holds goes first: how its updates reboot is
	// this one's.
	if p.meta || p.autoscale {
		if _, err := a.machines.Update(m.ID, func(mm *machines.Machine) error {
			mm.Spec.ManagedBy, mm.Spec.Locked = p.next.ManagedBy, p.next.Locked
			if p.autoscale {
				logAutoscaleChange(mm, p.next.Autoscale)
			}
			if p.next.UpdateRebootMode != mm.Spec.UpdateRebootMode {
				mm.Spec.UpdateRebootMode = p.next.UpdateRebootMode
				mm.Log("its updates reboot %s", rebootWay(mm.Spec.UpdateRebootMode))
			}
			return nil
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return true
		}
	}
	ext, hap, kern := slices.Clone(p.next.Extensions), p.next.HAProxy, p.next.Kernel
	req := clusterUpgradeRequest{Members: []string{member.Name}, Version: p.version, Extensions: &ext, HAProxy: &hap, Kernel: &kern,
		NewImage: p.image.ID() != m.Schematic, RebootMode: map[string]string{"kexec": "kexec"}[p.next.UpdateRebootMode]}
	ctx, cancel := context.WithTimeout(nodeproxy.ForCluster(r.Context(), c.ID), 2*time.Minute)
	plan, rerr := a.planClusterUpgrade(ctx, c, req)
	cancel()
	if rerr != nil {
		writeError(w, rerr.code, rerr.msg)
		return true
	}
	actor := requestPrincipal(r).User
	if !a.clusters.start(c.ID, "upgrading "+member.Name, actor, a.upgradeJob(c.ID, plan, actor)) {
		writeError(w, http.StatusConflict, "an operation already runs on cluster "+c.Name+": wait for it to end")
		return true
	}
	a.clusters.Journal(c.ID, actor, "upgrade", member.Name, 0, "%s's machine %s changed to Janus %s: through the cluster's update", member.Name, m.Spec.Name, p.version)
	m, _ = a.machines.Get(m.ID)
	writeJSON(w, http.StatusAccepted, a.machineView(m))
	return true
}
