package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Changing a machine in place (PATCH /api/machines/{id}) - what a
// Terraform plan applies without replacing the node:
//
//   - its network (addresses, gateways, modes, DNS, NTP): put on trial on
//     the node and confirmed, or the node goes back by itself;
//   - its size (vCPUs, memory): the node shuts down cleanly, the virtual
//     machine is resized and started again;
//   - its version and extensions: the node's own A/B update, from the
//     release's bundle or the image factory's, checked by the node and
//     confirmed healthy.
//
// Its name, hypervisor, interfaces (count, networks, names, MACs) and
// image don't change in place: that's a new machine.

// nodeAPI is what updating and destroying a machine asks of its node - a
// variable so tests give a fake.
type nodeAPI interface {
	Shutdown(ctx context.Context, node *store.Node) error
	ApplyNetwork(ctx context.Context, node *store.Node, st *store.Store, cfg *janusv1alpha1.NetworkConfig) error
	Upgrade(ctx context.Context, node *store.Node, source *janusv1alpha1.ImageSource) error
	Version(ctx context.Context, node *store.Node) (version, schematicID string, err error)
	ResolveBundle(ctx context.Context, version string, extensions []string) (*nodeproxy.Bundle, string, error)
}

type realNodeAPI struct{}

func (realNodeAPI) Shutdown(ctx context.Context, node *store.Node) error {
	return nodeproxy.Shutdown(ctx, node)
}

func (realNodeAPI) ApplyNetwork(ctx context.Context, node *store.Node, st *store.Store, cfg *janusv1alpha1.NetworkConfig) error {
	return nodeproxy.ApplyNetwork(ctx, node, st, cfg)
}

func (realNodeAPI) Upgrade(ctx context.Context, node *store.Node, source *janusv1alpha1.ImageSource) error {
	return nodeproxy.Upgrade(ctx, node, source)
}

func (realNodeAPI) Version(ctx context.Context, node *store.Node) (string, string, error) {
	return nodeproxy.NodeVersion(ctx, node)
}

func (realNodeAPI) ResolveBundle(ctx context.Context, version string, extensions []string) (*nodeproxy.Bundle, string, error) {
	return nodeproxy.ResolveBundle(ctx, version, extensions)
}

var nodes nodeAPI = realNodeAPI{}

// machineUpdate is a PATCH body: what's given is the machine's new value.
type machineUpdate struct {
	VCPUs      *int            `json:"vcpus,omitempty"`
	MemoryMiB  *int            `json:"memory_mib,omitempty"`
	Version    *string         `json:"version,omitempty"`
	Extensions *[]string       `json:"extensions,omitempty"`
	NICs       *[]machines.NIC `json:"nics,omitempty"`
	DNS        *[]string       `json:"dns,omitempty"`
	NTP        *[]string       `json:"ntp,omitempty"`
}

// updatePlan is what an update changes.
type updatePlan struct {
	next    machines.Spec
	network bool
	size    bool
	upgrade bool
	// The version and extensions to upgrade to.
	version    string
	extensions []string
}

const (
	nodeBackTimeout = 10 * time.Minute
	upgradeTimeout  = 20 * time.Minute
)

