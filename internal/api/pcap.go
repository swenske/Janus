package api

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pcapfilter"
)

const (
	pcapDefaultSnapLen = 65535
	pcapMaxSnapLen     = 262144
	pcapLinkTypeEther  = 1 // LINKTYPE_ETHERNET

	// Captured bytes are batched into Data messages rather than sent
	// one packet per message; a batch goes out once it's this big or
	// this old, whichever comes first.
	pcapFlushBytes    = 32 * 1024
	pcapFlushInterval = 200 * time.Millisecond
)

// PacketCapture is a tcpdump-equivalent with no tcpdump on the node: an
// AF_PACKET socket bound to one interface, the request's filter
// compiled to classic BPF here and attached in the kernel, and the
// result streamed back as a standard pcap file.
func (s *System) PacketCapture(req *janusv1alpha1.PacketCaptureRequest, stream janusv1alpha1.SystemService_PacketCaptureServer) error {
	if req.GetInterface() == "" {
		return status.Error(codes.InvalidArgument, "interface is required")
	}
	iface, err := net.InterfaceByName(req.GetInterface())
	if err != nil {
		return status.Errorf(codes.NotFound, "interface %q: %v", req.GetInterface(), err)
	}
	// AF_PACKET/SOCK_RAW hands back each frame with its link-layer
	// header as-is: Ethernet for real NICs, and a zeroed Ethernet header
	// for loopback. Anything else (tun, wireguard) would need a
	// different pcap link type and filter offsets - refused rather than
	// producing a file Wireshark misparses.
	if iface.Flags&net.FlagLoopback == 0 && len(iface.HardwareAddr) != 6 {
		return status.Errorf(codes.FailedPrecondition, "interface %q has no Ethernet framing; only Ethernet and loopback interfaces are supported", iface.Name)
	}

	snapLen := req.GetSnapLen()
	if snapLen == 0 {
		snapLen = pcapDefaultSnapLen
	}
	if snapLen > pcapMaxSnapLen {
		return status.Errorf(codes.InvalidArgument, "snap_len %d exceeds the %d-byte maximum", snapLen, pcapMaxSnapLen)
	}

	var exclude []pcapfilter.TCPConn
	if !req.GetIncludeOwnStream() {
		if conn, ok := ownStream(stream.Context()); ok {
			exclude = append(exclude, conn)
		}
	}
	prog, err := compilePcapFilter(req.GetBpfFilter(), exclude...)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "bpf_filter %q: %v", req.GetBpfFilter(), err)
	}

	fd, err := openCaptureSocket(iface.Index, prog, req.GetPromiscuous())
	if err != nil {
		return status.Errorf(codes.Internal, "open capture socket on %s: %v", iface.Name, err)
	}
	defer unix.Close(fd)

	buf := pcapGlobalHeader(snapLen, pcapLinkTypeEther)
	lastFlush := time.Now()
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		err := stream.Send(&janusv1alpha1.Data{Bytes: buf})
		buf = make([]byte, 0, pcapFlushBytes+int(snapLen)+16)
		lastFlush = time.Now()
		return err
	}

	var deadline time.Time
	if d := req.GetDurationSeconds(); d > 0 {
		deadline = time.Now().Add(time.Duration(d) * time.Second)
	}

	pkt := make([]byte, snapLen)
	ctx := stream.Context()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			_ = flush()
			return nil
		}
		// MSG_TRUNC makes recvfrom return the packet's real on-wire
		// length even when only snapLen bytes fit in pkt.
		n, _, err := unix.Recvfrom(fd, pkt, unix.MSG_TRUNC)
		switch {
		case err == nil:
			captured := min(n, int(snapLen))
			buf = appendPcapRecord(buf, time.Now(), pkt[:captured], n)
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR):
		default:
			return status.Errorf(codes.Internal, "read from %s: %v", iface.Name, err)
		}
		if len(buf) >= pcapFlushBytes || time.Since(lastFlush) >= pcapFlushInterval {
			if err := flush(); err != nil {
				return nil // client went away
			}
		}
	}
}

