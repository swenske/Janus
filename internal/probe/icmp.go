package probe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// Echo requests from a raw ICMP socket (janusd's CAP_NET_RAW - the
// kernel's unprivileged ping sockets need net.ipv4.ping_group_range,
// which a node leaves closed). A raw socket sees every ICMP message the
// node receives: ours are told apart by a random identifier, and the
// errors routers send back by the echo header they quote.

// icmpSock is a raw ICMP or ICMPv6 socket.
type icmpSock struct {
	c  *net.IPConn
	v6 bool
	p4 *ipv4.PacketConn
	p6 *ipv6.PacketConn
	id int
}

// answer is what came back for one of our echo requests.
type answer struct {
	kind  string // reply, time-exceeded, unreachable, too-big
	seq   int
	from  netip.Addr
	ttl   int
	bytes int
	mtu   int
	at    time.Time
}

func openICMP(dst, source netip.Addr, df bool) (*icmpSock, error) {
	network, laddr := "ip4:icmp", "0.0.0.0"
	if dst.Is6() {
		network, laddr = "ip6:ipv6-icmp", "::"
	}
	if source.IsValid() {
		laddr = source.String()
	}
	pc, err := net.ListenPacket(network, laddr)
	if err != nil {
		return nil, err
	}
	s := &icmpSock{c: pc.(*net.IPConn), v6: dst.Is6()}
	var id [2]byte
	_, _ = rand.Read(id[:])
	s.id = int(binary.BigEndian.Uint16(id[:]))
	if s.v6 {
		s.p6 = ipv6.NewPacketConn(s.c)
		_ = s.p6.SetControlMessage(ipv6.FlagHopLimit, true)
		var f ipv6.ICMPFilter
		f.SetAll(true)
		for _, t := range []ipv6.ICMPType{ipv6.ICMPTypeEchoReply, ipv6.ICMPTypeTimeExceeded, ipv6.ICMPTypeDestinationUnreachable, ipv6.ICMPTypePacketTooBig} {
			f.Accept(t)
		}
		_ = s.p6.SetICMPFilter(&f)
	} else {
		s.p4 = ipv4.NewPacketConn(s.c)
		_ = s.p4.SetControlMessage(ipv4.FlagTTL, true)
	}
	// DF set, or never: a large echo request is fragmented, or refused
	// with the MTU that stopped it - the kernel's default for raw
	// sockets is in between.
	raw, err := s.c.SyscallConn()
	if err == nil {
		var serr error
		err = raw.Control(func(fd uintptr) {
			if s.v6 {
				mode := map[bool]int{true: unix.IPV6_PMTUDISC_DO, false: unix.IPV6_PMTUDISC_DONT}[df]
				serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, mode)
			} else {
				mode := map[bool]int{true: unix.IP_PMTUDISC_DO, false: unix.IP_PMTUDISC_DONT}[df]
				serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, mode)
			}
		})
		if err == nil {
			err = serr
		}
	}
	if err != nil {
		s.c.Close()
		return nil, fmt.Errorf("the DF bit: %w", err)
	}
	return s, nil
}

func (s *icmpSock) close() { s.c.Close() }

func (s *icmpSock) setTTL(ttl int) error {
	if s.v6 {
		return s.p6.SetHopLimit(ttl)
	}
	return s.p4.SetTTL(ttl)
}

func (s *icmpSock) send(dst netip.Addr, seq int, payload []byte) error {
	var typ icmp.Type = ipv4.ICMPTypeEcho
	if s.v6 {
		typ = ipv6.ICMPTypeEchoRequest // the kernel sums ICMPv6 itself
	}
	b, err := (&icmp.Message{Type: typ, Body: &icmp.Echo{ID: s.id, Seq: seq, Data: payload}}).Marshal(nil)
	if err != nil {
		return err
	}
	_, err = s.c.WriteTo(b, &net.IPAddr{IP: dst.AsSlice()})
	return err
}

// read waits until deadline for an answer to one of our echo requests.
func (s *icmpSock) read(deadline time.Time) (answer, error) {
	buf := make([]byte, 65536)
	if err := s.c.SetReadDeadline(deadline); err != nil {
		return answer{}, err
	}
	for {
		var (
			n, ttl int
			src    net.Addr
			err    error
		)
		if s.v6 {
			var cm *ipv6.ControlMessage
			n, cm, src, err = s.p6.ReadFrom(buf)
			if cm != nil {
				ttl = cm.HopLimit
			}
		} else {
			var cm *ipv4.ControlMessage
			n, cm, src, err = s.p4.ReadFrom(buf)
			if cm != nil {
				ttl = cm.TTL
			}
		}
		if err != nil {
			return answer{}, err
		}
		at := time.Now()
		msg := buf[:n]
		// A raw IPv4 socket may hand the IP header over too: no ICMP
		// type we read starts with 0x4_.
		if !s.v6 && len(msg) >= 20 && msg[0]>>4 == 4 {
			msg = msg[int(msg[0]&0x0f)*4:]
		}
		if a, ok := s.parse(msg); ok {
			a.ttl, a.at = ttl, at
			if ip, ok := src.(*net.IPAddr); ok {
				a.from, _ = netip.AddrFromSlice(ip.IP)
				a.from = a.from.Unmap()
			}
			return a, nil
		}
	}
}

