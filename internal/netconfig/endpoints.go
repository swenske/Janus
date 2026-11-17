package netconfig

import (
	"net"
	"net/netip"
	"slices"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// A NetworkConfigApply whose configuration takes away the address the
// call came in on can't outlive it: the node queues its messages
// (validating, applying, awaiting-confirmation), applies within
// milliseconds, and from then on the kernel routes nothing from that
// source address - every message still queued is lost, the first ones
// included (controller-libvirt-test's part 4e: none arrived), and the
// stream stays silent until its deadline (30 s, every move of a cluster
// member on v2026.11.17's lab). The client gives it ApplyGrace from the
// call's start, then confirms over a new connection where the node
// answers.

// ApplyGrace is how long a client waits for a NetworkConfigApply that
// drops its own address (DropsEndpoint) before giving its stream up as
// lost with that address: the node validates and applies in well under
// a second.
const ApplyGrace = 5 * time.Second

// DropsEndpoint reports whether cfg takes endpoint's address (host:port,
// or a bare IP) away from the node: cfg gives it to no interface as a
// static address and leaves none to DHCP (which may well give it back).
func DropsEndpoint(cfg *janusv1alpha1.NetworkConfig, endpoint string) bool {
	host := endpoint
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return false
	}
	for _, iface := range cfg.GetInterfaces() {
		if iface.GetMode() == janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP {
			return false
		}
	}
	return len(cfg.GetInterfaces()) > 0 && !KeepsStatically(cfg, endpoint)
}

// ConfirmEndpoints lists where to confirm a node's new configuration cfg
// after NetworkConfigApply over endpoint (host:port): endpoint first -
// valid whenever the change keeps its address -, then each address the
// node reported, or, that report lost with the old address, cfg's static
// ones. lost: the call's connection was lost while applying; then
// endpoint goes last unless cfg keeps its address statically - it most
// likely left the node, and a confirmation tried there first cost its 4 s
// timeout every round.
func ConfirmEndpoints(endpoint string, cfg *janusv1alpha1.NetworkConfig, reported []string, lost bool) []string {
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		port = "9505"
	}
	out := []string{endpoint}
	add := func(cidr string) {
		p, err := netip.ParsePrefix(cidr)
		if err != nil || !p.Addr().IsGlobalUnicast() {
			return
		}
		ep := net.JoinHostPort(p.Addr().String(), port)
		if !slices.Contains(out, ep) {
			out = append(out, ep)
		}
	}
	for _, a := range reported {
		add(a)
	}
	if len(reported) == 0 {
		for _, iface := range cfg.GetInterfaces() {
			for _, a := range iface.GetAddresses() {
				add(a)
			}
		}
	}
	if lost && len(out) > 1 && !KeepsStatically(cfg, endpoint) {
		out = append(out[1:], endpoint)
	}
	return out
}

// KeepsStatically reports whether cfg gives endpoint's host (host:port,
// or a bare IP) to one of its interfaces as a static address.
func KeepsStatically(cfg *janusv1alpha1.NetworkConfig, endpoint string) bool {
	host := endpoint
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, iface := range cfg.GetInterfaces() {
		for _, a := range iface.GetAddresses() {
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Unmap() == ip.Unmap() {
				return true
			}
		}
	}
	return false
}
