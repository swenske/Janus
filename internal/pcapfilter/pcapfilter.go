// Package pcapfilter compiles a strict subset of tcpdump's filter
// language to a classic BPF program for Ethernet-framed packets, with no
// libpcap and no cgo.
//
// It exists because the only maintained pure-Go alternative
// (github.com/packetcap/go-pcap/filter) was found silently miscompiling
// ordinary expressions - "icmp" accepted every packet, "tcp[tcpflags] &
// tcp-syn != 0" never read the flags byte - and a capture filter that
// quietly lets everything through is worse than none. Anything outside
// the subset below is rejected with an error, never approximated.
//
// Supported grammar (keywords are case-sensitive, as in tcpdump):
//
//	expr      := factor { ("and" | "&&" | "or" | "||") factor }
//	factor    := ("not" | "!") factor | "(" expr ")" | primitive
//	primitive := proto
//	           | [proto] [dir] "host" ADDR
//	           | [proto] [dir] "net" CIDR
//	           | [proto] [dir] "port" N
//	           | [proto] [dir] "portrange" N-M
//	           | [proto] dir ADDR            (same as "dir host ADDR")
//	proto     := "ip" | "ip6" | "arp" | "tcp" | "udp" | "icmp" | "icmp6"
//	dir       := "src" | "dst"
//
// Semantics follow tcpdump: "and" and "or" have equal precedence and
// associate left to right ("a or b and c" is "(a or b) and c"); host/net match IPv4, IPv6 and ARP sender/
// target addresses; port/portrange match TCP, UDP and SCTP over IPv4
// (first fragment only) and IPv6 (no extension headers); no direction
// means source or destination.
package pcapfilter

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/net/bpf"
)

const (
	etherTypeIPv4 = 0x0800
	etherTypeARP  = 0x0806
	etherTypeIPv6 = 0x86dd

	protoICMP   = 1
	protoTCP    = 6
	protoUDP    = 17
	protoICMPv6 = 58
	protoSCTP   = 132
)

// Compile returns a BPF program that returns accept for a matching
// packet and 0 otherwise. An empty expression is an error: callers
// wanting no filter should not attach one.
func Compile(expr string, accept uint32) ([]bpf.Instruction, error) {
	toks := tokenize(expr)
	if len(toks) == 0 {
		return nil, errors.New("empty filter expression")
	}
	p := &parser{toks: toks}
	root, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.toks[p.pos])
	}

	var b builder
	acceptL, rejectL := b.newLabel(), b.newLabel()
	root.gen(&b, acceptL, rejectL)
	b.mark(acceptL)
	b.emit(bpf.RetConstant{Val: accept})
	b.mark(rejectL)
	b.emit(bpf.RetConstant{Val: 0})
	return b.resolve()
}

// --- lexer / parser ---

func tokenize(s string) []string {
	var toks []string
	for _, f := range strings.Fields(s) {
		for strings.HasPrefix(f, "!") && f != "!" {
			toks = append(toks, "!")
			f = f[1:]
		}
		for len(f) > 0 {
			i := strings.IndexAny(f, "()")
			if i < 0 {
				toks = append(toks, f)
				break
			}
			if i > 0 {
				toks = append(toks, f[:i])
			}
			toks = append(toks, f[i:i+1])
			f = f[i+1:]
		}
	}
	return toks
}

type parser struct {
	toks []string
	pos  int
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *parser) next() string {
	t := p.peek()
	if t != "" {
		p.pos++
	}
	return t
}

// parseExpr gives "and" and "or" equal precedence, left-associative -
// pcap-filter(7)'s rule, not C's.
func (p *parser) parseExpr() (node, error) {
	left, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for {
		op := p.peek()
		if op != "and" && op != "&&" && op != "or" && op != "||" {
			return left, nil
		}
		p.next()
		right, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		if op == "and" || op == "&&" {
			left = andNode{left, right}
		} else {
			left = orNode{left, right}
		}
	}
}

func (p *parser) parseFactor() (node, error) {
	switch t := p.peek(); t {
	case "":
		return nil, errors.New("unexpected end of expression")
	case "not", "!":
		p.next()
		n, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	case "(":
		p.next()
		n, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, errors.New(`missing ")"`)
		}
		return n, nil
	}
	return p.parsePrimitive()
}

var protoKeywords = map[string]bool{"ip": true, "ip6": true, "arp": true, "tcp": true, "udp": true, "icmp": true, "icmp6": true}

