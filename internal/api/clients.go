package api

import (
	"context"
	"crypto/x509"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// Clients keeps the connections to the API open now: whose each is -
// its certificate, and the users a Controller's calls are for -, the
// calls it made and the bytes it carried (SystemService.ClientList).
// Its Listener counts the bytes; ConnStats with it follows the
// connections and their calls.
type Clients struct {
	mu    sync.Mutex
	next  uint64
	conns map[uint64]*client
	// accepted are the connections Listener accepted, by their remote
	// address, until gRPC tags them (TagConn): those whose handshake
	// fails never get there, and leave when closed.
	accepted map[string]*countedConn
}

// ActiveWindow is how long a user a Controller acted for stays listed
// after its last call, with no stream open.
const ActiveWindow = 2 * time.Minute

// maxUsers bounds the users kept for one connection: the latest stay.
const maxUsers = 256

// ServiceCredentialName is the name a Controller gives the service
// credential it gets when it adds a node (dashboard/backend): the
// Controller's way in to a node that doesn't trust its fleet yet.
const ServiceCredentialName = "janus-controller"

func NewClients() *Clients {
	return &Clients{conns: map[uint64]*client{}, accepted: map[string]*countedConn{}}
}

type client struct {
	id            uint64
	opened        time.Time
	local, remote string
	name          string
	roles         []string
	controller    bool
	fleet         bool
	io            *countedConn // nil: not accepted through Listener

	mu      sync.Mutex
	calls   uint64
	streams int
	last    time.Time
	users   map[string]*clientUser
}

type clientUser struct {
	roles   []string
	calls   uint64
	streams int
	last    time.Time
}

// Listener counts the bytes through each connection l accepts.
func (c *Clients) Listener(l net.Listener) net.Listener {
	return countingListener{l, c}
}

type countingListener struct {
	net.Listener
	c *Clients
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	cc := &countedConn{Conn: conn, c: l.c, key: conn.RemoteAddr().String()}
	l.c.mu.Lock()
	l.c.accepted[cc.key] = cc
	l.c.mu.Unlock()
	return cc, nil
}

type countedConn struct {
	net.Conn
	c       *Clients
	key     string
	in, out atomic.Uint64
	once    sync.Once
}

func (cc *countedConn) Read(b []byte) (int, error) {
	n, err := cc.Conn.Read(b)
	cc.in.Add(uint64(n)) //nolint:gosec // G115: a length, never negative
	return n, err
}

func (cc *countedConn) Write(b []byte) (int, error) {
	n, err := cc.Conn.Write(b)
	cc.out.Add(uint64(n)) //nolint:gosec // G115: a length, never negative
	return n, err
}

func (cc *countedConn) Close() error {
	cc.once.Do(func() {
		cc.c.mu.Lock()
		if cc.c.accepted[cc.key] == cc {
			delete(cc.c.accepted, cc.key)
		}
		cc.c.mu.Unlock()
	})
	return cc.Conn.Close()
}

type clientKey struct{}

// tag keeps a connection gRPC opened: its certificate's identity, the
// counters Listener gave it.
func (c *Clients) tag(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	cl := &client{opened: time.Now()}
	if info.LocalAddr != nil {
		cl.local = info.LocalAddr.String()
	}
	if info.RemoteAddr != nil {
		cl.remote = info.RemoteAddr.String()
	}
	if p, ok := peer.FromContext(ctx); ok {
		if tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(tlsInfo.State.PeerCertificates) > 0 {
			leaf := tlsInfo.State.PeerCertificates[0]
			cl.name, cl.roles = leaf.Subject.CommonName, leaf.Subject.Organization
			cl.controller = slices.Contains(cl.roles, pki.RoleController) || cl.name == ServiceCredentialName
			cl.fleet = fleetChain(tlsInfo.State.VerifiedChains)
		}
	}
	c.mu.Lock()
	c.next++
	cl.id = c.next
	if cc := c.accepted[cl.remote]; cc != nil {
		cl.io = cc
		delete(c.accepted, cl.remote)
	}
	c.conns[cl.id] = cl
	c.mu.Unlock()
	return context.WithValue(ctx, clientKey{}, cl)
}

// fleetChain reports whether a verified chain ends at the fleet's root.
func fleetChain(chains [][]*x509.Certificate) bool {
	if len(chains) == 0 || len(chains[0]) == 0 || FleetRoot == nil {
		return false
	}
	root := FleetRoot()
	return root != nil && chains[0][len(chains[0])-1].Equal(root)
}

func (c *Clients) end(ctx context.Context) {
	if cl, ok := ctx.Value(clientKey{}).(*client); ok {
		c.mu.Lock()
		delete(c.conns, cl.id)
		c.mu.Unlock()
	}
}

type rpcKey struct{}

// call is one RPC on a connection, for whom it's made.
type call struct {
	cl     *client
	user   string
	roles  []string
	stream bool
}

func (c *Clients) tagRPC(ctx context.Context) context.Context {
	cl, ok := ctx.Value(clientKey{}).(*client)
	if !ok {
		return ctx
	}
	r := &call{cl: cl}
	if cl.controller {
		md, _ := metadata.FromIncomingContext(ctx)
		if u := md.Get(pki.AsUserKey); len(u) == 1 && userNamePattern.MatchString(u[0]) {
			r.user = u[0]
			if roles := md.Get(pki.AsRolesKey); len(roles) == 1 {
				r.roles = strings.Split(roles[0], ",")
			}
		}
	}
	return context.WithValue(ctx, rpcKey{}, r)
}

func (c *Clients) handleRPC(ctx context.Context, s stats.RPCStats) {
	r, ok := ctx.Value(rpcKey{}).(*call)
	if !ok {
		return
	}
	switch s := s.(type) {
	case *stats.Begin:
		r.stream = s.IsClientStream || s.IsServerStream
		r.cl.began(r, time.Now())
	case *stats.End:
		if r.stream {
			r.cl.ended(r, time.Now())
		}
	}
}

func (cl *client) began(r *call, now time.Time) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.calls++
	cl.last = now
	if r.stream {
		cl.streams++
	}
	if r.user == "" {
		return
	}
	u := cl.users[r.user]
	if u == nil {
		if cl.users == nil {
			cl.users = map[string]*clientUser{}
		}
		if len(cl.users) >= maxUsers {
			cl.prune(now)
		}
		if len(cl.users) >= maxUsers {
			cl.evictOldest()
		}
		u = &clientUser{}
		cl.users[r.user] = u
	}
	u.roles = r.roles
	u.calls++
	u.last = now
	if r.stream {
		u.streams++
	}
}

