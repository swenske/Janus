package netmgr

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
)

// Link kinds, as NetworkInterfaceStatus.kind reports them.
const (
	kindPhysical = "physical"
	kindVLAN     = "vlan"
	kindLoopback = "loopback"
	kindOther    = "other"
)

// linkInfo is what planning needs to know about an existing link.
type linkInfo struct {
	name   string
	index  int
	mac    string
	kind   string
	parent int // VLAN parent's index
	vlanID int
	up     bool
	mtu    int
}

// ifPlan is one interface's desired state.
type ifPlan struct {
	name       string
	mode       janusv1alpha1.AddressingMode
	existing   *linkInfo // nil: a VLAN to create
	vlanParent string
	vlanID     int
	addrs      []netip.Prefix // managed addresses: static, or the boot lease's
	gw4, gw6   netip.Addr
	metric     int
	mtu        int
}

type rename struct{ from, to string }

// plan is what applying a configuration takes, computed from the links
// that exist - pure, so it's tested without a kernel.
type plan struct {
	renames     []rename
	deleteVLANs []linkInfo
	down        []linkInfo
	ifaces      []ifPlan // physical interfaces first, then VLANs
	// dhcp is the interface holding the boot lease, when a DHCP-mode
	// interface is in effect: DNS, domain, NTP and hostname from the
	// lease apply only then.
	dhcp string
}

// autoMetric is the default-route metric of the interface at position i.
func autoMetric(i int) int { return 1024 + i }

func buildPlan(cfg *janusv1alpha1.NetworkConfig, links []linkInfo, lease *BootLease) (*plan, error) {
	if err := netconfig.Validate(cfg); err != nil {
		return nil, err
	}
	byName := map[string]*linkInfo{}
	byMAC := map[string]*linkInfo{}
	byIndex := map[int]*linkInfo{}
	var physical []*linkInfo
	for i := range links {
		l := &links[i]
		byName[l.name] = l
		byIndex[l.index] = l
		if l.kind == kindPhysical {
			physical = append(physical, l)
			if l.mac != "" {
				byMAC[l.mac] = l
			}
		}
	}
	sort.Slice(physical, func(i, j int) bool { return physical[i].index < physical[j].index })

	p := &plan{}
	var leaseLink *linkInfo
	if lease != nil {
		leaseLink = byMAC[lease.MAC]
		if leaseLink == nil {
			leaseLink = byName[lease.Interface]
		}
	}

	// Nothing configured: the kernel's boot-time DHCP result stays, and
	// nothing else is touched but VLANs (none exist unless a trial made
	// them).
	if len(cfg.GetInterfaces()) == 0 {
		for _, l := range links {
			if l.kind == kindVLAN {
				p.deleteVLANs = append(p.deleteVLANs, l)
			}
		}
		if leaseLink != nil {
			ip, err := leasePlan(lease, leaseLink, leaseLink.name, autoMetric(0), 0)
			if err != nil {
				return nil, err
			}
			p.ifaces = append(p.ifaces, ip)
			p.dhcp = leaseLink.name
		}
		return p, nil
	}

	// Physical interfaces, matched by MAC or name.
	final := map[string]*linkInfo{} // final name -> link
	claimed := map[int]string{}     // link index -> final name
	for i, ifc := range cfg.GetInterfaces() {
		if ifc.GetVlan() != nil {
			continue
		}
		var l *linkInfo
		if m := ifc.GetMac(); m != "" {
			l = byMAC[normMAC(m)]
			if l == nil {
				return nil, fmt.Errorf("interface %s: no physical interface has MAC %s (%s)", ifc.GetName(), m, describe(physical))
			}
		} else {
			l = byName[ifc.GetName()]
			if l == nil || l.kind != kindPhysical {
				return nil, fmt.Errorf("interface %s: no such physical interface (%s)", ifc.GetName(), describe(physical))
			}
		}
		if other, ok := claimed[l.index]; ok {
			return nil, fmt.Errorf("interface %s: %s is already configured as %s", ifc.GetName(), l.name, other)
		}
		claimed[l.index] = ifc.GetName()
		final[ifc.GetName()] = l
		if l.name != ifc.GetName() {
			p.renames = append(p.renames, rename{from: l.name, to: ifc.GetName()})
		}

		metric := int(ifc.GetRouteMetric())
		if metric == 0 {
			metric = autoMetric(i)
		}
		switch ifc.GetMode() {
		case janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP:
			if lease == nil {
				return nil, fmt.Errorf("interface %s: DHCP mode needs the lease the kernel obtains at boot, and none was obtained this boot - configure it statically", ifc.GetName())
			}
			if leaseLink == nil || leaseLink.index != l.index {
				return nil, fmt.Errorf("interface %s: DHCP is only available on the interface the kernel configured at boot (%s, %s) - configure %s statically", ifc.GetName(), lease.Interface, lease.MAC, ifc.GetName())
			}
			ip, err := leasePlan(lease, l, ifc.GetName(), metric, int(ifc.GetMtu()))
			if err != nil {
				return nil, err
			}
			ip.existing = l
			p.ifaces = append(p.ifaces, ip)
			p.dhcp = ifc.GetName()
		default:
			ip, err := specPlan(ifc, metric)
			if err != nil {
				return nil, err
			}
			ip.existing = l
			p.ifaces = append(p.ifaces, ip)
		}
	}
	// A rename's target name may be held only by a link that's renamed
	// away too (renames go through temporary names), or by a VLAN (VLANs
	// are deleted before renaming).
	for _, r := range p.renames {
		l := byName[r.to]
		if l == nil || l.kind == kindVLAN {
			continue
		}
		if _, renamedAway := claimed[l.index]; renamedAway {
			continue
		}
		return nil, fmt.Errorf("interface %s: the name is taken by %s, which the configuration doesn't list", r.to, l.name)
	}

	// VLANs.
	wantVLAN := map[string]bool{}
	parentsUp := map[int]bool{}
	for i, ifc := range cfg.GetInterfaces() {
		v := ifc.GetVlan()
		if v == nil {
			continue
		}
		parent := final[v.GetParent()]
		if parent == nil {
			// Not listed itself: an existing physical interface, kept up
			// without an address to carry the VLAN.
			parent = byName[v.GetParent()]
			if parent == nil || parent.kind != kindPhysical || claimed[parent.index] != "" {
				return nil, fmt.Errorf("interface %s: VLAN parent %s isn't a physical interface of this node (%s)", ifc.GetName(), v.GetParent(), describe(physical))
			}
			parentsUp[parent.index] = true
		}
		metric := int(ifc.GetRouteMetric())
		if metric == 0 {
			metric = autoMetric(i)
		}
		ip, err := specPlan(ifc, metric)
		if err != nil {
			return nil, err
		}
		ip.vlanParent, ip.vlanID = v.GetParent(), int(v.GetId())
		if l := byName[ifc.GetName()]; l != nil {
			switch {
			case l.kind == kindVLAN && l.parent == parent.index && l.vlanID == ip.vlanID:
				ip.existing = l
			case l.kind == kindVLAN:
				p.deleteVLANs = append(p.deleteVLANs, *l) // recreated below
			default:
				return nil, fmt.Errorf("interface %s: the name is taken by a %s interface", ifc.GetName(), l.kind)
			}
		}
		wantVLAN[ifc.GetName()] = true
		p.ifaces = append(p.ifaces, ip)
	}
	for _, l := range links {
		if l.kind == kindVLAN && !wantVLAN[l.name] {
			p.deleteVLANs = append(p.deleteVLANs, l)
		}
	}

	// Parents kept up for their VLANs, and every other unlisted physical
	// interface down.
	for _, l := range physical {
		if _, ok := claimed[l.index]; ok {
			continue
		}
		if parentsUp[l.index] {
			p.ifaces = append([]ifPlan{{name: l.name, mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_NONE, existing: l}}, p.ifaces...)
			continue
		}
		p.down = append(p.down, *l)
	}
	return p, nil
}