// parse reads an ICMP message: one of ours, or a router's about one of
// ours.
func (s *icmpSock) parse(msg []byte) (answer, bool) {
	proto := 1
	if s.v6 {
		proto = 58
	}
	m, err := icmp.ParseMessage(proto, msg)
	if err != nil {
		return answer{}, false
	}
	a := answer{bytes: len(msg)}
	var quoted []byte
	switch b := m.Body.(type) {
	case *icmp.Echo:
		if m.Type != ipv4.ICMPTypeEchoReply && m.Type != ipv6.ICMPTypeEchoReply || b.ID != s.id {
			return answer{}, false
		}
		a.kind, a.seq = "reply", b.Seq
		return a, true
	case *icmp.TimeExceeded:
		a.kind, quoted = "time-exceeded", b.Data
	case *icmp.DstUnreach:
		a.kind, quoted = "unreachable", b.Data
		if !s.v6 && m.Code == 4 && len(msg) >= 8 { // fragmentation needed: the next hop's MTU
			a.kind, a.mtu = "too-big", int(binary.BigEndian.Uint16(msg[6:8]))
		}
	case *icmp.PacketTooBig:
		a.kind, a.mtu, quoted = "too-big", b.MTU, b.Data
	default:
		return answer{}, false
	}
	id, seq, ok := quotedEcho(quoted, s.v6)
	if !ok || id != s.id {
		return answer{}, false
	}
	a.seq = seq
	return a, true
}

// quotedEcho finds our echo request's identifier and sequence in the
// datagram an ICMP error quotes: its IP header, then at least 8 bytes.
func quotedEcho(data []byte, v6 bool) (id, seq int, ok bool) {
	if v6 {
		if len(data) < 48 || data[6] != 58 || data[40] != 128 {
			return 0, 0, false
		}
		return int(binary.BigEndian.Uint16(data[44:46])), int(binary.BigEndian.Uint16(data[46:48])), true
	}
	if len(data) < 20 {
		return 0, 0, false
	}
	ihl := int(data[0]&0x0f) * 4
	if data[9] != 1 || len(data) < ihl+8 || data[ihl] != 8 {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint16(data[ihl+4 : ihl+6])), int(binary.BigEndian.Uint16(data[ihl+6 : ihl+8])), true
}

// routeMTU is the MTU of the node's route to dst, 0 when unknown: what a
// connected UDP socket's IP_MTU says.
func routeMTU(dst netip.Addr) int {
	c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: dst.AsSlice(), Port: 9})
	if err != nil {
		return 0
	}
	defer c.Close()
	raw, err := c.SyscallConn()
	if err != nil {
		return 0
	}
	mtu := 0
	_ = raw.Control(func(fd uintptr) {
		if dst.Is6() {
			mtu, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU)
		} else {
			mtu, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU)
		}
	})
	return mtu
}

// deadline is the earlier of after and ctx's.
func deadline(ctx context.Context, after time.Duration) time.Time {
	d := time.Now().Add(after)
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		return cd
	}
	return d
}