// planUpdate checks an update against the machine and says what it
// changes.
func planUpdate(m *machines.Machine, u machineUpdate) (*updatePlan, error) {
	cur := m.Spec
	next := cur
	next.NICs = append([]machines.NIC(nil), cur.NICs...)
	p := &updatePlan{version: m.Version, extensions: cur.Extensions}

	if u.VCPUs != nil {
		next.VCPUs = *u.VCPUs
	}
	if u.MemoryMiB != nil {
		next.MemoryMiB = *u.MemoryMiB
	}
	if next.VCPUs < 1 || next.VCPUs > 64 {
		return nil, errors.New("vcpus: 1 to 64")
	}
	if next.MemoryMiB < 512 || next.MemoryMiB > 262144 {
		return nil, errors.New("memory_mib: 512 to 262144")
	}
	p.size = next.VCPUs != cur.VCPUs || next.MemoryMiB != cur.MemoryMiB

	if u.Version != nil || u.Extensions != nil {
		if cur.Image != nil {
			return nil, errors.New("this machine was created from an image URL: its version and extensions can't be changed, only replaced")
		}
		if u.Version != nil {
			if !versionRe.MatchString(*u.Version) {
				return nil, fmt.Errorf("version %q isn't a Janus release (vYYYY.MM.DD[-N])", *u.Version)
			}
			p.version = *u.Version
			next.Version = *u.Version
		}
		if u.Extensions != nil {
			sc := &schematic.Schematic{Customization: schematic.Customization{Extensions: append([]string(nil), *u.Extensions...)}}
			if err := sc.Normalize(); err != nil {
				return nil, err
			}
			p.extensions = sc.Customization.Extensions
			next.Extensions = sc.Customization.Extensions
		}
		want := &schematic.Schematic{Customization: schematic.Customization{Extensions: append([]string(nil), p.extensions...)}}
		if err := want.Normalize(); err != nil {
			return nil, err
		}
		p.upgrade = p.version != m.Version || want.ID() != m.Schematic
	}

	if u.NICs != nil {
		nics := *u.NICs
		if len(nics) != len(cur.NICs) {
			return nil, errors.New("the number of interfaces can't change in place: that's a new machine")
		}
		for i, n := range nics {
			c := cur.NICs[i]
			if n.MAC == "" {
				n.MAC = c.MAC
			}
			if n.Mode == "" {
				n.Mode = "dhcp"
			}
			if n.Network != c.Network || n.Name != c.Name || n.MAC != c.MAC {
				return nil, fmt.Errorf("interface %d's network, name and MAC address can't change in place: that's a new machine", i)
			}
			next.NICs[i] = n
		}
	}
	if u.DNS != nil {
		next.DNS = *u.DNS
	}
	if u.NTP != nil {
		next.NTP = *u.NTP
	}
	if _, err := networkConfig(next); err != nil {
		return nil, err
	}
	p.network = !nicsEqual(next.NICs, cur.NICs) || !slices.Equal(next.DNS, cur.DNS) || !slices.Equal(next.NTP, cur.NTP)
	p.next = next
	return p, nil
}

func nicsEqual(a, b []machines.NIC) bool {
	return slices.EqualFunc(a, b, func(x, y machines.NIC) bool {
		return x.Network == y.Network && x.Name == y.Name && x.MAC == y.MAC && x.Mode == y.Mode &&
			slices.Equal(x.Addresses, y.Addresses) && x.Gateway == y.Gateway
	})
}

func (a *app) handleMachineUpdate(w http.ResponseWriter, r *http.Request) {
	m, ok := a.getMachine(w, r)
	if !ok {
		return
	}
	var u machineUpdate
	if !decodeBody(w, r, &u) {
		return
	}
	if m.Phase != machines.PhaseReady || m.NodeID == "" || m.Ref == nil || m.Ref.UUID == "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("the machine is %s: only a ready machine with its node can be changed", m.Phase))
		return
	}
	if _, ok := a.store.Get(m.NodeID); !ok {
		writeError(w, http.StatusConflict, "the machine's node is gone")
		return
	}
	p, err := planUpdate(m, u)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !p.network && !p.size && !p.upgrade {
		writeJSON(w, http.StatusOK, a.machineView(m))
		return
	}
	id := m.ID
	if err := a.runner.phase(id, machines.PhaseUpdating, "updating"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !a.runner.start(id, func(ctx context.Context) error { return a.runner.update(ctx, id, p) }) {
		writeError(w, http.StatusConflict, "the machine is busy")
		return
	}
	m, _ = a.machines.Get(id)
	writeJSON(w, http.StatusAccepted, a.machineView(m))
}

// update is the update job. Whatever happens, the machine stays ready -
// its node is there - with the error if a step failed; the steps done
// are kept in its spec.
func (r *machineRunner) update(ctx context.Context, id string, p *updatePlan) error {
	err := r.updateSteps(ctx, id, p)
	_, uerr := r.a.machines.Update(id, func(m *machines.Machine) error {
		m.Phase = machines.PhaseReady
		if err != nil {
			m.Error = "update failed: " + err.Error()
			m.Log("%s", m.Error)
		} else {
			m.Error = ""
			m.Log("updated")
		}
		return nil
	})
	return uerr
}