func leasePlan(lease *BootLease, l *linkInfo, name string, metric, mtu int) (ifPlan, error) {
	addr, err := netip.ParsePrefix(lease.Address)
	if err != nil {
		return ifPlan{}, fmt.Errorf("boot lease: address %q: %w", lease.Address, err)
	}
	ip := ifPlan{name: name, mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP, existing: l, addrs: []netip.Prefix{addr}, metric: metric, mtu: mtu}
	if lease.Router != "" {
		if ip.gw4, err = netip.ParseAddr(lease.Router); err != nil {
			return ifPlan{}, fmt.Errorf("boot lease: router %q: %w", lease.Router, err)
		}
	}
	return ip, nil
}

func specPlan(ifc *janusv1alpha1.NetworkInterface, metric int) (ifPlan, error) {
	ip := ifPlan{name: ifc.GetName(), mode: ifc.GetMode(), metric: metric, mtu: int(ifc.GetMtu())}
	for _, a := range ifc.GetAddresses() {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			return ifPlan{}, err // Validate already refuses this
		}
		ip.addrs = append(ip.addrs, p)
	}
	if g := ifc.GetGateway(); g != "" {
		ip.gw4, _ = netip.ParseAddr(g)
	}
	if g := ifc.GetGateway6(); g != "" {
		ip.gw6, _ = netip.ParseAddr(g)
	}
	return ip, nil
}

func normMAC(m string) string { return strings.ToLower(m) }

// describe lists the physical interfaces, for an error message that says
// what does exist.
func describe(physical []*linkInfo) string {
	if len(physical) == 0 {
		return "this node has no physical Ethernet interface"
	}
	var parts []string
	for _, l := range physical {
		parts = append(parts, fmt.Sprintf("%s %s", l.name, l.mac))
	}
	return "physical interfaces: " + strings.Join(parts, ", ")
}