func timedOut(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// target resolves an echo probe's target and opens the socket for it -
// ok false when the probe is over (done sent).
func (p *prober) echoSocket(ctx context.Context) (*icmpSock, netip.Addr, bool, error) {
	addrs, failed, err := p.addresses(ctx, p.r.Target)
	if err != nil || failed != "" {
		if err == nil {
			err = p.done(false, failed, nil)
		}
		return nil, netip.Addr{}, false, err
	}
	dst := addrs[0]
	s, err := openICMP(dst, p.r.Source, p.r.DontFragment)
	if err != nil {
		return nil, dst, false, p.done(false, "a raw ICMP socket: "+err.Error(), nil)
	}
	return s, dst, true, nil
}

// ping answers a "ping" probe: Count echo requests, Interval apart,
// each waiting Timeout for its reply.
func (p *prober) ping(ctx context.Context) error {
	s, dst, ok, err := p.echoSocket(ctx)
	if !ok {
		return err
	}
	defer s.close()
	payload := make([]byte, p.r.Size)
	for i := range payload {
		payload[i] = byte(i)
	}
	sum := &Summary{}
	var total float64
	next := time.Now()
	for seq := 1; seq <= p.r.Count; seq++ {
		if seq > 1 {
			select {
			case <-ctx.Done():
				return p.done(sum.Received > 0, "the probe's time is up", sum)
			case <-time.After(time.Until(next)):
			}
		}
		next = time.Now().Add(p.r.Interval)
		reply := &Reply{Seq: seq}
		ev := Event{Stage: "reply", Reply: reply}
		start := time.Now()
		sum.Sent++
		if err := s.send(dst, seq, payload); err != nil {
			reply.Kind, ev.Message = "unsent", err.Error()
			if errors.Is(err, syscall.EMSGSIZE) {
				reply.Kind, reply.MTU = "too-big", routeMTU(dst)
				ev.Message = fmt.Sprintf("larger than the route's MTU (%d), and DF is set", reply.MTU)
			}
		} else {
			for {
				a, err := s.read(deadline(ctx, p.r.Timeout-time.Since(start)))
				if err != nil {
					reply.Kind = "timeout"
					if !timedOut(err) {
						ev.Message = err.Error()
					}
					break
				}
				if a.seq != seq { // a late answer to an earlier one
					continue
				}
				reply.From, reply.TTL, reply.Bytes, reply.Kind, reply.MTU = a.from.String(), a.ttl, a.bytes, a.kind, a.mtu
				reply.RTTMS = float64(a.at.Sub(start).Microseconds()) / 1000
				if a.kind == "reply" {
					ev.OK = true
					sum.Received++
					total += reply.RTTMS
					if sum.MinMS == 0 || reply.RTTMS < sum.MinMS {
						sum.MinMS = reply.RTTMS
					}
					sum.MaxMS = max(sum.MaxMS, reply.RTTMS)
				}
				break
			}
		}
		ev.ElapsedMS = since(start)
		if err := p.emit(ev); err != nil {
			return err
		}
	}
	if sum.Received > 0 {
		sum.AvgMS = total / float64(sum.Received)
	}
	sum.Reached = sum.Received > 0
	return p.done(sum.Received > 0, fmt.Sprintf("%d sent, %d received", sum.Sent, sum.Received), sum)
}

// traceroute answers a "traceroute" probe: three echo requests per TTL,
// 1 to MaxHops, until the target answers or a router says it's
// unreachable.
func (p *prober) traceroute(ctx context.Context) error {
	s, dst, ok, err := p.echoSocket(ctx)
	if !ok {
		return err
	}
	defer s.close()
	payload := make([]byte, p.r.Size)
	sum := &Summary{}
	for ttl := 1; ttl <= p.r.MaxHops; ttl++ {
		if ctx.Err() != nil {
			return p.done(false, "the probe's time is up", sum)
		}
		if err := s.setTTL(ttl); err != nil {
			return p.done(false, "setting the TTL: "+err.Error(), sum)
		}
		hop := &Hop{TTL: ttl, RTTsMS: []float64{-1, -1, -1}}
		ev := Event{Stage: "hop", Hop: hop}
		start := time.Now()
		sent := [3]time.Time{}
		for i := range 3 {
			sent[i] = time.Now()
			if err := s.send(dst, ttl*4+i, payload); err != nil {
				ev.Message = err.Error()
			}
		}
		end := deadline(ctx, p.r.Timeout)
		unreachable := false
		for got := 0; got < 3; {
			a, err := s.read(end)
			if err != nil {
				break
			}
			i := a.seq - ttl*4
			if i < 0 || i > 2 || hop.RTTsMS[i] >= 0 {
				continue
			}
			hop.RTTsMS[i] = float64(a.at.Sub(sent[i]).Microseconds()) / 1000
			hop.Address = a.from.String()
			got++
			switch a.kind {
			case "reply":
				hop.Reached = true
			case "unreachable", "too-big":
				unreachable = true
				ev.Message = a.kind
			}
		}
		ev.OK = hop.Address != ""
		ev.ElapsedMS = since(start)
		if hop.Address != "" {
			hop.Name = p.reverse(ctx, hop.Address)
		}
		if err := p.emit(ev); err != nil {
			return err
		}
		sum.Hops = ttl
		if hop.Reached {
			sum.Reached = true
			return p.done(true, fmt.Sprintf("%s reached in %d hop%s", dst, ttl, map[bool]string{true: "", false: "s"}[ttl == 1]), sum)
		}
		if unreachable {
			return p.done(false, fmt.Sprintf("hop %d says %s is unreachable", ttl, dst), sum)
		}
	}
	return p.done(false, fmt.Sprintf("%s not reached in %d hops", dst, p.r.MaxHops), sum)
}
