package clusters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RouterOS as the balancer in front (docs/guide/clusters.md "RouterOS"):
// the engine keeps, on a RouterOS 7 router, a static route per member in
// service and service prefix - dst-address the prefix, gateway the
// member, the same distance -, so the router spreads the flows over
// them (ECMP). Its routes carry the comment janus-cluster:<id>, and a
// route without it is never touched. Every announce makes the router's
// routes match the list - an out removes the member's routes before the
// member is drained; Reconcile reads them again and puts back a change
// made on the router. Over its REST API (/rest), as a user of its own,
// its password the balancer's secret. A member's route goes to its
// address, or to its balancer_gateway variable when it has one. A
// read_only hook writes none - the members announce the prefixes over
// BGP - and removes those it wrote before; RouterRoles reads the routes
// whatever brought them, each member's role.

// RouterOSComment is the comment on the routes the engine keeps for
// cluster id.
func RouterOSComment(id string) string { return "janus-cluster:" + id }

var routingTableName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)

// rosID is a RouterOS item's id: "*" and hexadecimal digits.
var rosID = regexp.MustCompile(`^\*[0-9A-Fa-f]+$`)

func (b *Balancer) validateRouterOS(u *url.URL) error {
	switch {
	case u.Scheme == "http" && !b.AllowHTTP:
		return errors.New("balancer url: the router over https - its password goes with each call; allow_http for a lab's router")
	case u.Path != "" && u.Path != "/", u.RawQuery != "":
		return errors.New("balancer url: the router's address alone (https://router or https://router:port)")
	case b.User == "" || len(b.User) > 64 || strings.ContainsAny(b.User, ": \t"):
		return errors.New("balancer user: the router's user the engine signs in as")
	case len(b.Prefixes) == 0 || len(b.Prefixes) > 16:
		return errors.New("balancer prefixes: 1 to 16 service prefixes (203.0.113.10/32)")
	case b.Distance < 0 || b.Distance > 255:
		return errors.New("balancer distance: 1 to 255")
	case b.RoutingTable != "" && !routingTableName.MatchString(b.RoutingTable):
		return errors.New("balancer routing_table: a RouterOS routing table's name (main)")
	case b.ReadOnly && (b.Distance != 0 || b.CheckGateway):
		return errors.New("balancer distance, check_gateway: for the routes the engine writes - read_only reads the routes BGP brings")
	}
	for _, s := range b.Prefixes {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Masked() != p {
			return fmt.Errorf("balancer prefix %q: an address and its length, no host bits (203.0.113.10/32)", s)
		}
	}
	return nil
}

func (b *Balancer) distance() int {
	if b.Distance == 0 {
		return 1
	}
	return b.Distance
}

func (b *Balancer) routingTable() string {
	if b.RoutingTable == "" {
		return "main"
	}
	return b.RoutingTable
}

// rosRoute is a route as RouterOS's REST API gives it - every value a
// string.
type rosRoute struct {
	ID       string `json:".id,omitempty"`
	Dst      string `json:"dst-address"`
	Gateway  string `json:"gateway"`
	Distance string `json:"distance"`
	Table    string `json:"routing-table"`
	Comment  string `json:"comment"`
	// Check is check-gateway: "ping", or absent.
	Check string `json:"check-gateway,omitempty"`
	// Read, never written: "true"/"false", the interface's gateway.
	Active      string `json:"active,omitempty"`
	ECMP        string `json:"ecmp,omitempty"`
	ImmediateGW string `json:"immediate-gw,omitempty"`
}

type rosKey struct{ dst, gateway string }

// rosGateway is where member m's routes go.
func rosGateway(m BalancerMember) string {
	if g := m.Vars["balancer_gateway"]; g != "" {
		return g
	}
	return m.Address
}

// families are the address families b's prefixes are in: "ip" (IPv4),
// "ipv6" - RouterOS's menus.
func (b *Balancer) families() []string {
	var v4, v6 bool
	for _, s := range b.Prefixes {
		if p, err := netip.ParsePrefix(s); err == nil {
			v4, v6 = v4 || p.Addr().Is4(), v6 || p.Addr().Is6()
		}
	}
	var out []string
	if v4 {
		out = append(out, "ip")
	}
	if v6 {
		out = append(out, "ipv6")
	}
	return out
}