func (r *machineRunner) updateSteps(ctx context.Context, id string, p *updatePlan) error {
	a := r.a
	m, ok := a.machines.Get(id)
	if !ok {
		return errors.New("machine vanished")
	}
	node, ok := a.store.Get(m.NodeID)
	if !ok {
		return errors.New("its node is gone")
	}
	h, ok := a.hypervisors.Get(m.Spec.HypervisorID)
	if !ok {
		return errors.New("its hypervisor is gone")
	}
	drv, err := newDriver(h, a.controllerID)
	if err != nil {
		return err
	}
	defer drv.Close()

	if p.network {
		r.logEvent(id, "applying the network configuration on trial")
		cfg, err := networkConfig(p.next)
		if err != nil {
			return err
		}
		nctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = nodes.ApplyNetwork(nctx, node, a.store, cfg)
		cancel()
		if err != nil {
			return fmt.Errorf("network: %w", err)
		}
		if _, err := a.machines.Update(id, func(m *machines.Machine) error {
			m.Spec.NICs, m.Spec.DNS, m.Spec.NTP = p.next.NICs, p.next.DNS, p.next.NTP
			m.Log("network configuration confirmed - the node is at %s", node.Addr())
			return nil
		}); err != nil {
			return err
		}
	}

	if p.size {
		r.logEvent(id, "shutting the node down to resize it to %d vCPU, %d MiB", p.next.VCPUs, p.next.MemoryMiB)
		if err := stopMachine(ctx, drv, node, *m.Ref); err != nil {
			return err
		}
		if err := drv.Resize(ctx, *m.Ref, p.next.VCPUs, p.next.MemoryMiB); err != nil {
			_ = drv.Power(ctx, *m.Ref, hypervisor.PowerStart)
			return fmt.Errorf("resize: %w", err)
		}
		if _, err := a.machines.Update(id, func(m *machines.Machine) error {
			m.Spec.VCPUs, m.Spec.MemoryMiB = p.next.VCPUs, p.next.MemoryMiB
			return nil
		}); err != nil {
			return err
		}
		if err := drv.Power(ctx, *m.Ref, hypervisor.PowerStart); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		r.logEvent(id, "resized - waiting for the node")
		if _, _, err := waitNode(ctx, node, nodeBackTimeout, nil); err != nil {
			return err
		}
	}

	if p.upgrade {
		b, err := r.resolveBundle(ctx, id, p.version, p.extensions)
		if err != nil {
			return err
		}
		r.logEvent(id, "updating the node to Janus %s, schematic %.8s", b.Version, b.Schematic)
		uctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
		defer cancel()
		src := &janusv1alpha1.ImageSource{Reference: b.BaseURL, Sha256: b.SHA256, AllowSchematicChange: b.Schematic != m.Schematic}
		if err := nodes.Upgrade(uctx, node, src); err != nil {
			return fmt.Errorf("update: %w", err)
		}
		r.logEvent(id, "the node is rebooting into Janus %s", b.Version)
		if _, _, err := waitNode(uctx, node, upgradeTimeout, func(v, sc string) bool { return v == b.Version && sc == b.Schematic }); err != nil {
			return fmt.Errorf("after the update: %w", err)
		}
		if _, err := a.machines.Update(id, func(m *machines.Machine) error {
			m.Version, m.Schematic = b.Version, b.Schematic
			m.Spec.Version, m.Spec.Extensions = p.next.Version, p.next.Extensions
			m.Log("the node runs Janus %s", b.Version)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// resolveBundle finds the update bundle, waiting while the image
// factory builds it.
func (r *machineRunner) resolveBundle(ctx context.Context, id, version string, extensions []string) (*nodeproxy.Bundle, error) {
	deadline := time.Now().Add(factoryBuildTimeout)
	reported := false
	for {
		b, state, err := nodes.ResolveBundle(ctx, version, extensions)
		if err == nil {
			return b, nil
		}
		if state != "building" {
			return nil, fmt.Errorf("find the update: %w", err)
		}
		if !reported {
			r.logEvent(id, "the image factory is building Janus %s", version)
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

// stopMachine shuts the node down cleanly, then forces its virtual
// machine off if it's still running after a while.
func stopMachine(ctx context.Context, drv hypervisor.Driver, node *store.Node, ref hypervisor.MachineRef) error {
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := nodes.Shutdown(sctx, node)
	cancel()
	if err == nil {
		waitPoweredOff(ctx, drv, ref, 90*time.Second)
	}
	st, err := drv.MachineStatus(ctx, ref)
	if err != nil {
		return err
	}
	if st.Power != hypervisor.PowerOff {
		if err := drv.Power(ctx, ref, hypervisor.PowerForceOff); err != nil {
			return fmt.Errorf("power off: %w", err)
		}
	}
	return nil
}

// waitNode waits until the node answers - and, with ok, until what it
// answers suits.
func waitNode(ctx context.Context, node *store.Node, timeout time.Duration, ok func(version, schematic string) bool) (string, string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		v, sc, err := nodes.Version(vctx, node)
		cancel()
		if err == nil && (ok == nil || ok(v, sc)) {
			return v, sc, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("it runs %s (schematic %.8s)", v, sc)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", "", fmt.Errorf("the node isn't back after %s: %v", timeout, lastErr)
}