func (cl *client) ended(r *call, now time.Time) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.streams--
	cl.last = now
	if u := cl.users[r.user]; u != nil {
		u.streams--
		u.last = now
	}
}

// prune forgets the users with no stream open and no call for
// ActiveWindow. cl.mu is held.
func (cl *client) prune(now time.Time) {
	for name, u := range cl.users {
		if u.streams <= 0 && now.Sub(u.last) > ActiveWindow {
			delete(cl.users, name)
		}
	}
}

// evictOldest forgets the user whose last call is the oldest. cl.mu is
// held.
func (cl *client) evictOldest() {
	var oldest string
	for name, u := range cl.users {
		if oldest == "" || u.last.Before(cl.users[oldest].last) {
			oldest = name
		}
	}
	delete(cl.users, oldest)
}

// ClientUserInfo is a user a Controller acted for on a connection.
type ClientUserInfo struct {
	Name     string
	Roles    []string
	Calls    uint64
	Streams  int
	LastCall time.Time
}

// ClientInfo is a connection to the API, as Clients knows it now.
type ClientInfo struct {
	ID                uint64
	Opened            time.Time
	Local, Remote     string
	Name              string
	Roles             []string
	Controller, Fleet bool
	Calls             uint64
	Streams           int
	LastCall          time.Time
	BytesIn, BytesOut uint64
	Users             []ClientUserInfo // the latest first
}