func (p *parser) parsePrimitive() (node, error) {
	proto := ""
	if protoKeywords[p.peek()] {
		proto = p.next()
	}
	dir := ""
	if t := p.peek(); t == "src" || t == "dst" {
		dir = p.next()
	}
	kind := p.peek()
	switch kind {
	case "host", "net", "port", "portrange":
		p.next()
	default:
		if dir != "" && kind != "" && net.ParseIP(kind) != nil {
			kind = "host" // "src 10.0.0.1"
		} else if dir == "" && proto != "" {
			return protoNode{proto}, nil
		} else if kind == "" {
			return nil, errors.New("unexpected end of expression")
		} else {
			return nil, fmt.Errorf("unsupported filter keyword %q (supported: ip ip6 arp tcp udp icmp icmp6, [src|dst] host/net/port/portrange, and/or/not, parentheses)", kind)
		}
	}
	value := p.next()
	if value == "" {
		return nil, fmt.Errorf("%q needs a value", kind)
	}

	var prim node
	switch kind {
	case "host", "net":
		ipnet, err := parseAddr(kind, value)
		if err != nil {
			return nil, err
		}
		prim = addrNode{net: ipnet, dir: dir}
	case "port", "portrange":
		lo, hi, err := parsePorts(kind, value)
		if err != nil {
			return nil, err
		}
		if proto != "" && proto != "tcp" && proto != "udp" {
			return nil, fmt.Errorf("%q can't qualify %q", proto, kind)
		}
		prim = portNode{lo: lo, hi: hi, dir: dir}
	}
	if proto != "" {
		return andNode{protoNode{proto}, prim}, nil
	}
	return prim, nil
}

func parseAddr(kind, s string) (*net.IPNet, error) {
	if kind == "host" {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("host %q: not an IP address (host names aren't resolved)", s)
		}
		bits := 128
		if ip.To4() != nil {
			ip, bits = ip.To4(), 32
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
	}
	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return nil, fmt.Errorf("net %q: expected CIDR notation like 10.0.0.0/8", s)
	}
	if ip4 := ipnet.IP.To4(); ip4 != nil {
		ipnet.IP = ip4
	}
	return ipnet, nil
}

func parsePorts(kind, s string) (uint32, uint32, error) {
	parse := func(v string) (uint32, error) {
		n, err := strconv.ParseUint(v, 10, 16)
		if err != nil {
			return 0, fmt.Errorf("%s %q: expected a port number (service names aren't supported)", kind, s)
		}
		return uint32(n), nil
	}
	if kind == "port" {
		n, err := parse(s)
		return n, n, err
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("portrange %q: expected N-M", s)
	}
	lo, err := parse(a)
	if err != nil {
		return 0, 0, err
	}
	hi, err := parse(b)
	if err != nil {
		return 0, 0, err
	}
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi, nil
}

// --- code generation ---
//
// Each node emits "jumping code": instructions that end by jumping to
// label t when the node matches and f when it doesn't. Nothing assumes
// register contents left behind by another node.

type node interface {
	gen(b *builder, t, f label)
}

type andNode struct{ l, r node }
type orNode struct{ l, r node }
type notNode struct{ n node }

func (n andNode) gen(b *builder, t, f label) {
	mid := b.newLabel()
	n.l.gen(b, mid, f)
	b.mark(mid)
	n.r.gen(b, t, f)
}

func (n orNode) gen(b *builder, t, f label) {
	mid := b.newLabel()
	n.l.gen(b, t, mid)
	b.mark(mid)
	n.r.gen(b, t, f)
}

func (n notNode) gen(b *builder, t, f label) { n.n.gen(b, f, t) }

type protoNode struct{ proto string }

