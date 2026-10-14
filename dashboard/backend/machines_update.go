package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Changing a machine in place (PATCH /api/machines/{id}) - from the
// Hypervisors tab or a Terraform plan:
//
//   - its hardware - vCPUs, memory, the maxima auto-scaling may reach,
//     network interfaces (added, removed, moved to another network): the
//     node shuts down cleanly, the virtual machine is reconfigured and
//     started again;
//   - its auto-scaling's mode and thresholds: just the record;
//   - its network (interfaces' names, modes, addresses, gateways; DNS,
//     NTP): put on trial on the node and confirmed, or the node goes back
//     by itself - merged into the node's own configuration, so what only
//     its page sets (VLANs, MTUs, search domains, route metrics) stays;
//   - its version and extensions: the node's own A/B update, from the
//     release's bundle or the image factory's, checked by the node and
//     confirmed healthy;
//   - who manages it as code, and whether that locks it.
//
// Its name, hypervisor and image don't change in place: that's a new
// machine.

// nodeAPI is what updating, syncing and destroying a machine ask of its
// node - a variable so tests give a fake.
type nodeAPI interface {
	Shutdown(ctx context.Context, node *store.Node) error
	ApplyNetwork(ctx context.Context, node *store.Node, st *store.Store, cfg *janusv1alpha1.NetworkConfig) error
	Upgrade(ctx context.Context, node *store.Node, source *janusv1alpha1.ImageSource, mode janusv1alpha1.RebootMode) (string, error)
	Info(ctx context.Context, node *store.Node) (*nodeproxy.NodeInfo, error)
	Network(ctx context.Context, node *store.Node) (*janusv1alpha1.NetworkConfig, error)
	NetworkStatus(ctx context.Context, node *store.Node) (*janusv1alpha1.NetworkStatusResponse, error)
	ResolveBundle(ctx context.Context, version string, sc *schematic.Schematic) (*nodeproxy.Bundle, string, error)
	// Load and HAProxyConfig are what auto-scaling reads (autoscale.go);
	// the Sysctl calls how it makes the kernel parameters follow
	// (autoscale_sysctl.go).
	Load(ctx context.Context, node *store.Node) (*nodeproxy.Load, error)
	HAProxyConfig(ctx context.Context, node *store.Node) ([]byte, error)
	SysctlList(ctx context.Context, node *store.Node) (*janusv1alpha1.SysctlListResponse, error)
	SysctlHistory(ctx context.Context, node *store.Node, limit uint32) ([]*janusv1alpha1.SysctlHistoryEntry, error)
	SysctlApply(ctx context.Context, node *store.Node, req *janusv1alpha1.SysctlApplyRequest) (*janusv1alpha1.SysctlApplyResponse, error)
	SysctlConfirm(ctx context.Context, node *store.Node) error
}

type realNodeAPI struct{}

func (realNodeAPI) Shutdown(ctx context.Context, node *store.Node) error {
	return nodeproxy.Shutdown(ctx, node)
}

func (realNodeAPI) ApplyNetwork(ctx context.Context, node *store.Node, st *store.Store, cfg *janusv1alpha1.NetworkConfig) error {
	return nodeproxy.ApplyNetwork(ctx, node, st, cfg)
}

func (realNodeAPI) Upgrade(ctx context.Context, node *store.Node, source *janusv1alpha1.ImageSource, mode janusv1alpha1.RebootMode) (string, error) {
	return nodeproxy.Upgrade(ctx, node, source, mode)
}

func (realNodeAPI) Info(ctx context.Context, node *store.Node) (*nodeproxy.NodeInfo, error) {
	return nodeproxy.GetNodeInfo(ctx, node)
}

func (realNodeAPI) Network(ctx context.Context, node *store.Node) (*janusv1alpha1.NetworkConfig, error) {
	return nodeproxy.NetworkConfig(ctx, node)
}

func (realNodeAPI) NetworkStatus(ctx context.Context, node *store.Node) (*janusv1alpha1.NetworkStatusResponse, error) {
	return nodeproxy.NetworkStatus(ctx, node)
}

func (realNodeAPI) ResolveBundle(ctx context.Context, version string, sc *schematic.Schematic) (*nodeproxy.Bundle, string, error) {
	return nodeproxy.ResolveBundle(ctx, version, sc)
}

