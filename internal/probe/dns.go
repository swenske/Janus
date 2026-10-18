package probe

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
)

var recordTypes = map[string]bool{"A": true, "AAAA": true, "CNAME": true, "SRV": true, "TXT": true, "MX": true, "NS": true, "PTR": true}

// resolver asks the node's resolv.conf servers, or Server, from Source.
func (p *prober) resolver() *net.Resolver {
	if p.r.Server == "" && !p.r.Source.IsValid() {
		return &net.Resolver{PreferGo: true}
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if p.r.Server != "" {
			host, port, _ := hostPort(p.r.Server, 53)
			address = net.JoinHostPort(host, strconv.Itoa(port))
		}
		d := net.Dialer{Timeout: p.r.Timeout}
		if p.r.Source.IsValid() {
			if strings.HasPrefix(network, "tcp") {
				d.LocalAddr = &net.TCPAddr{IP: p.r.Source.AsSlice()}
			} else {
				d.LocalAddr = &net.UDPAddr{IP: p.r.Source.AsSlice()}
			}
		}
		return d.DialContext(ctx, network, address)
	}}
}

func (p *prober) serverName() string {
	if p.r.Server != "" {
		return p.r.Server
	}
	return "resolv.conf"
}

// dns answers a "dns" probe: one lookup.
func (p *prober) dns(ctx context.Context) error {
	start := time.Now()
	records, err := p.lookup(ctx, p.r.Target, p.r.RecordType)
	ev := Event{Stage: "dns", OK: err == nil, ElapsedMS: since(start), DNS: &DNSResult{Name: p.r.Target, Type: cmpOr(p.r.RecordType, "A+AAAA"), Server: p.serverName(), Records: records}}
	if err != nil {
		ev.Message = err.Error()
	}
	if err := p.emit(ev); err != nil {
		return err
	}
	if err != nil {
		return p.done(false, err.Error(), nil)
	}
	return p.done(true, fmt.Sprintf("%d records", len(records)), nil)
}

func (p *prober) lookup(ctx context.Context, name, typ string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.r.Timeout)
	defer cancel()
	res := p.resolver()
	var out []string
	switch typ {
	case "", "A", "AAAA":
		network := map[string]string{"": "ip", "A": "ip4", "AAAA": "ip6"}[typ]
		ips, err := res.LookupNetIP(ctx, network, name)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			out = append(out, ip.Unmap().String())
		}
	case "CNAME":
		c, err := res.LookupCNAME(ctx, name)
		if err != nil {
			return nil, err
		}
		out = []string{c}
	case "SRV":
		_, srvs, err := res.LookupSRV(ctx, "", "", name)
		if err != nil {
			return nil, err
		}
		for _, s := range srvs {
			out = append(out, fmt.Sprintf("%d %d %d %s", s.Priority, s.Weight, s.Port, s.Target))
		}
	case "TXT":
		txts, err := res.LookupTXT(ctx, name)
		if err != nil {
			return nil, err
		}
		out = txts
	case "MX":
		mxs, err := res.LookupMX(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, m := range mxs {
			out = append(out, fmt.Sprintf("%d %s", m.Pref, m.Host))
		}
	case "NS":
		nss, err := res.LookupNS(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, n := range nss {
			out = append(out, n.Host)
		}
		sort.Strings(out)
	case "PTR":
		names, err := res.LookupAddr(ctx, name)
		if err != nil {
			return nil, err
		}
		out = names
	}
	return out, nil
}

// addresses resolves host for a connection or an echo request: itself
// when it's an address - no "dns" event then -, else its A and AAAA
// records (a "dns" event), those of Source's family only when one is
// set. failed says why there's none; err is emit's.
func (p *prober) addresses(ctx context.Context, host string) (addrs []netip.Addr, failed string, err error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}, "", nil
	}
	start := time.Now()
	records, lerr := p.lookup(ctx, host, "")
	ev := Event{Stage: "dns", OK: lerr == nil, ElapsedMS: since(start), DNS: &DNSResult{Name: host, Type: "A+AAAA", Server: p.serverName(), Records: records}}
	if lerr != nil {
		ev.Message = lerr.Error()
	}
	if err := p.emit(ev); err != nil {
		return nil, "", err
	}
	if lerr != nil {
		return nil, lerr.Error(), nil
	}
	for _, r := range records {
		a, err := netip.ParseAddr(r)
		if err != nil || (p.r.Source.IsValid() && a.Is4() != p.r.Source.Is4()) {
			continue
		}
		addrs = append(addrs, a)
	}
	if len(addrs) == 0 {
		return nil, host + " has no address of the source's family", nil
	}
	return addrs, "", nil
}

// reverse is addr's name, quickly or not at all - a traceroute's hops.
func (p *prober) reverse(ctx context.Context, addr string) string {
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	names, err := p.resolver().LookupAddr(ctx, addr)
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[0], ".")
}

func cmpOr(s, otherwise string) string {
	if s != "" {
		return s
	}
	return otherwise
}