func (n protoNode) gen(b *builder, t, f label) {
	switch n.proto {
	case "ip":
		b.etherType(etherTypeIPv4, t, f)
	case "ip6":
		b.etherType(etherTypeIPv6, t, f)
	case "arp":
		b.etherType(etherTypeARP, t, f)
	case "icmp":
		ok := b.newLabel()
		b.etherType(etherTypeIPv4, ok, f)
		b.mark(ok)
		b.emit(bpf.LoadAbsolute{Off: 23, Size: 1})
		b.jeq(protoICMP, t, f)
	case "icmp6":
		ok := b.newLabel()
		b.etherType(etherTypeIPv6, ok, f)
		b.mark(ok)
		b.emit(bpf.LoadAbsolute{Off: 20, Size: 1})
		b.jeq(protoICMPv6, t, f)
	case "tcp", "udp":
		proto := uint32(protoTCP)
		if n.proto == "udp" {
			proto = protoUDP
		}
		v4, v6, notV4 := b.newLabel(), b.newLabel(), b.newLabel()
		b.emit(bpf.LoadAbsolute{Off: 12, Size: 2})
		b.jeq(etherTypeIPv4, v4, notV4)
		b.mark(notV4)
		b.jeq(etherTypeIPv6, v6, f)
		b.mark(v4)
		b.emit(bpf.LoadAbsolute{Off: 23, Size: 1})
		b.jeq(proto, t, f)
		b.mark(v6)
		b.emit(bpf.LoadAbsolute{Off: 20, Size: 1})
		b.jeq(proto, t, f)
	}
}

type addrNode struct {
	net *net.IPNet
	dir string
}

func (n addrNode) gen(b *builder, t, f label) {
	// Offsets of the source/destination address in each frame type.
	type loc struct {
		etherType uint32
		src, dst  uint32
	}
	var locs []loc
	if n.net.IP.To4() != nil && len(n.net.IP) == 4 {
		locs = []loc{{etherTypeIPv4, 26, 30}, {etherTypeARP, 28, 38}}
	} else {
		locs = []loc{{etherTypeIPv6, 22, 38}}
	}

	b.emit(bpf.LoadAbsolute{Off: 12, Size: 2})
	for i, l := range locs {
		match, next := b.newLabel(), f
		if i < len(locs)-1 {
			next = b.newLabel()
		}
		b.jeq(l.etherType, match, next)
		b.mark(match)
		switch n.dir {
		case "src":
			b.matchPrefix(l.src, n.net, t, f)
		case "dst":
			b.matchPrefix(l.dst, n.net, t, f)
		default:
			tryDst := b.newLabel()
			b.matchPrefix(l.src, n.net, t, tryDst)
			b.mark(tryDst)
			b.matchPrefix(l.dst, n.net, t, f)
		}
		if next != f {
			b.mark(next)
			b.emit(bpf.LoadAbsolute{Off: 12, Size: 2})
		}
	}
}

type portNode struct {
	lo, hi uint32
	dir    string
}

func (n portNode) gen(b *builder, t, f label) {
	v4, v6, notV4 := b.newLabel(), b.newLabel(), b.newLabel()
	b.emit(bpf.LoadAbsolute{Off: 12, Size: 2})
	b.jeq(etherTypeIPv4, v4, notV4)
	b.mark(notV4)
	b.jeq(etherTypeIPv6, v6, f)

	// IPv4: L4 protocol has ports, packet isn't a non-first fragment,
	// then the L4 header sits after a variable-length IP header.
	b.mark(v4)
	v4ok, v4frag := b.newLabel(), b.newLabel()
	b.emit(bpf.LoadAbsolute{Off: 23, Size: 1})
	b.portProto(v4ok, f)
	b.mark(v4ok)
	b.emit(bpf.LoadAbsolute{Off: 20, Size: 2})
	b.cond(bpf.JumpBitsSet, 0x1fff, f, v4frag)
	b.mark(v4frag)
	b.emit(bpf.LoadMemShift{Off: 14})
	n.ports(b, func(off uint32) bpf.Instruction { return bpf.LoadIndirect{Off: 14 + off, Size: 2} }, t, f)

	// IPv6, no extension headers - the same assumption tcpdump makes.
	b.mark(v6)
	v6ok := b.newLabel()
	b.emit(bpf.LoadAbsolute{Off: 20, Size: 1})
	b.portProto(v6ok, f)
	b.mark(v6ok)
	n.ports(b, func(off uint32) bpf.Instruction { return bpf.LoadAbsolute{Off: 54 + off, Size: 2} }, t, f)
}

