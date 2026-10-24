package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
	"github.com/swenske/Janus/internal/bgp"
	"github.com/swenske/Janus/internal/cluster"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/vrrp"
)

// Cluster is ClusterService: the node's membership of a cluster
// (internal/cluster) and the peers section HAProxy runs with.
type Cluster struct {
	janusv1alpha1.UnimplementedClusterServiceServer
	Store *cluster.Store
	// PeersFile is where the generated section goes, for HAProxy to read
	// as a second -f; empty: HAProxy doesn't read one (janusd off a node).
	PeersFile string
	HAProxy   *haproxy.Manager
	// LocalIPs are the node's addresses: a member's own must be one.
	LocalIPs func() []net.IP
	// VRRP and BGP are what a drain waits for: the virtual IPs given up,
	// the haproxy_* protocols down. Nil, or not available: nothing to
	// wait for.
	VRRP *vrrp.Manager
	BGP  *bgp.Manager
	// ACME installs the certificates pulled from a member; ACMECertDir
	// is where this node's are, for ClusterExport.
	ACME        *acme.Manager
	ACMECertDir string
}

// ClusterDrain takes the member out of service: unhealthy to keepalived
// and BIRD until ClusterUndrain, HAProxy serving meanwhile.
func (c *Cluster) ClusterDrain(req *janusv1alpha1.ClusterDrainRequest, stream janusv1alpha1.ClusterService_ClusterDrainServer) error {
	caller, _ := CallerFrom(stream.Context())
	timeout := time.Duration(req.GetTimeoutSeconds()) * time.Second
	if timeout == 0 {
		timeout = time.Minute
	}
	if c.Store.SetDrained(true) {
		log.Printf("cluster: drained - HAProxy reads as unhealthy to keepalived and BIRD until undrained - by %s", caller)
		events.Publish("cluster.drained", map[string]any{"by": caller.String()})
	}
	send := func(stage, msg string, vrrpOK, bgpOK, done bool) error {
		return stream.Send(&janusv1alpha1.ClusterDrainProgress{Stage: stage, Message: msg, VrrpReleased: vrrpOK, BgpWithdrawn: bgpOK, Done: done})
	}
	if err := send("marked", "HAProxy marked unhealthy for keepalived and BIRD", false, false, false); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var lastVRRP, lastBGP bool
	first := true
	for {
		vrrpOK, vrrpMsg := c.vrrpReleased()
		bgpOK, bgpMsg := c.bgpWithdrawn()
		if first || vrrpOK != lastVRRP {
			if err := send("vrrp", vrrpMsg, vrrpOK, bgpOK, false); err != nil {
				return err
			}
		}
		if first || bgpOK != lastBGP {
			if err := send("bgp", bgpMsg, vrrpOK, bgpOK, false); err != nil {
				return err
			}
		}
		first, lastVRRP, lastBGP = false, vrrpOK, bgpOK
		if vrrpOK && bgpOK {
			return send("done", "out of service: the traffic has moved to the peers", true, true, true)
		}
		if time.Now().After(deadline) {
			return send("timeout", "still in service after "+timeout.String()+" - the drain stays in effect", vrrpOK, bgpOK, false)
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// vrrpReleased: every VRRP instance is BACKUP (or FAULT, INIT, STOP) -
// or there is no VRRP to speak of.
func (c *Cluster) vrrpReleased() (bool, string) {
	if c.VRRP == nil || !c.VRRP.Available() {
		return true, "no VRRP on this node"
	}
	instances, err := c.VRRP.Status()
	if err != nil {
		return true, "no VRRP instance running"
	}
	var masters []string
	for _, in := range instances {
		if in.State == "MASTER" {
			masters = append(masters, in.Name)
		}
	}
	if len(masters) > 0 {
		return false, "still MASTER of " + joinLines(masters)
	}
	if len(instances) == 0 {
		return true, "no VRRP instance"
	}
	return true, "every VRRP instance gave its virtual IPs up"
}

// bgpWithdrawn: every haproxy_* protocol is held down - or there is no
// BGP to speak of.
func (c *Cluster) bgpWithdrawn() (bool, string) {
	if c.BGP == nil || !c.BGP.Available() {
		return true, "no BGP on this node"
	}
	st, err := c.BGP.Status()
	if err != nil {
		return true, "BIRD isn't running"
	}
	var up []string
	gated := 0
	for _, p := range st.Protocols {
		if !strings.HasPrefix(p.Name, bgp.GatePrefix) {
			continue
		}
		gated++
		if !p.HeldDown && p.State != "down" {
			up = append(up, p.Name)
		}
	}
	if len(up) > 0 {
		return false, "still announcing through " + joinLines(up)
	}
	if gated == 0 {
		return true, "no haproxy_* protocol"
	}
	return true, "every haproxy_* protocol is down: the routes are withdrawn"
}

// ClusterUndrain puts the member back in service.
func (c *Cluster) ClusterUndrain(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.ClusterInfo, error) {
	caller, _ := CallerFrom(ctx)
	if c.Store.SetDrained(false) {
		log.Printf("cluster: undrained - HAProxy's health is its own again - by %s", caller)
		events.Publish("cluster.undrained", map[string]any{"by": caller.String()})
	}
	return c.info(), nil
}

// ClusterGet is what the node knows of its cluster.
func (c *Cluster) ClusterGet(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.ClusterInfo, error) {
	return c.info(), nil
}

// ClusterSet installs or updates the membership, and HAProxy with it.
func (c *Cluster) ClusterSet(ctx context.Context, req *janusv1alpha1.ClusterSetRequest) (*janusv1alpha1.ClusterInfo, error) {
	caller, _ := CallerFrom(ctx)
	m := cluster.Membership{ID: req.GetId(), Name: req.GetName(), Member: req.GetMemberName(), PeersPort: int(req.GetPeersPort()),
		Managed: req.GetManaged(), ManagedBy: req.GetManagedBy(), Advisory: req.GetAdvisory()}
	for _, mb := range req.GetMembers() {
		m.Members = append(m.Members, cluster.Member{Name: mb.GetName(), Address: mb.GetAddress(), API: mb.GetApiAddress()})
	}
	if self := m.Find(m.Member); self != nil && c.LocalIPs != nil {
		ip := net.ParseIP(self.Address)
		if ip != nil && !slices.ContainsFunc(c.LocalIPs(), ip.Equal) {
			return nil, status.Errorf(codes.InvalidArgument, "member %s's address %s isn't one of this node's", m.Member, self.Address)
		}
	}
	was := c.Store.Snapshot()
	wasMember := c.Store.IsMember()
	section, cert, err := c.Store.Set(cluster.SetRequest{Membership: m, CAPEM: req.GetCaCert(), MemberPEM: req.GetMemberCert()})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := c.apply(section, cert, was); err != nil {
		return nil, err
	}
	what := "cluster.updated"
	if !wasMember {
		what = "cluster.joined"
	}
	info := c.info()
	log.Printf("cluster: %s %s (%s) as %s, %d members - by %s", map[bool]string{true: "updated", false: "joined"}[wasMember], m.Name, m.ID, m.Member, len(m.Members), caller)
	events.Publish(what, map[string]any{"id": m.ID, "name": m.Name, "member": m.Member, "members": len(m.Members), "by": caller.String()})
	return info, nil
}

// ClusterRelease lets domains of this member be changed again by its own
// users: the cluster reports the member as drifted until its next apply.
func (c *Cluster) ClusterRelease(ctx context.Context, req *janusv1alpha1.ClusterReleaseRequest) (*janusv1alpha1.ClusterInfo, error) {
	caller, _ := CallerFrom(ctx)
	released, err := c.Store.Release(req.GetDomains())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if len(released) > 0 {
		log.Printf("cluster: released %s - this node's own users may change them; the cluster reports it as drifted - by %s", strings.Join(released, ", "), caller)
		events.Publish("cluster.released", map[string]any{"domains": released, "by": caller.String()})
	}
	return c.info(), nil
}

// ClusterLeave forgets the membership: HAProxy's peers section names the
// node alone again.
func (c *Cluster) ClusterLeave(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.ClusterInfo, error) {
	caller, _ := CallerFrom(ctx)
	m := c.Store.Membership()
	if m == nil {
		return c.info(), nil
	}
	was := c.Store.Snapshot()
	if err := c.Store.Leave(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := c.apply(true, false, was); err != nil {
		return nil, err
	}
	// The ring was the cluster's: the members that stay must not be able
	// to read or forge this node's tickets any more (they renew theirs
	// too, the orchestrator sees to it).
	ring := "a new TLS ticket key ring"
	if keys, err := c.Store.RenewTicketKeys(); err != nil {
		ring = "its TLS ticket key ring NOT renewed: " + err.Error()
	} else if _, err := c.loadTicketKeys(keys); err != nil {
		ring = "a new TLS ticket key ring, HAProxy didn't take it: " + err.Error()
	}
	log.Printf("cluster: left %s (%s), %s - by %s", m.Name, m.ID, ring, caller)
	events.Publish("cluster.left", map[string]any{"id": m.ID, "name": m.Name, "member": m.Member, "by": caller.String()})
	return c.info(), nil
}

// apply gives HAProxy the new membership: the regenerated peers section
// through a reload, or the member certificate alone at runtime. A
// HAProxy that refuses the section gets the previous membership back.
func (c *Cluster) apply(section, cert bool, was cluster.Snapshot) error {
	if c.PeersFile == "" || c.HAProxy == nil {
		return nil
	}
	changed, err := c.Store.WritePeersFile(c.PeersFile)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	switch {
	case section || changed:
		if err := c.HAProxy.Reload(); err != nil {
			var se *haproxy.StartError
			if !errors.As(err, &se) {
				return status.Errorf(codes.Internal, "reload HAProxy: %v", err)
			}
			if rerr := c.Store.Restore(was); rerr != nil {
				log.Printf("cluster: put the previous membership back: %v", rerr)
			} else if _, werr := c.Store.WritePeersFile(c.PeersFile); werr != nil {
				log.Printf("cluster: put the previous peers section back: %v", werr)
			} else if rerr := c.HAProxy.Reload(); rerr != nil {
				log.Printf("cluster: HAProxy with the previous peers section: %v", rerr)
			}
			return status.Errorf(codes.FailedPrecondition, "HAProxy refused the peers section - the previous membership is back: %s", joinLines(se.Output))
		}
	case cert:
		memberPEM, err := os.ReadFile(filepath.Join(c.Store.Dir, cluster.MemberFile))
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if err := c.HAProxy.ReplaceCertFile(filepath.Join(c.Store.Dir, cluster.MemberFile), memberPEM); err != nil {
			// A process that didn't load it (the file is newer than it):
			// a reload does.
			if rerr := c.HAProxy.Reload(); rerr != nil {
				return status.Errorf(codes.Internal, "the member certificate: %v; reload HAProxy: %v", err, rerr)
			}
		}
	}
	return nil
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		if out != "" {
			out += "; "
		}
		out += l
	}
	return out
}

// ClusterStatus reads HAProxy's view of the peers.
func (c *Cluster) ClusterStatus(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.ClusterStatusResponse, error) {
	resp := &janusv1alpha1.ClusterStatusResponse{Info: c.info(), NowUnix: time.Now().Unix()}
	if c.HAProxy == nil || c.PeersFile == "" {
		return resp, nil
	}
	section, err := c.HAProxy.ShowPeersSection(cluster.Section)
	if err != nil {
		return nil, status.Error(codes.Unavailable, fmt.Sprintf("HAProxy's peers: %v", err))
	}
	resp.TablesLearnt = section.Learnt
	for _, p := range section.Peers {
		pp := &janusv1alpha1.ClusterPeer{Name: p.Name, Address: p.Address, Local: p.Local, Up: p.Up(), LastStatus: p.LastStatus,
			AppState: p.AppState, LearnState: p.LearnState, LastHandshake: p.LastHandshake, NewConnections: p.NewConnections, ProtocolErrors: p.ProtocolErrors}
		for _, t := range p.Tables {
			pp.Tables = append(pp.Tables, &janusv1alpha1.ClusterPeerTable{Name: t.Name, LastAcked: t.LastAcked, LastPushed: t.LastPushed, LastGet: t.LastGet,
				TeachingOrigin: t.TeachingOrigin, LocalUpdate: t.TableLocalUpdate, Update: t.TableUpdate, Synced: !p.Local && t.Synced()})
		}
		resp.Peers = append(resp.Peers, pp)
	}
	return resp, nil
}

// info is the membership as ClusterInfo.
func (c *Cluster) info() *janusv1alpha1.ClusterInfo {
	info := &janusv1alpha1.ClusterInfo{MemberName: c.Store.LocalPeer(), PeersPort: uint32(c.Store.BindPort()), PeersConfig: string(c.Store.PeersConfig()), TlsTicketKeys: c.ticketKeysState()}
	// A node removed from its cluster stays drained, out of service on
	// its own: it says so, member or not.
	info.Drained = c.Store.Drained()
	m := c.Store.Membership()
	if m == nil {
		info.Members = []*janusv1alpha1.ClusterMember{{Name: info.MemberName}}
		return info
	}
	info.Member, info.Id, info.Name, info.Managed, info.ManagedBy, info.UpdatedUnix = true, m.ID, m.Name, m.Managed, m.ManagedBy, m.Updated.Unix()
	info.Advisory = m.Advisory
	info.CaCert = c.Store.CAPEM()
	if cert := c.Store.MemberCertificate(); cert != nil {
		info.MemberCertNotAfterUnix = cert.NotAfter.Unix()
	}
	for _, mb := range m.Members {
		info.Members = append(info.Members, &janusv1alpha1.ClusterMember{Name: mb.Name, Address: mb.Address, ApiAddress: mb.API})
	}
	return info
}