// openCaptureSocket creates the AF_PACKET socket with protocol 0 - which
// receives nothing at all - attaches the filter, and only then binds
// with ETH_P_ALL. Doing it in that order means no packet is ever
// queued before the filter applies, with no drain step needed.
func openCaptureSocket(ifindex int, prog []unix.SockFilter, promiscuous bool) (int, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	fail := func(what string, err error) (int, error) {
		unix.Close(fd)
		return -1, fmt.Errorf("%s: %w", what, err)
	}
	if len(prog) > 0 {
		fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
		if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &fprog); err != nil {
			return fail("attach filter", err)
		}
	}
	// A receive timeout keeps the read loop checking for cancellation
	// and flushing partial batches on a quiet interface.
	tv := unix.NsecToTimeval(pcapFlushInterval.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fail("set receive timeout", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: ifindex}); err != nil {
		return fail("bind", err)
	}
	if promiscuous {
		// Membership is tied to this socket: the kernel drops promiscuous
		// mode on its own once the socket closes.
		mreq := unix.PacketMreq{Ifindex: int32(ifindex), Type: unix.PACKET_MR_PROMISC}
		if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mreq); err != nil {
			return fail("enable promiscuous mode", err)
		}
	}
	return fd, nil
}

// ownStream is the TCP connection carrying this RPC, as the node sees it
// - so behind the Controller or a NAT it's still exactly this stream.
func ownStream(ctx context.Context) (pcapfilter.TCPConn, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return pcapfilter.TCPConn{}, false
	}
	remote, rok := p.Addr.(*net.TCPAddr)
	local, lok := p.LocalAddr.(*net.TCPAddr)
	if !rok || !lok {
		return pcapfilter.TCPConn{}, false
	}
	return pcapfilter.TCPConn{A: unmapped(remote.AddrPort()), B: unmapped(local.AddrPort())}, true
}

// unmapped turns ::ffff:a.b.c.d back into a.b.c.d - net.TCPAddr keeps
// IPv4 addresses in 16-byte form.
func unmapped(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// compilePcapFilter turns a tcpdump-style expression (see
// internal/pcapfilter for the supported subset), minus any excluded
// connections, into the classic BPF program SO_ATTACH_FILTER takes. An
// empty expression with nothing to exclude means no filter at all.
func compilePcapFilter(expr string, exclude ...pcapfilter.TCPConn) ([]unix.SockFilter, error) {
	if expr == "" && len(exclude) == 0 {
		return nil, nil
	}
	ins, err := pcapfilter.Compile(expr, pcapMaxSnapLen, exclude...)
	if err != nil {
		return nil, err
	}
	raw, err := bpf.Assemble(ins)
	if err != nil {
		return nil, fmt.Errorf("assemble: %w", err)
	}
	prog := make([]unix.SockFilter, len(raw))
	for i, r := range raw {
		prog[i] = unix.SockFilter{Code: r.Op, Jt: r.Jt, Jf: r.Jf, K: r.K}
	}
	return prog, nil
}

// pcapGlobalHeader is the classic libpcap file header (little-endian,
// version 2.4, microsecond timestamps).
func pcapGlobalHeader(snapLen uint32, linkType uint32) []byte {
	h := make([]byte, 24, 24+pcapFlushBytes)
	binary.LittleEndian.PutUint32(h[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(h[4:], 2)
	binary.LittleEndian.PutUint16(h[6:], 4)
	binary.LittleEndian.PutUint32(h[16:], snapLen)
	binary.LittleEndian.PutUint32(h[20:], linkType)
	return h
}

func appendPcapRecord(buf []byte, ts time.Time, data []byte, origLen int) []byte {
	var h [16]byte
	binary.LittleEndian.PutUint32(h[0:], uint32(ts.Unix()))
	binary.LittleEndian.PutUint32(h[4:], uint32(ts.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(h[8:], uint32(len(data)))
	binary.LittleEndian.PutUint32(h[12:], uint32(origLen))
	return append(append(buf, h[:]...), data...)
}

func htons(v uint16) uint16 {
	return v<<8 | v>>8
}