// routerOSApply makes cluster c's routes on its router match list: the
// routes of members out of service (or no longer members) removed
// first, then those missing added - in the address families its
// prefixes are in, and sweep's (a family no prefix is in any more: its
// routes all go).
func (e *Engine) routerOSApply(ctx context.Context, c *Cluster, list []BalancerMember, sweep ...string) (added, removed int, err error) {
	b := c.Balancer
	password, err := e.Store.BalancerSecret(c.ID)
	if err != nil {
		return 0, 0, err
	}
	client, err := BalancerClient(b)
	if err != nil {
		return 0, 0, err
	}
	r := rosClient{client: client, base: strings.TrimSuffix(b.URL, "/") + "/rest", user: b.User, password: string(password)}
	comment := RouterOSComment(c.ID)
	dist, table := strconv.Itoa(b.distance()), b.routingTable()
	check := ""
	if b.CheckGateway {
		check = "ping"
	}
	families := b.families()
	for _, f := range sweep {
		if !slices.Contains(families, f) {
			families = append(families, f)
		}
	}
	for _, family := range families {
		want := map[rosKey]bool{}
		for _, s := range b.Prefixes {
			p, err := netip.ParsePrefix(s)
			if err != nil || (family == "ip") != p.Addr().Is4() {
				continue
			}
			for _, m := range list {
				gw, err := netip.ParseAddr(rosGateway(m))
				if b.ReadOnly || !m.InService || err != nil || gw.Is4() != p.Addr().Is4() {
					continue // read_only: none of the cluster's, BGP's
				}
				want[rosKey{p.String(), gw.String()}] = true
			}
		}
		var have []rosRoute
		if err := r.do(ctx, http.MethodGet, "/"+family+"/route?comment="+url.QueryEscape(comment), nil, &have); err != nil {
			return added, removed, err
		}
		kept := map[rosKey]bool{}
		for _, h := range have {
			if h.Comment != comment {
				continue // never one that isn't the cluster's
			}
			k := rosKey{normalPrefix(h.Dst), normalAddr(h.Gateway)}
			if want[k] && !kept[k] && h.Distance == dist && h.Table == table && h.Check == check {
				kept[k] = true
				continue
			}
			if !rosID.MatchString(h.ID) {
				return added, removed, fmt.Errorf("the router gave a route the id %q", h.ID)
			}
			if err := r.do(ctx, http.MethodDelete, "/"+family+"/route/"+h.ID, nil, nil); err != nil {
				return added, removed, err
			}
			removed++
		}
		for k := range want {
			if kept[k] {
				continue
			}
			route := rosRoute{Dst: k.dst, Gateway: k.gateway, Distance: dist, Table: table, Comment: comment, Check: check}
			if err := r.do(ctx, http.MethodPut, "/"+family+"/route", route, nil); err != nil {
				return added, removed, err
			}
			added++
		}
	}
	return added, removed, nil
}

// RouterRole is a member's place on the router for one service prefix:
// Role "multipath" (its route active, one of several equal ones),
// "master" (the only active one), "backup" (present, not active),
// "absent" (no route through it); Active and Total count the prefix's
// routes.
type RouterRole struct {
	Prefix string `json:"prefix"`
	Role   string `json:"role"`
	Active int    `json:"active"`
	Total  int    `json:"total"`
}

// RouterView is a reading of a cluster's router: each member's roles,
// one per service prefix in the balancer's order; Unknown counts the
// routes through a gateway that is no member's.
type RouterView struct {
	ReadAt  time.Time               `json:"read_at"`
	Members map[string][]RouterRole `json:"members"`
	Unknown int                     `json:"unknown,omitempty"`
}

