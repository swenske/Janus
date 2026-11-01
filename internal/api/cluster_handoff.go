package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/cluster"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/firewall"
	"github.com/swenske/Janus/internal/handoff"
	"github.com/swenske/Janus/internal/sockdiag"
)

// The connection handoff (internal/handoff, docs/guide/clusters.md
// "Handing connections over"): ClusterFlows lists this member's
// connections for a peer, ClusterHandoff has this member copy a peer's.

// flowStates are the TCP states whose packets a handoff copies: a
// connection that carries data or is closing - not one in TIME_WAIT,
// whose last ACK the client doesn't wait for.
var flowStates = sockdiag.States(sockdiag.Established, sockdiag.SynRecv, sockdiag.FinWait1, sockdiag.FinWait2, sockdiag.CloseWait, sockdiag.LastAck, sockdiag.Closing)

// flowsChunk is how many connections a ClusterFlowsChunk carries at
// most: 64 KiB of IPv4 ones.
const flowsChunk = 5000

// parseAddrs reads a request's addresses: at least one.
func parseAddrs(in []string) ([]netip.Addr, error) {
	if len(in) == 0 {
		return nil, errors.New("no address")
	}
	var out []netip.Addr
	for _, s := range in {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%q isn't an IP address", s)
		}
		if a = a.Unmap(); !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

// ClusterFlows streams this member's open connections on the addresses
// asked for, and where its peers copy their packets to.
func (c *Cluster) ClusterFlows(req *janusv1alpha1.ClusterFlowsRequest, stream janusv1alpha1.ClusterService_ClusterFlowsServer) error {
	addrs, err := parseAddrs(req.GetAddresses())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	flows, err := localFlows(addrs)
	if err != nil {
		return status.Errorf(codes.Internal, "listing the connections: %v", err)
	}
	first := &janusv1alpha1.ClusterFlowsChunk{}
	if r4, r6 := c.reach(addrs); r4.IsValid() || r6.IsValid() {
		if r4.IsValid() {
			first.Reach4 = r4.String()
		}
		if r6.IsValid() {
			first.Reach6 = r6.String()
		}
	}
	chunk := first
	for len(flows) > 0 || chunk == first {
		n := min(len(flows), flowsChunk)
		chunk.Flows4, chunk.Flows6 = handoff.Pack(flows[:n])
		flows = flows[n:]
		if err := stream.Send(chunk); err != nil {
			return err
		}
		chunk = &janusv1alpha1.ClusterFlowsChunk{}
	}
	return nil
}

// localFlows is this node's open TCP connections on addrs.
func localFlows(addrs []netip.Addr) ([]handoff.Flow, error) {
	var out []handoff.Flow
	for _, fam := range []uint8{unix.AF_INET, unix.AF_INET6} {
		err := sockdiag.TCP(fam, flowStates, func(s sockdiag.Socket) {
			if slices.Contains(addrs, s.Src.Addr().Unmap()) {
				out = append(out, handoff.Flow{Remote: netip.AddrPortFrom(s.Dst.Addr().Unmap(), s.Dst.Port()), Local: netip.AddrPortFrom(s.Src.Addr().Unmap(), s.Src.Port())})
			}
		})
		if err != nil {
			if fam == unix.AF_INET6 && (errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EAFNOSUPPORT)) {
				continue // no IPv6 in this kernel
			}
			return nil, err
		}
	}
	return out, nil
}

// reach is this member's own addresses its peers copy packets to, one
// per family, on the interface that holds the shared addresses: its
// member address when it's there, else an address no other member holds
// (not one of the shared ones, nor a host route's - a virtual IP's), an
// IPv6 link-local one last.
func (c *Cluster) reach(shared []netip.Addr) (r4, r6 netip.Addr) {
	var self netip.Addr
	if m := c.Store.Membership(); m != nil {
		if me := m.Find(m.Member); me != nil {
			self, _ = netip.ParseAddr(me.Address)
			self = self.Unmap()
		}
	}
	links, err := netlink.LinkList()
	if err != nil {
		return
	}
	for _, l := range links {
		addrs, err := netlink.AddrList(l, netlink.FAMILY_ALL)
		if err != nil {
			continue
		}
		holds := false
		for _, a := range addrs {
			if ip, ok := netip.AddrFromSlice(a.IP); ok && slices.Contains(shared, ip.Unmap()) {
				holds = true
			}
		}
		if !holds {
			continue
		}
		var ll6 netip.Addr
		for _, a := range addrs {
			ip, ok := netip.AddrFromSlice(a.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			ones, bits := a.Mask.Size()
			own := !slices.Contains(shared, ip) && ones < bits && a.Flags&unix.IFA_F_DEPRECATED == 0 && a.Flags&unix.IFA_F_TENTATIVE == 0
			switch {
			case ip.Is4() && ip == self:
				r4 = ip
			case ip.Is4() && own && !r4.IsValid():
				r4 = ip
			case ip.Is6() && ip.IsLinkLocalUnicast():
				ll6 = ip // the peer names its own interface (dup's device)
			case ip.Is6() && ip == self:
				r6 = ip
			case ip.Is6() && own && ip.IsGlobalUnicast() && !r6.IsValid():
				r6 = ip
			}
		}
		if !r6.IsValid() {
			r6 = ll6
		}
		return r4, r6
	}
	return
}

// dialMember opens a connection to member to, over mTLS with this node's
// member certificate - the cluster's CA vouching for both ends.
func (c *Cluster) dialMember(to *cluster.Member) (*grpc.ClientConn, error) {
	cert := c.Store.MemberTLSCertificate()
	if cert == nil {
		return nil, errors.New("this node has no member certificate")
	}
	addr := to.API
	if addr == "" {
		addr = net.JoinHostPort(to.Address, "9505")
	}
	pool := x509.NewCertPool()
	for _, ca := range c.Store.CAs() {
		pool.AddCert(ca)
	}
	// The member presents its member certificate for its own name.
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{*cert}, RootCAs: pool, ServerName: to.Name, MinVersion: tls.VersionTLS13})
	return grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
}