// List is the connections open now, the oldest first.
func (c *Clients) List() []ClientInfo {
	c.mu.Lock()
	conns := make([]*client, 0, len(c.conns))
	for _, cl := range c.conns {
		conns = append(conns, cl)
	}
	c.mu.Unlock()
	sort.Slice(conns, func(i, j int) bool { return conns[i].id < conns[j].id })
	now := time.Now()
	out := make([]ClientInfo, 0, len(conns))
	for _, cl := range conns {
		info := ClientInfo{ID: cl.id, Opened: cl.opened, Local: cl.local, Remote: cl.remote, Name: cl.name, Roles: cl.roles, Controller: cl.controller, Fleet: cl.fleet}
		if cl.io != nil {
			info.BytesIn, info.BytesOut = cl.io.in.Load(), cl.io.out.Load()
		}
		cl.mu.Lock()
		info.Calls, info.Streams, info.LastCall = cl.calls, cl.streams, cl.last
		cl.prune(now)
		for name, u := range cl.users {
			info.Users = append(info.Users, ClientUserInfo{Name: name, Roles: u.roles, Calls: u.calls, Streams: u.streams, LastCall: u.last})
		}
		cl.mu.Unlock()
		sort.Slice(info.Users, func(i, j int) bool {
			a, b := info.Users[i], info.Users[j]
			if !a.LastCall.Equal(b.LastCall) {
				return a.LastCall.After(b.LastCall)
			}
			return a.Name < b.Name
		})
		out = append(out, info)
	}
	return out
}

// ClientList lists the connections to the API: everything for an
// admin; for anyone else, the Controllers' and the caller's own
// connection whole but for the users a Controller acts for (counted,
// the caller's own alone listed), the other clients without their
// names and addresses.
func (s *System) ClientList(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.ClientListResponse, error) {
	if s.Clients == nil {
		return nil, status.Error(codes.Unavailable, "this janusd keeps no list of its clients")
	}
	caller, _ := CallerFrom(ctx)
	admin := slices.Contains(caller.Roles, pki.RoleAdmin)
	var self uint64
	if cl, ok := ctx.Value(clientKey{}).(*client); ok {
		self = cl.id
	}
	resp := &janusv1alpha1.ClientListResponse{}
	for _, c := range s.Clients.List() {
		mine := c.ID == self
		pc := &janusv1alpha1.ClientConnection{
			Id: c.ID, Controller: c.Controller, Fleet: c.Fleet,
			LocalAddress: c.Local, Interface: targetInterface(c.Local, c.Remote),
			OpenedUnixNs: unixNano(c.Opened), Calls: c.Calls, OpenStreams: uint32(max(c.Streams, 0)), //nolint:gosec // G115: a count, never negative
			LastCallUnixNs: unixNano(c.LastCall), BytesReceived: c.BytesIn, BytesSent: c.BytesOut,
			UserCount: uint32(len(c.Users)), Self: mine, //nolint:gosec // G115: at most maxUsers
		}
		if admin || mine || c.Controller {
			pc.Name, pc.Roles, pc.RemoteAddress = c.Name, c.Roles, c.Remote
		}
		for _, u := range c.Users {
			you := mine && u.Name == caller.Name
			if !admin && !you {
				continue
			}
			pc.Users = append(pc.Users, &janusv1alpha1.ClientUser{
				Name: u.Name, Roles: u.Roles, Calls: u.Calls, OpenStreams: uint32(max(u.Streams, 0)), //nolint:gosec // G115: a count, never negative
				LastCallUnixNs: unixNano(u.LastCall), Self: you,
			})
		}
		resp.Connections = append(resp.Connections, pc)
	}
	return resp, nil
}