var nodes nodeAPI = realNodeAPI{}

// machineUpdate is a PATCH body: what's given is the machine's new value.
type machineUpdate struct {
	VCPUs      *int            `json:"vcpus,omitempty"`
	MemoryMiB  *int            `json:"memory_mib,omitempty"`
	Version    *string         `json:"version,omitempty"`
	Extensions *[]string       `json:"extensions,omitempty"`
	HAProxy    *string         `json:"haproxy,omitempty"`
	Kernel     *string         `json:"kernel,omitempty"`
	NICs       *[]machines.NIC `json:"nics,omitempty"`
	DNS        *[]string       `json:"dns,omitempty"`
	NTP        *[]string       `json:"ntp,omitempty"`
	ManagedBy  *string         `json:"managed_by,omitempty"`
	Locked     *bool           `json:"locked,omitempty"`
	// "kexec" or "firmware" - see machines.Spec.
	UpdateRebootMode *string `json:"update_reboot_mode,omitempty"`
	// Autoscale: a policy sets or changes it, null removes it - absent,
	// it stays as it is.
	Autoscale optionalAutoscale `json:"autoscale"`
}

// optionalAutoscale is an update's autoscale: given or not, null or a
// policy.
type optionalAutoscale struct {
	set    bool
	policy *machines.Autoscale
}

func (o *optionalAutoscale) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(b) == "null" {
		o.policy = nil
		return nil
	}
	o.policy = &machines.Autoscale{}
	return json.Unmarshal(b, o.policy)
}

// updateRebootMode is an update_reboot_mode as a spec keeps it: "kexec",
// or "" for the firmware ("" or "firmware" given).
func updateRebootMode(s string) (string, error) {
	switch s {
	case "", "firmware":
		return "", nil
	case "kexec":
		return "kexec", nil
	}
	return "", fmt.Errorf("update_reboot_mode %q: firmware or kexec", s)
}

// onlyRelease reports an update that changes nothing but unlocks the
// machine (or says who manages it) - what a locked machine's page may
// still do.
func (u machineUpdate) onlyRelease() bool {
	return u.VCPUs == nil && u.MemoryMiB == nil && u.Version == nil && u.Extensions == nil && u.HAProxy == nil && u.Kernel == nil && u.NICs == nil &&
		u.DNS == nil && u.NTP == nil && u.UpdateRebootMode == nil && !u.Autoscale.set && (u.Locked == nil || !*u.Locked)
}

// updatePlan is what an update changes.
type updatePlan struct {
	next machines.Spec
	// hardware: vCPUs, memory, the maxima auto-scaling may reach, or the
	// set of interfaces - a stop and a start.
	hardware bool
	// autoscale: the auto-scaling policy changes - its mode or
	// thresholds alone are just the record.
	autoscale bool
	// network: the node's network configuration to apply.
	network bool
	// added and removed interfaces, by MAC.
	added, removed []string
	// moved: interfaces moved to another network, by MAC.
	moved   []string
	upgrade bool
	// The version and schematic (extensions, HAProxy branch, kernel
	// track) to upgrade to.
	version string
	image   *schematic.Schematic
	meta    bool
}

func (p *updatePlan) changes() bool {
	return p.hardware || p.network || p.upgrade || p.meta || p.autoscale
}

const (
	nodeBackTimeout = 10 * time.Minute
	upgradeTimeout  = 20 * time.Minute
)