// readFlows reads member owner's connections on addrs (its
// ClusterFlows): Manager.Read.
func (c *Cluster) readFlows(ctx context.Context, owner string, addrs []netip.Addr) (handoff.Reading, error) {
	m := c.Store.Membership()
	if m == nil {
		return handoff.Reading{}, errors.New("this node is in no cluster")
	}
	to := m.Find(owner)
	if to == nil {
		return handoff.Reading{}, fmt.Errorf("%s is no member of the cluster any more", owner)
	}
	conn, err := c.dialMember(to)
	if err != nil {
		return handoff.Reading{}, err
	}
	defer conn.Close()
	req := &janusv1alpha1.ClusterFlowsRequest{}
	for _, a := range addrs {
		req.Addresses = append(req.Addresses, a.String())
	}
	stream, err := janusv1alpha1.NewClusterServiceClient(conn).ClusterFlows(ctx, req)
	if err != nil {
		return handoff.Reading{}, errors.New(status.Convert(err).Message())
	}
	var r handoff.Reading
	for first := true; ; first = false {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return handoff.Reading{}, errors.New(status.Convert(err).Message())
		}
		if first {
			r.Reach4, _ = netip.ParseAddr(chunk.GetReach4())
			r.Reach6, _ = netip.ParseAddr(chunk.GetReach6())
			r.Reach4, r.Reach6 = r.Reach4.WithZone(""), r.Reach6.WithZone("")
		}
		for _, fam := range []struct {
			b  []byte
			v6 bool
		}{{chunk.GetFlows4(), false}, {chunk.GetFlows6(), true}} {
			flows, err := handoff.Unpack(fam.b, fam.v6)
			if err != nil {
				return handoff.Reading{}, fmt.Errorf("%s's connections: %v", owner, err)
			}
			r.Flows = append(r.Flows, flows...)
		}
	}
	// Only what it says is on the addresses asked for: a peer copies
	// nothing else.
	r.Flows = slices.DeleteFunc(r.Flows, func(f handoff.Flow) bool { return !slices.Contains(addrs, f.Local.Addr()) })
	return r, nil
}

// NewHandoff is the node's handoff manager for c - nftables through the
// firewall's lock, the reserved tables kept by its ruleset.
func NewHandoff(c *Cluster) *handoff.Manager {
	m := &handoff.Manager{
		Read: c.readFlows,
		Device: func(a netip.Addr) (string, error) {
			routes, err := netlink.RouteGet(net.IP(a.AsSlice()))
			if err != nil {
				return "", err
			}
			if len(routes) == 0 {
				return "", errors.New("no route")
			}
			l, err := netlink.LinkByIndex(routes[0].LinkIndex)
			if err != nil {
				return "", err
			}
			return l.Attrs().Name, nil
		},
		Apply: firewall.ApplyReserved,
		Counters: func() (map[string]uint64, error) {
			v4, err := firewall.ListReserved("ip", handoff.Table)
			if err != nil {
				return nil, err
			}
			v6, err := firewall.ListReserved("ip6", handoff.Table)
			if err != nil {
				return nil, err
			}
			var outs [][]byte
			for _, b := range [][]byte{v4, v6} {
				if b != nil {
					outs = append(outs, b)
				}
			}
			return handoff.ParseCounters(outs...)
		},
	}
	firewall.Reserved = m.Document
	// A janusd that restarted finds the tables its previous run left:
	// it hands nothing over yet.
	if firewall.Available() {
		if err := firewall.ApplyReserved(handoff.Document(nil, handoff.Guards{})); err != nil {
			log.Printf("cluster: clearing the handoff's tables: %v", err)
		}
	}
	return m
}

var (
	handoffProbe     sync.Once
	handoffProbeFail string
)

// handoffUnavailable is why this node can't hand connections over; "":
// it can.
func (c *Cluster) handoffUnavailable() string {
	switch {
	case c.Handoff == nil:
		return "this janusd doesn't manage its host"
	case !firewall.Available():
		return "the nftables extension isn't in this node's image"
	}
	handoffProbe.Do(func() {
		if err := firewall.CheckReserved(handoff.ProbeDocument); err != nil {
			handoffProbeFail = "this node's kernel can't copy packets (nft's dup statement): update the node to a release that can - " + err.Error()
		}
	})
	return handoffProbeFail
}