// ports compares the source (offset 0) and/or destination (offset 2)
// port loaded by load.
func (n portNode) ports(b *builder, load func(off uint32) bpf.Instruction, t, f label) {
	var offs []uint32
	switch n.dir {
	case "src":
		offs = []uint32{0}
	case "dst":
		offs = []uint32{2}
	default:
		offs = []uint32{0, 2}
	}
	for i, off := range offs {
		miss := f
		if i < len(offs)-1 {
			miss = b.newLabel()
		}
		b.emit(load(off))
		if n.lo == n.hi {
			b.jeq(n.lo, t, miss)
		} else {
			geLo := b.newLabel()
			b.cond(bpf.JumpGreaterOrEqual, n.lo, geLo, miss)
			b.mark(geLo)
			b.cond(bpf.JumpGreaterThan, n.hi, miss, t)
		}
		if miss != f {
			b.mark(miss)
		}
	}
}

// --- builder: instructions with symbolic jump targets ---

type label int

type item struct {
	ins    bpf.Instruction // plain instruction, nil for jumps/labels
	isJump bool
	cond   bpf.JumpTest
	val    uint32
	t, f   label
	mark   label // >0 for a label definition
}

type builder struct {
	items  []item
	labels int
}

func (b *builder) newLabel() label          { b.labels++; return label(b.labels) }
func (b *builder) mark(l label)             { b.items = append(b.items, item{mark: l}) }
func (b *builder) emit(ins bpf.Instruction) { b.items = append(b.items, item{ins: ins}) }

func (b *builder) cond(c bpf.JumpTest, val uint32, t, f label) {
	b.items = append(b.items, item{isJump: true, cond: c, val: val, t: t, f: f})
}

func (b *builder) jeq(val uint32, t, f label) { b.cond(bpf.JumpEqual, val, t, f) }

func (b *builder) etherType(et uint32, t, f label) {
	b.emit(bpf.LoadAbsolute{Off: 12, Size: 2})
	b.jeq(et, t, f)
}

// portProto jumps to t when A holds TCP, UDP or SCTP.
func (b *builder) portProto(t, f label) {
	notTCP, notUDP := b.newLabel(), b.newLabel()
	b.jeq(protoTCP, t, notTCP)
	b.mark(notTCP)
	b.jeq(protoUDP, t, notUDP)
	b.mark(notUDP)
	b.jeq(protoSCTP, t, f)
}

// matchPrefix compares the address at off against ipnet, one 32-bit
// word at a time, skipping words the mask doesn't cover.
func (b *builder) matchPrefix(off uint32, ipnet *net.IPNet, t, f label) {
	type word struct{ off, val, mask uint32 }
	var words []word
	for i := 0; i < len(ipnet.IP); i += 4 {
		mask := uint32(ipnet.Mask[i])<<24 | uint32(ipnet.Mask[i+1])<<16 | uint32(ipnet.Mask[i+2])<<8 | uint32(ipnet.Mask[i+3])
		if mask == 0 {
			continue
		}
		val := uint32(ipnet.IP[i])<<24 | uint32(ipnet.IP[i+1])<<16 | uint32(ipnet.IP[i+2])<<8 | uint32(ipnet.IP[i+3])
		words = append(words, word{off + uint32(i), val & mask, mask})
	}
	if len(words) == 0 { // 0.0.0.0/0 or ::/0
		b.items = append(b.items, item{isJump: true, cond: bpf.JumpEqual, val: 0, t: t, f: t})
		return
	}
	for i, w := range words {
		next := t
		if i < len(words)-1 {
			next = b.newLabel()
		}
		b.emit(bpf.LoadAbsolute{Off: w.off, Size: 4})
		if w.mask != 0xffffffff {
			b.emit(bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: w.mask})
		}
		b.jeq(w.val, next, f)
		if next != t {
			b.mark(next)
		}
	}
}

func (b *builder) resolve() ([]bpf.Instruction, error) {
	pos := map[label]int{}
	n := 0
	for _, it := range b.items {
		if it.mark != 0 {
			pos[it.mark] = n
		} else {
			n++
		}
	}
	out := make([]bpf.Instruction, 0, n)
	for _, it := range b.items {
		switch {
		case it.mark != 0:
		case it.isJump:
			here := len(out) + 1
			skipT, skipF := pos[it.t]-here, pos[it.f]-here
			if skipT < 0 || skipF < 0 || skipT > 255 || skipF > 255 {
				return nil, errors.New("filter expression too long to compile (conditional jump out of range)")
			}
			out = append(out, bpf.JumpIf{Cond: it.cond, Val: it.val, SkipTrue: uint8(skipT), SkipFalse: uint8(skipF)})
		default:
			out = append(out, it.ins)
		}
	}
	return out, nil
}
