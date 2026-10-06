package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/schematic"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// A machine's spec says what it is, not only what it was asked to be: a
// change made on its node's page (network, update), with janusctl, or on
// the hypervisor shows up in its record - and, for a machine managed as
// code, in the next plan, as a change to undo or adopt. The record is
// read again:
//
//   - after every change the Controller makes or relays,
//   - when asked (GET /api/machines/{id}?refresh=true - what the
//     Terraform provider reads before a plan),
//   - every syncEvery in the background.
//
// The name isn't read back: it's the machine's (its virtual machine is
// named after it); a hostname changed on the node is shown beside it.

const syncEvery = time.Minute

// syncFresh: a record read this recently isn't read again on request - a
// variable for tests.
var syncFresh = 10 * time.Second

// syncLoop keeps every ready machine's record current until ctx ends.
func (r *machineRunner) syncLoop(ctx context.Context) {
	t := time.NewTicker(syncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, m := range r.a.machines.List() {
			if m.Phase != machines.PhaseReady || m.NodeID == "" {
				continue
			}
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = r.sync(sctx, m.ID)
			cancel()
		}
	}
}

// syncNode reads a node's machine again after a change made from its
// page - at once, and again a little later, once an update has rebooted
// it.
func (r *machineRunner) syncNode(node *store.Node) {
	if node.MachineID == "" {
		return
	}
	go func() {
		for _, after := range []time.Duration{2 * time.Second, 90 * time.Second} {
			time.Sleep(after)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = r.sync(ctx, node.MachineID)
			cancel()
		}
	}()
}

// sync reads a ready machine's version, extensions, network and hardware
// from its node and its hypervisor into its record.
func (r *machineRunner) sync(ctx context.Context, id string) error {
	return r.syncAs(ctx, id, "changed outside the Controller")
}

// syncAs is sync, its findings logged as label.
func (r *machineRunner) syncAs(ctx context.Context, id, label string) error {
	a := r.a
	m, ok := a.machines.Get(id)
	if !ok || m.Phase != machines.PhaseReady || m.NodeID == "" {
		return nil
	}
	node, ok := a.store.Get(m.NodeID)
	if !ok {
		return nil
	}
	var errs []string
	info, err := nodes.Info(ctx, node)
	if err != nil {
		errs = append(errs, "node: "+err.Error())
	}
	cfg, err := nodes.Network(ctx, node)
	if err != nil && info != nil {
		errs = append(errs, "network: "+err.Error())
	}
	var hw *hypervisor.MachineStatus
	if h, ok := a.hypervisors.Get(m.Spec.HypervisorID); ok && m.Ref != nil && m.Ref.UUID != "" {
		st := a.hypervisorStatus(ctx, h)
		hw = st.Machines[id]
		if hw == nil && st.Error != "" {
			errs = append(errs, "hypervisor: "+st.Error)
		}
	}

	_, err = a.machines.Update(id, func(m *machines.Machine) error {
		if m.Phase != machines.PhaseReady {
			return errSkip // a change started meanwhile: its own sync follows
		}
		var seen []string
		note := func(format string, args ...any) { seen = append(seen, fmt.Sprintf(format, args...)) }
		if info != nil {
			if m.Version != info.Version {
				note("version %s → %s", or(m.Version, "?"), info.Version)
				m.Version = info.Version
			}
			if m.Schematic != info.Schematic {
				m.Schematic = info.Schematic
			}
			sc := &schematic.Schematic{Customization: schematic.Customization{Extensions: append([]string(nil), info.Extensions...)}}
			if sc.Normalize() == nil && !slices.Equal(sc.Customization.Extensions, m.Spec.Extensions) {
				note("extensions [%s] → [%s]", strings.Join(m.Spec.Extensions, " "), strings.Join(sc.Customization.Extensions, " "))
				m.Spec.Extensions = sc.Customization.Extensions
			}
			if info.HAProxy != m.Spec.HAProxy {
				note("HAProxy branch %s → %s", or(m.Spec.HAProxy, "default"), or(info.HAProxy, "default"))
				m.Spec.HAProxy = info.HAProxy
			}
			if info.Kernel != m.Spec.Kernel {
				note("kernel track %s → %s", or(m.Spec.Kernel, "default"), or(info.Kernel, "default"))
				m.Spec.Kernel = info.Kernel
			}
		}
		if cfg != nil {
			m.NodeHostname = cfg.GetHostname()
			for i := range m.Spec.NICs {
				n := &m.Spec.NICs[i]
				for _, iface := range cfg.GetInterfaces() {
					if !strings.EqualFold(iface.GetMac(), n.MAC) {
						continue
					}
					was := *n
					n.Name, n.Mode, n.Addresses, n.Gateway = iface.GetName(), modeName(iface.GetMode()), nilIfEmpty(iface.GetAddresses()), iface.GetGateway()
					if !nicsConfigEqual([]machines.NIC{was}, []machines.NIC{*n}) {
						note("interface %s: %s %s → %s %s", was.Name, was.Mode, strings.Join(was.Addresses, " "), n.Mode, strings.Join(n.Addresses, " "))
					}
				}
			}
			if dns := nilIfEmpty(cfg.GetDns().GetServers()); !slices.Equal(dns, m.Spec.DNS) {
				note("DNS [%s] → [%s]", strings.Join(m.Spec.DNS, " "), strings.Join(dns, " "))
				m.Spec.DNS = dns
			}
			if ntp := nilIfEmpty(cfg.GetNtp().GetServers()); !slices.Equal(ntp, m.Spec.NTP) {
				note("NTP [%s] → [%s]", strings.Join(m.Spec.NTP, " "), strings.Join(ntp, " "))
				m.Spec.NTP = ntp
			}
		}
		if hw != nil && hw.Power == hypervisor.PowerRunning {
			if hw.VCPUs != m.Spec.VCPUs || hw.MemoryMiB != m.Spec.MemoryMiB {
				note("hardware %d vCPU %d MiB → %d vCPU %d MiB", m.Spec.VCPUs, m.Spec.MemoryMiB, hw.VCPUs, hw.MemoryMiB)
				m.Spec.VCPUs, m.Spec.MemoryMiB = hw.VCPUs, hw.MemoryMiB
			}
		}
		if len(seen) > 0 {
			m.Log("%s: %s", label, strings.Join(seen, "; "))
		}
		m.SyncedAt = time.Now().UTC()
		m.SyncError = strings.Join(errs, "; ")
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	if err != nil {
		log.Printf("machine %s: sync: %v", id, err)
	}
	return err
}

var errSkip = errors.New("skipped")

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