// ClusterHandoff has this member copy another member's connections to
// it, or stop doing so.
func (c *Cluster) ClusterHandoff(ctx context.Context, req *janusv1alpha1.ClusterHandoffRequest) (*janusv1alpha1.ClusterHandoffResponse, error) {
	caller, _ := CallerFrom(ctx)
	m := c.Store.Membership()
	if m == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node is in no cluster")
	}
	var owner *cluster.Member
	if req.GetOwner() != "" {
		if owner = m.Find(req.GetOwner()); owner == nil || owner.Name == m.Member {
			return nil, status.Errorf(codes.InvalidArgument, "%q isn't another member of the cluster", req.GetOwner())
		}
	} else if !req.GetStop() && req.GetHoldSeconds() == 0 && req.GetRefuseSeconds() == 0 {
		return nil, status.Error(codes.InvalidArgument, "an owner, a hold or a refusal")
	}
	if req.GetStop() {
		if c.Handoff != nil {
			var err error
			if owner != nil {
				err = c.Handoff.Stop(owner.Name)
			} else {
				err = c.Handoff.EndGuards()
			}
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}
		if owner != nil {
			log.Printf("cluster: handoff to %s ended - by %s", owner.Name, caller)
			return &janusv1alpha1.ClusterHandoffResponse{State: &janusv1alpha1.ClusterHandoffState{Owner: owner.Name}}, nil
		}
		return &janusv1alpha1.ClusterHandoffResponse{}, nil
	}
	if why := c.handoffUnavailable(); why != "" {
		return nil, status.Error(codes.FailedPrecondition, why)
	}
	addrs, err := parseAddrs(req.GetAddresses())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if c.LocalIPs != nil {
		local := c.LocalIPs()
		for _, a := range addrs {
			if !slices.ContainsFunc(local, func(ip net.IP) bool { l, ok := netip.AddrFromSlice(ip); return ok && l.Unmap() == a }) {
				return nil, status.Errorf(codes.InvalidArgument, "%s isn't one of this node's addresses: a handoff is for the addresses every member holds", a)
			}
		}
	}
	resp := &janusv1alpha1.ClusterHandoffResponse{}
	for _, g := range []struct {
		refuse  bool
		seconds uint32
		what    string
	}{{true, req.GetRefuseSeconds(), "refusing new connections"}, {false, req.GetHoldSeconds(), "holding the packets of connections it has no trace of"}} {
		if g.seconds == 0 {
			continue
		}
		if g.seconds > uint32(handoff.MaxGuard.Seconds()) {
			return nil, status.Errorf(codes.InvalidArgument, "hold_seconds, refuse_seconds: at most %d", int(handoff.MaxGuard.Seconds()))
		}
		until, err := c.Handoff.Guard(g.refuse, addrs, time.Duration(g.seconds)*time.Second)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		log.Printf("cluster: %s on %s until %s - by %s", g.what, addrList(addrs), until.UTC().Format(time.RFC3339), caller)
	}
	refuse, hold := c.Handoff.GuardsUntil()
	if !refuse.IsZero() {
		resp.RefuseUntilUnix = refuse.Unix()
	}
	if !hold.IsZero() {
		resp.HoldUntilUnix = hold.Unix()
	}
	if owner == nil {
		return resp, nil
	}
	d := time.Duration(req.GetSeconds()) * time.Second
	if d <= 0 || d > handoff.MaxDuration {
		return nil, status.Errorf(codes.InvalidArgument, "seconds: 1 to %d", int(handoff.MaxDuration.Seconds()))
	}
	was := slices.ContainsFunc(c.Handoff.List(), func(i handoff.Info) bool { return i.Owner == owner.Name })
	info, err := c.Handoff.Start(ctx, owner.Name, addrs, d)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if !was {
		log.Printf("cluster: handing %s's connections over (%d now) until %s - by %s", owner.Name, info.Flows, info.Until.UTC().Format(time.RFC3339), caller)
		events.Publish("cluster.handoff", map[string]any{"owner": owner.Name, "flows": info.Flows, "until": info.Until.UTC().Format(time.RFC3339), "by": caller.String()})
	}
	resp.State = handoffState(info)
	return resp, nil
}

func addrList(addrs []netip.Addr) string {
	var s []string
	for _, a := range addrs {
		s = append(s, a.String())
	}
	return strings.Join(s, ", ")
}

func handoffState(i handoff.Info) *janusv1alpha1.ClusterHandoffState {
	s := &janusv1alpha1.ClusterHandoffState{Owner: i.Owner, Flows: uint32(min(i.Flows, 1<<31)), UntilUnix: i.Until.Unix(), Error: i.Error, Packets: i.Packets}
	if !i.Read.IsZero() {
		s.ReadUnix = i.Read.Unix()
	}
	return s
}