// RouterRoles reads cluster c's service prefixes' routes on its RouterOS
// router - whatever brought them: the members' BGP for a read_only hook,
// the engine's own otherwise - and says each member's role. A route's
// gateway is matched with the member's address, its balancer_gateway
// variable and the addresses on its interfaces: BIRD gives the router
// its link-local address as an IPv6 route's next hop.
func (e *Engine) RouterRoles(ctx context.Context, c *Cluster) (*RouterView, error) {
	b := c.Balancer
	if !b.IsRouterOS() {
		return nil, errors.New("the cluster has no RouterOS hook")
	}
	password, err := e.Store.BalancerSecret(c.ID)
	if err != nil {
		return nil, err
	}
	client, err := BalancerClient(b)
	if err != nil {
		return nil, err
	}
	r := rosClient{client: client, base: strings.TrimSuffix(b.URL, "/") + "/rest", user: b.User, password: string(password)}
	owner := map[netip.Addr]string{}
	for _, m := range c.Members {
		for _, s := range []string{m.Address, m.Vars["balancer_gateway"]} {
			if a, err := netip.ParseAddr(s); err == nil {
				owner[a.Unmap()] = m.Name
			}
		}
		// A member that doesn't answer: its address and gateway only.
		if addrs, err := e.Nodes.Addresses(ctx, m); err == nil {
			for _, a := range addrs {
				owner[a.Unmap()] = m.Name
			}
		}
	}
	view := &RouterView{ReadAt: time.Now().UTC(), Members: map[string][]RouterRole{}}
	table := b.routingTable()
	for _, s := range b.Prefixes {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			continue
		}
		family := "ip"
		if p.Addr().Is6() {
			family = "ipv6"
		}
		var routes []rosRoute
		if err := r.do(ctx, http.MethodGet, "/"+family+"/route?dst-address="+url.QueryEscape(p.String()), nil, &routes); err != nil {
			return nil, err
		}
		type via struct {
			member         string
			active, shared bool
		}
		var vias []via
		active, total := 0, 0
		for _, rt := range routes {
			if normalPrefix(rt.Dst) != p.String() || (rt.Table != "" && rt.Table != table) {
				continue
			}
			total++
			on := rt.Active == "true"
			if on {
				active++
			}
			name := ""
			for _, g := range []string{rt.Gateway, rt.ImmediateGW} {
				if a, err := netip.ParseAddr(normalAddr(g)); err == nil && owner[a.Unmap()] != "" {
					name = owner[a.Unmap()]
					break
				}
			}
			if name == "" {
				view.Unknown++
				continue
			}
			vias = append(vias, via{name, on, rt.ECMP == "true"})
		}
		for _, m := range c.Members {
			role := "absent"
			for _, v := range vias {
				if v.member != m.Name {
					continue
				}
				switch {
				case v.active && (v.shared || active > 1):
					role = "multipath"
				case v.active && role != "multipath":
					role = "master"
				case !v.active && role == "absent":
					role = "backup"
				}
			}
			view.Members[m.Name] = append(view.Members[m.Name], RouterRole{Prefix: p.String(), Role: role, Active: active, Total: total})
		}
	}
	return view, nil
}

func normalPrefix(s string) string {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.String()
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()).String()
	}
	return s
}

// normalAddr is a route's gateway as an address: RouterOS may add the
// interface ("10.0.0.21%ether1").
func normalAddr(s string) string {
	s, _, _ = strings.Cut(s, "%")
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String()
	}
	return s
}

// reconcileRouterOS reads cluster c's routes on its router and puts
// back what changed there; silent when nothing did.
func (e *Engine) reconcileRouterOS(ctx context.Context, c *Cluster) error {
	list := e.BalancerList(c)
	added, removed, err := e.routerOSApply(ctx, c, list)
	_ = e.Store.UpdateState(c.ID, func(s *State) {
		if err != nil {
			s.Balancer.Error = err.Error()
			return
		}
		s.Balancer = BalancerState{Sent: listHash(list), SentAt: time.Now().UTC()}
	})
	if err != nil {
		e.Journal(c.ID, Actor, "balancer", "", 0, "the router wasn't told the members: %v", err)
		return err
	}
	if added+removed > 0 {
		e.Journal(c.ID, Actor, "balancer", "", 0, "the router's routes put back (%d added, %d removed): changed on the router, or a call missed", added, removed)
	}
	return nil
}

// rosClient calls a RouterOS router's REST API, a failed call tried
// again as a webhook's is.
type rosClient struct {
	client         *http.Client
	base           string
	user, password string
}

func (r rosClient) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	var last error
	for attempt := 0; ; attempt++ {
		last = r.once(ctx, method, path, payload, out)
		var final *finalError
		if last == nil || errors.As(last, &final) || attempt >= len(BalancerRetries) {
			break
		}
		select {
		case <-time.After(BalancerRetries[attempt]):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var final *finalError
	if errors.As(last, &final) {
		return final.err
	}
	return last
}

func (r rosClient) once(ctx context.Context, method, path string, payload []byte, out any) error {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, rd)
	if err != nil {
		return &finalError{err}
	}
	req.SetBasicAuth(r.user, r.password)
	req.Header.Set("User-Agent", "Janus-cluster-balancer/1")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 == 2 {
		if out != nil && len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return &finalError{fmt.Errorf("%s %s: %v", method, path, err)}
			}
		}
		return nil
	}
	// RouterOS answers {"error":400,"message":"Bad Request","detail":"..."}.
	var why struct{ Message, Detail string }
	_ = json.Unmarshal(data, &why)
	err = fmt.Errorf("%s %s answered %s", method, strings.SplitN(path, "?", 2)[0], resp.Status)
	if why.Detail != "" || why.Message != "" {
		err = fmt.Errorf("%v: %s", err, strings.TrimSpace(why.Message+" "+why.Detail))
	}
	if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusRequestTimeout {
		return &finalError{err}
	}
	return err
}