// planUpdate checks an update against the machine and says what it
// changes. allowed says which networks the hypervisor lets machines use.
func planUpdate(m *machines.Machine, u machineUpdate, allowed func(string) bool) (*updatePlan, error) {
	cur := m.Spec
	next := cur
	next.NICs = append([]machines.NIC(nil), cur.NICs...)
	p := &updatePlan{version: m.Version, image: cur.Schematic()}

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
	if u.Autoscale.set {
		next.Autoscale = u.Autoscale.policy
		if next.Autoscale != nil {
			if err := checkAutoscale(next); err != nil {
				return nil, err
			}
		}
		p.autoscale = !autoscaleEqual(cur.Autoscale, next.Autoscale)
	} else if next.Autoscale != nil && (next.VCPUs != cur.VCPUs || next.MemoryMiB != cur.MemoryMiB) {
		if err := next.Autoscale.Check(next); err != nil {
			return nil, fmt.Errorf("with its auto-scaling: %w", err)
		}
	}
	// The maxima count: the virtual machine is started with room for
	// them.
	p.hardware = next.Hardware() != cur.Hardware()

	if u.Version != nil || u.Extensions != nil || u.HAProxy != nil || u.Kernel != nil {
		if cur.Image != nil {
			return nil, errors.New("this machine was created from an image URL: its version, extensions, HAProxy branch and kernel track can't be changed, only replaced")
		}
		if u.Version != nil {
			if !versionRe.MatchString(*u.Version) {
				return nil, fmt.Errorf("version %q isn't a Janus release (vYYYY.MM.DD[-N])", *u.Version)
			}
			p.version = *u.Version
			next.Version = *u.Version
		}
		if u.Extensions != nil {
			next.Extensions = append([]string(nil), *u.Extensions...)
		}
		if u.HAProxy != nil {
			next.HAProxy = *u.HAProxy
		}
		if u.Kernel != nil {
			next.Kernel = *u.Kernel
		}
		want := next.Schematic()
		if err := want.Normalize(); err != nil {
			return nil, err
		}
		next.Extensions = want.Customization.Extensions
		p.image = want
		p.upgrade = p.version != m.Version || want.ID() != m.Schematic
	}

	if u.NICs != nil {
		if err := planNICs(p, cur.NICs, *u.NICs, allowed); err != nil {
			return nil, err
		}
		next.NICs = p.next.NICs
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
	p.network = len(p.added) > 0 || len(p.removed) > 0 || !nicsConfigEqual(next.NICs, cur.NICs) ||
		!slices.Equal(next.DNS, cur.DNS) || !slices.Equal(next.NTP, cur.NTP)

	if u.ManagedBy != nil && *u.ManagedBy != cur.ManagedBy {
		next.ManagedBy, p.meta = *u.ManagedBy, true
	}
	if u.Locked != nil && *u.Locked != cur.Locked {
		next.Locked, p.meta = *u.Locked, true
	}
	if u.UpdateRebootMode != nil {
		mode, err := updateRebootMode(*u.UpdateRebootMode)
		if err != nil {
			return nil, err
		}
		if mode != cur.UpdateRebootMode {
			next.UpdateRebootMode, p.meta = mode, true
		}
	}
	p.next = next
	return p, nil
}

// checkAutoscale checks spec's auto-scaling policy.
func checkAutoscale(spec machines.Spec) error {
	return spec.Autoscale.Check(spec)
}

// autoscaleEqual reports whether two policies are the same.
func autoscaleEqual(a, b *machines.Autoscale) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// planNICs works out the new set of interfaces: by MAC, those kept (their
// configuration, maybe their network, may change), added and removed.
func planNICs(p *updatePlan, cur, want []machines.NIC, allowed func(string) bool) error {
	if len(want) == 0 || len(want) > 8 {
		return errors.New("nics: 1 to 8 network interfaces")
	}
	byMAC := map[string]machines.NIC{}
	for _, n := range cur {
		byMAC[strings.ToLower(n.MAC)] = n
	}
	seen := map[string]bool{}
	var out []machines.NIC
	for i, n := range want {
		n.MAC = strings.ToLower(strings.TrimSpace(n.MAC))
		if n.Name == "" {
			n.Name = fmt.Sprintf("eth%d", i)
		}
		if !allowed(n.Network) {
			return fmt.Errorf("network %q isn't one this hypervisor allows", n.Network)
		}
		c, existing := byMAC[n.MAC]
		if n.Mode == "" {
			if existing {
				n.Mode = c.Mode
			} else {
				n.Mode = "static"
			}
		}
		switch {
		case n.MAC == "":
			mac, err := randomMAC()
			if err != nil {
				return err
			}
			n.MAC = mac
			fallthrough
		case !existing:
			if hw, err := net.ParseMAC(n.MAC); err != nil || !macRe.MatchString(n.MAC) || hw[0]&1 == 1 {
				return fmt.Errorf("MAC address %q isn't a unicast Ethernet address", n.MAC)
			}
			if n.Mode == "dhcp" {
				return fmt.Errorf("interface %s: DHCP is only available on the interface the node booted with - give an added interface static addresses, or none", n.Name)
			}
			p.added = append(p.added, n.MAC)
		case n.Network != c.Network:
			p.moved = append(p.moved, n.MAC)
		}
		if seen[n.MAC] {
			return fmt.Errorf("MAC address %s is used twice", n.MAC)
		}
		seen[n.MAC] = true
		out = append(out, n)
	}
	for _, c := range cur {
		if !seen[strings.ToLower(c.MAC)] {
			p.removed = append(p.removed, strings.ToLower(c.MAC))
		}
	}
	if len(p.added) > 0 || len(p.removed) > 0 || len(p.moved) > 0 {
		p.hardware = true
	}
	p.next.NICs = out
	return nil
}

func nicsConfigEqual(a, b []machines.NIC) bool {
	return slices.EqualFunc(a, b, func(x, y machines.NIC) bool {
		return x.Network == y.Network && x.Name == y.Name && strings.EqualFold(x.MAC, y.MAC) && x.Mode == y.Mode &&
			slices.Equal(x.Addresses, y.Addresses) && x.Gateway == y.Gateway
	})
}

// controlMAC is the MAC of the interface the Controller reaches node
// through - which can't be removed or moved: the node would be lost.
func controlMAC(ctx context.Context, node *store.Node) (string, error) {
	host, _, err := net.SplitHostPort(node.Addr())
	if err != nil {
		return "", err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", err
	}
	st, err := nodes.NetworkStatus(ctx, node)
	if err != nil {
		return "", fmt.Errorf("read the node's interfaces: %w", err)
	}
	for _, iface := range st.GetInterfaces() {
		for _, a := range iface.GetAddresses() {
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr() == ip {
				return strings.ToLower(iface.GetMac()), nil
			}
		}
	}
	return "", fmt.Errorf("no interface of the node has %s, the address the Controller reaches it at", ip)
}

// mergeNetwork is the node's configuration cur with what the machine's
// spec manages put in: its physical interfaces (by MAC, except those in
// skip - not plugged in yet), their names, modes, addresses and gateways,
// and the DNS and NTP servers. The interfaces in removed go, with the
// VLANs on them; everything else - VLANs, MTUs, metrics, search domains,
// the hostname - is the node's, kept.
func mergeNetwork(cur *janusv1alpha1.NetworkConfig, spec machines.Spec, removed, skip []string) *janusv1alpha1.NetworkConfig {
	out := &janusv1alpha1.NetworkConfig{Hostname: spec.Name}
	if cur != nil {
		out = proto.Clone(cur).(*janusv1alpha1.NetworkConfig)
	}
	gone := map[string]bool{}
	renamed := map[string]string{}
	var kept []*janusv1alpha1.NetworkInterface
	for _, iface := range out.GetInterfaces() {
		if mac := strings.ToLower(iface.GetMac()); mac != "" && slices.Contains(removed, mac) {
			gone[iface.GetName()] = true
			continue
		}
		kept = append(kept, iface)
	}
	var final []*janusv1alpha1.NetworkInterface
	for _, iface := range kept {
		if v := iface.GetVlan(); v != nil && gone[v.GetParent()] {
			continue
		}
		final = append(final, iface)
	}
	for _, n := range spec.NICs {
		mac := strings.ToLower(n.MAC)
		if slices.Contains(skip, mac) {
			continue
		}
		var target *janusv1alpha1.NetworkInterface
		for _, iface := range final {
			if strings.EqualFold(iface.GetMac(), mac) {
				target = iface
				break
			}
		}
		if target == nil {
			target = &janusv1alpha1.NetworkInterface{Mac: mac}
			final = append(final, target)
		} else if target.GetName() != n.Name {
			renamed[target.GetName()] = n.Name
		}
		target.Name = n.Name
		target.Mode = addressingMode(n.Mode)
		target.Addresses = append([]string(nil), n.Addresses...)
		target.Gateway = n.Gateway
	}
	// A VLAN follows its parent's new name.
	for _, iface := range final {
		if v := iface.GetVlan(); v != nil {
			if to, ok := renamed[v.GetParent()]; ok {
				v.Parent = to
			}
		}
	}
	out.Interfaces = final
	if len(spec.DNS) > 0 || out.GetDns() != nil {
		if out.Dns == nil {
			out.Dns = &janusv1alpha1.NetworkDNS{}
		}
		out.Dns.Servers = append([]string(nil), spec.DNS...)
	}
	if len(spec.NTP) > 0 || out.GetNtp() != nil {
		if out.Ntp == nil {
			out.Ntp = &janusv1alpha1.NetworkNTP{}
		}
		out.Ntp.Servers = append([]string(nil), spec.NTP...)
	}
	return out
}

func addressingMode(mode string) janusv1alpha1.AddressingMode {
	switch mode {
	case "static":
		return janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC
	case "none":
		return janusv1alpha1.AddressingMode_ADDRESSING_MODE_NONE
	case "disabled":
		return janusv1alpha1.AddressingMode_ADDRESSING_MODE_DISABLED
	}
	return janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP
}

func modeName(m janusv1alpha1.AddressingMode) string {
	switch m {
	case janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC:
		return "static"
	case janusv1alpha1.AddressingMode_ADDRESSING_MODE_NONE:
		return "none"
	case janusv1alpha1.AddressingMode_ADDRESSING_MODE_DISABLED:
		return "disabled"
	}
	return "dhcp"
}

// authKind says how a request was authenticated (requireAuth).
type authKind int

const (
	authSession authKind = iota
	authToken
)

type authKindKey struct{}

// fromPage reports a request made from the Controller's pages - the
// admin's session - rather than a program's API token.
func fromPage(r *http.Request) bool {
	k, _ := r.Context().Value(authKindKey{}).(authKind)
	return k == authSession
}

// lockedFromPage answers 423 when the machine is locked and the request
// comes from the Controller's pages.
func lockedFromPage(w http.ResponseWriter, r *http.Request, m *machines.Machine) bool {
	if !m.Spec.Locked || !fromPage(r) {
		return false
	}
	by := m.Spec.ManagedBy
	if by == "" {
		by = "code"
	}
	writeError(w, http.StatusLocked, fmt.Sprintf("%s is managed by %s and locked: a change made here would be undone by its next run - change it there, or release it first", m.Spec.Name, by))
	return true
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
	if !u.onlyRelease() && lockedFromPage(w, r, m) {
		return
	}
	h, ok := a.hypervisors.Get(m.Spec.HypervisorID)
	if !ok {
		writeError(w, http.StatusConflict, "its hypervisor is gone")
		return
	}
	p, err := planUpdate(m, u, h.AllowsNetwork)
	if err == nil && p.hardware {
		err = checkHardware(h, p.next.Hardware())
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !p.changes() {
		writeJSON(w, http.StatusOK, a.machineView(m))
		return
	}
	// Who manages it, the lock, how its updates reboot, auto-scaling's
	// mode and thresholds: just the record.
	if !p.hardware && !p.network && !p.upgrade {
		m, err = a.machines.Update(m.ID, func(m *machines.Machine) error {
			m.Spec.ManagedBy, m.Spec.Locked = p.next.ManagedBy, p.next.Locked
			if p.autoscale {
				logAutoscaleChange(m, p.next.Autoscale)
			}
			switch {
			case !m.Spec.Locked && u.Locked != nil:
				m.Log("released: its pages may change it again")
			case m.Spec.Locked && u.Locked != nil:
				m.Log("locked: managed by %s", m.Spec.ManagedBy)
			}
			if p.next.UpdateRebootMode != m.Spec.UpdateRebootMode {
				m.Spec.UpdateRebootMode = p.next.UpdateRebootMode
				m.Log("its updates reboot %s", rebootWay(m.Spec.UpdateRebootMode))
			}
			return nil
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, a.machineView(m))
		return
	}

	if m.Phase != machines.PhaseReady || m.NodeID == "" || m.Ref == nil || m.Ref.UUID == "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("the machine is %s: only a ready machine with its node can be changed", m.Phase))
		return
	}
	node, ok := a.store.Get(m.NodeID)
	if !ok {
		writeError(w, http.StatusConflict, "the machine's node is gone")
		return
	}
	if len(p.removed) > 0 || len(p.moved) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		mac, err := controlMAC(ctx, node)
		cancel()
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if slices.Contains(p.removed, mac) || slices.Contains(p.moved, mac) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("the Controller reaches the node through the interface %s (%s): it can't be removed or moved to another network", mac, node.Addr()))
			return
		}
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
// its node is there - with the error if a step failed; it's read again
// from the node and the hypervisor afterwards, so its record says what
// was really done.
func (r *machineRunner) update(ctx context.Context, id string, p *updatePlan) error {
	err := r.updateSteps(ctx, id, p)
	_, uerr := r.a.machines.Update(id, func(m *machines.Machine) error {
		m.Phase = machines.PhaseReady
		m.Spec.ManagedBy, m.Spec.Locked = p.next.ManagedBy, p.next.Locked
		if p.next.UpdateRebootMode != m.Spec.UpdateRebootMode {
			m.Spec.UpdateRebootMode = p.next.UpdateRebootMode
			m.Log("its updates reboot %s", rebootWay(m.Spec.UpdateRebootMode))
		}
		if err != nil {
			m.Error = "update failed: " + err.Error()
			m.Log("%s", m.Error)
		} else {
			m.Error = ""
			m.Spec.VCPUs, m.Spec.MemoryMiB, m.Spec.NICs, m.Spec.DNS, m.Spec.NTP = p.next.VCPUs, p.next.MemoryMiB, p.next.NICs, p.next.DNS, p.next.NTP
			m.Spec.Extensions, m.Spec.HAProxy, m.Spec.Kernel = p.next.Extensions, p.next.HAProxy, p.next.Kernel
			if p.autoscale {
				logAutoscaleChange(m, p.next.Autoscale)
			}
			if p.hardware {
				// Started again at its nominal size: auto-scaling starts
				// over - what followed the memory went back before the
				// restart (updateSteps), or goes back now.
				m.AutoscaleState = keptSysctl(m.AutoscaleState)
			}
			m.Log("updated")
		}
		return nil
	})
	if uerr != nil {
		return uerr
	}
	// Read back what was really done - all of it, or what a failed
	// update got through.
	if m, ok := r.a.machines.Get(id); ok {
		r.a.hvStatus.forget(m.Spec.HypervisorID)
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = r.syncAs(sctx, id, "read back from the node after the update")
	return nil
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

	applyNetwork := func(skip []string, what string) error {
		r.logEvent(id, "%s", what)
		nctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		cur, err := nodes.Network(nctx, node)
		if err != nil {
			return fmt.Errorf("read the node's network configuration: %w", err)
		}
		if err := nodes.ApplyNetwork(nctx, node, a.store, mergeNetwork(cur, p.next, p.removed, skip)); err != nil {
			return fmt.Errorf("network: %w", err)
		}
		r.logEvent(id, "network configuration confirmed - the node is at %s", node.Addr())
		return nil
	}

	// Interfaces leave the configuration while they're still there: a
	// node whose configuration names an interface it doesn't have keeps
	// none of it.
	networkDone := false
	if p.network && len(p.removed) > 0 {
		if err := applyNetwork(p.added, "applying the network configuration without the removed interfaces, on trial"); err != nil {
			return err
		}
		networkDone = len(p.added) == 0
	}

	if p.hardware {
		hw := p.next.Hardware()
		room := ""
		if hw.Scalable() {
			room = fmt.Sprintf(" (room for %d vCPU, %d MiB)", max(hw.MaxVCPUs, hw.VCPUs), max(hw.MaxMemoryMiB, hw.MemoryMiB))
		}
		// It restarts at its size: what auto-scaling made follow the
		// memory goes back first, while the node answers - a policy
		// removed has nobody left to do it.
		if s := r.a.autoscaler; s != nil && memoryFollowed(m.AutoscaleState) {
			if msg := s.putBack(ctx, m, node, nil, m.AutoscaleState, time.Now()); msg != "" {
				r.logEvent(id, "auto-scaling: %s", msg)
			}
		}
		r.logEvent(id, "shutting the node down to change its hardware: %d vCPU, %d MiB%s, %d interfaces", p.next.VCPUs, p.next.MemoryMiB, room, len(p.next.NICs))
		if err := stopMachine(ctx, drv, node, *m.Ref); err != nil {
			return err
		}
		var nics []hypervisor.NIC
		for _, n := range p.next.NICs {
			nics = append(nics, hypervisor.NIC{Network: n.Network, MAC: n.MAC})
		}
		if err := drv.Reconfigure(ctx, *m.Ref, hw, nics); err != nil {
			_ = drv.Power(ctx, *m.Ref, hypervisor.PowerStart)
			return fmt.Errorf("reconfigure: %w", err)
		}
		if err := drv.Power(ctx, *m.Ref, hypervisor.PowerStart); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		r.logEvent(id, "reconfigured - waiting for the node")
		if _, err := waitNode(ctx, node, nodeBackTimeout, nil); err != nil {
			return err
		}
	}

	if p.network && !networkDone {
		if err := applyNetwork(nil, "applying the network configuration on trial"); err != nil {
			return err
		}
	}

	if p.upgrade {
		b, err := r.resolveBundle(ctx, id, p.version, p.image)
		if err != nil {
			return err
		}
		r.logEvent(id, "updating the node to Janus %s, schematic %.8s", b.Version, b.Schematic)
		uctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
		defer cancel()
		src := &janusv1alpha1.ImageSource{Reference: b.BaseURL, Sha256: b.SHA256, AllowSchematicChange: b.Schematic != m.Schematic}
		mode := janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT
		if p.next.UpdateRebootMode == "kexec" {
			mode = janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC
		}
		how, err := nodes.Upgrade(uctx, node, src, mode)
		if err != nil {
			return fmt.Errorf("update: %w", err)
		}
		// The node says how: through kexec, or through the firmware (and
		// why, when kexec was asked).
		if how == "" {
			how = "rebooting"
		}
		r.logEvent(id, "the node is rebooting into Janus %s - %s", b.Version, how)
		if _, err := waitNode(uctx, node, upgradeTimeout, func(i *nodeproxy.NodeInfo) bool { return i.Version == b.Version && i.Schematic == b.Schematic }); err != nil {
			return fmt.Errorf("after the update: %w", err)
		}
		if _, err := a.machines.Update(id, func(m *machines.Machine) error {
			m.Spec.Version = p.next.Version
			m.Log("the node runs Janus %s", b.Version)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// rebootWay says how updates reboot, for a machine's history.
func rebootWay(mode string) string {
	if mode == "kexec" {
		return "through kexec"
	}
	return "through the firmware"
}

// resolveBundle finds the update bundle, waiting while the image
// factory builds it.
func (r *machineRunner) resolveBundle(ctx context.Context, id, version string, sc *schematic.Schematic) (*nodeproxy.Bundle, error) {
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
func waitNode(ctx context.Context, node *store.Node, timeout time.Duration, ok func(*nodeproxy.NodeInfo) bool) (*nodeproxy.NodeInfo, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		info, err := nodes.Info(vctx, node)
		cancel()
		if err == nil && (ok == nil || ok(info)) {
			return info, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("it runs %s (schematic %.8s)", info.Version, info.Schematic)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return nil, fmt.Errorf("the node isn't back after %s: %v", timeout, lastErr)
}

// logAutoscaleChange sets a machine's auto-scaling policy and says so in
// its history - a new mode starts it over.
func logAutoscaleChange(m *machines.Machine, next *machines.Autoscale) {
	prev := m.Spec.Autoscale
	m.Spec.Autoscale = next
	switch {
	case next == nil:
		m.AutoscaleState = nil
		m.Log("auto-scaling removed")
	case prev == nil || prev.Mode != next.Mode:
		// A new mode starts over; what it changed on the node it still
		// puts back.
		m.AutoscaleState = keptSysctl(m.AutoscaleState)
		m.Log("auto-scaling %s: up to %d vCPU, %d MiB", autoscaleModeText(next.Mode), next.MaxVCPUs, next.MaxMemoryMiB)
	default:
		m.Log("auto-scaling's policy changed: up to %d vCPU, %d MiB", next.MaxVCPUs, next.MaxMemoryMiB)
	}
}

func autoscaleModeText(mode string) string {
	switch mode {
	case machines.AutoscaleOn:
		return "on"
	case machines.AutoscaleObserve:
		return "observing (it logs what it would do, nothing more)"
	}
	return "off"
}

// keptSysctl is a fresh auto-scaling state that still holds st's record of
// the kernel parameters it changed - nil when there's none.
func keptSysctl(st *machines.AutoscaleState) *machines.AutoscaleState {
	if rec := sysctlOf(st); rec != nil {
		return &machines.AutoscaleState{Sysctl: rec}
	}
	return nil
}
