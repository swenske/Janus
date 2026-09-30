package pcapfilter

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/bpf"
)

// pkt describes a frame just precisely enough for a filter to classify.
type pkt struct {
	name             string
	arp              bool
	ipv6             bool
	proto            byte
	src, dst         string
	srcPort, dstPort uint16
	fragOffset       uint16 // IPv4 only, in 8-byte units
	ipOptions        int    // IPv4 only, extra header bytes
}

func (p pkt) frame() []byte {
	eth := make([]byte, 14)
	if p.arp {
		binary.BigEndian.PutUint16(eth[12:], etherTypeARP)
		a := make([]byte, 28)
		binary.BigEndian.PutUint16(a[0:], 1)
		binary.BigEndian.PutUint16(a[2:], etherTypeIPv4)
		a[4], a[5] = 6, 4
		copy(a[14:18], net.ParseIP(p.src).To4())
		copy(a[24:28], net.ParseIP(p.dst).To4())
		return append(eth, a...)
	}
	l4 := make([]byte, 20)
	binary.BigEndian.PutUint16(l4[0:], p.srcPort)
	binary.BigEndian.PutUint16(l4[2:], p.dstPort)
	var ip []byte
	if p.ipv6 {
		binary.BigEndian.PutUint16(eth[12:], etherTypeIPv6)
		ip = make([]byte, 40)
		ip[0] = 0x60
		binary.BigEndian.PutUint16(ip[4:], uint16(len(l4)))
		ip[6], ip[7] = p.proto, 64
		copy(ip[8:24], net.ParseIP(p.src).To16())
		copy(ip[24:40], net.ParseIP(p.dst).To16())
	} else {
		binary.BigEndian.PutUint16(eth[12:], etherTypeIPv4)
		hl := 20 + p.ipOptions
		ip = make([]byte, hl)
		ip[0] = 0x40 | byte(hl/4)
		binary.BigEndian.PutUint16(ip[2:], uint16(hl+len(l4)))
		binary.BigEndian.PutUint16(ip[6:], p.fragOffset)
		ip[8], ip[9] = 64, p.proto
		copy(ip[12:16], net.ParseIP(p.src).To4())
		copy(ip[16:20], net.ParseIP(p.dst).To4())
	}
	return append(append(eth, ip...), l4...)
}

var (
	tcpIn8080   = pkt{name: "tcp 10.0.2.2:40000 > 10.0.2.15:8080", proto: protoTCP, src: "10.0.2.2", dst: "10.0.2.15", srcPort: 40000, dstPort: 8080}
	tcpOut8080  = pkt{name: "tcp 10.0.2.15:8080 > 10.0.2.2:40000", proto: protoTCP, src: "10.0.2.15", dst: "10.0.2.2", srcPort: 8080, dstPort: 40000}
	tcpOpts8080 = pkt{name: "tcp to :8080 with IP options", proto: protoTCP, src: "10.0.2.2", dst: "10.0.2.15", srcPort: 40000, dstPort: 8080, ipOptions: 8}
	tcpFrag8080 = pkt{name: "non-first fragment whose payload looks like :8080", proto: protoTCP, src: "10.0.2.2", dst: "10.0.2.15", srcPort: 40000, dstPort: 8080, fragOffset: 185}
	tcp443      = pkt{name: "tcp to :443", proto: protoTCP, src: "10.0.2.2", dst: "10.0.2.15", srcPort: 40001, dstPort: 443}
	tcp22       = pkt{name: "tcp 10.0.2.2 > :22", proto: protoTCP, src: "10.0.2.2", dst: "10.0.2.15", srcPort: 40002, dstPort: 22}
	udp8080     = pkt{name: "udp to :8080", proto: protoUDP, src: "10.0.2.2", dst: "10.0.2.15", srcPort: 40003, dstPort: 8080}
	dnsQuery    = pkt{name: "udp dns query", proto: protoUDP, src: "10.0.2.15", dst: "10.0.2.3", srcPort: 40004, dstPort: 53}
	dnsReply    = pkt{name: "udp dns reply", proto: protoUDP, src: "10.0.2.3", dst: "10.0.2.15", srcPort: 53, dstPort: 40004}
	sctp9000    = pkt{name: "sctp to :9000", proto: protoSCTP, src: "10.0.2.2", dst: "10.0.2.15", srcPort: 9000, dstPort: 9000}
	icmp4       = pkt{name: "icmp", proto: protoICMP, src: "192.168.1.1", dst: "10.0.2.15"}
	otherHosts  = pkt{name: "tcp 192.168.1.1 > 192.168.1.2:80", proto: protoTCP, src: "192.168.1.1", dst: "192.168.1.2", srcPort: 40005, dstPort: 80}
	arpReq      = pkt{name: "arp who-has 10.0.2.2 tell 10.0.2.15", arp: true, src: "10.0.2.15", dst: "10.0.2.2"}
	tcp6In8080  = pkt{name: "tcp6 fd00::2 > fd00::15:8080", ipv6: true, proto: protoTCP, src: "fd00::2", dst: "fd00::15", srcPort: 40006, dstPort: 8080}
	udp6DNS     = pkt{name: "udp6 dns query to 2001:db8::53", ipv6: true, proto: protoUDP, src: "fd00::15", dst: "2001:db8::53", srcPort: 40007, dstPort: 53}
	icmp6       = pkt{name: "icmp6", ipv6: true, proto: protoICMPv6, src: "fe80::1", dst: "fe80::2"}
)

var allPackets = []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22, udp8080, dnsQuery, dnsReply, sctp9000, icmp4, otherHosts, arpReq, tcp6In8080, udp6DNS, icmp6}

// filterCases lists, for each expression, exactly which packets must
// match - every other packet in allPackets must be dropped.
var filterCases = []struct {
	expr  string
	match []pkt
}{
	{"ip", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22, udp8080, dnsQuery, dnsReply, sctp9000, icmp4, otherHosts}},
	{"ip6", []pkt{tcp6In8080, udp6DNS, icmp6}},
	{"arp", []pkt{arpReq}},
	{"tcp", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22, otherHosts, tcp6In8080}},
	{"udp", []pkt{udp8080, dnsQuery, dnsReply, udp6DNS}},
	{"icmp", []pkt{icmp4}},
	{"icmp6", []pkt{icmp6}},
	{"port 8080", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, udp8080, tcp6In8080}},
	{"tcp port 8080", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcp6In8080}},
	{"dst port 8080", []pkt{tcpIn8080, tcpOpts8080, udp8080, tcp6In8080}},
	{"src port 8080", []pkt{tcpOut8080}},
	{"udp and dst port 53", []pkt{dnsQuery, udp6DNS}},
	{"port 9000", []pkt{sctp9000}},
	{"portrange 440-8080", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, udp8080, tcp443, tcp6In8080}},
	{"host 10.0.2.2", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22, udp8080, sctp9000, arpReq}},
	{"src host 10.0.2.2", []pkt{tcpIn8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22, udp8080, sctp9000}},
	{"dst 10.0.2.2", []pkt{tcpOut8080, arpReq}},
	{"ip host 10.0.2.2", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22, udp8080, sctp9000}},
	{"host 10.0.2.2 and not port 22", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcpFrag8080, tcp443, udp8080, sctp9000, arpReq}},
	{"net 10.0.0.0/8", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22, udp8080, dnsQuery, dnsReply, sctp9000, icmp4, arpReq}},
	{"net 192.168.1.0/24", []pkt{icmp4, otherHosts}},
	{"net 0.0.0.0/0", []pkt{tcpIn8080, tcpOut8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22, udp8080, dnsQuery, dnsReply, sctp9000, icmp4, otherHosts, arpReq}},
	{"host fd00::2", []pkt{tcp6In8080}},
	{"net 2001:db8::/32", []pkt{udp6DNS}},
	{"net fe80::/10", []pkt{icmp6}},
	{"not tcp", []pkt{udp8080, dnsQuery, dnsReply, sctp9000, icmp4, arpReq, udp6DNS, icmp6}},
	{"!tcp and !udp", []pkt{sctp9000, icmp4, arpReq, icmp6}},
	{"icmp or port 53", []pkt{icmp4, dnsQuery, dnsReply, udp6DNS}},
	{"tcp and (port 22 or port 443)", []pkt{tcp22, tcp443}},
	{"tcp and port 22 or port 443", []pkt{tcp22, tcp443}},
	{"udp || port 443 && src 10.0.2.2", []pkt{udp8080, tcp443}},
	{"port 53 or tcp and src 10.0.2.2", []pkt{tcpIn8080, tcpOpts8080, tcpFrag8080, tcp443, tcp22}},
	{"not (host 10.0.2.2 or host 10.0.2.15)", []pkt{otherHosts, tcp6In8080, udp6DNS, icmp6}},
}

func run(t *testing.T, prog []bpf.Instruction, p pkt) bool {
	t.Helper()
	vm, err := bpf.NewVM(prog)
	if err != nil {
		t.Fatalf("invalid BPF program: %v", err)
	}
	n, err := vm.Run(p.frame())
	if err != nil {
		t.Fatalf("%s: vm: %v", p.name, err)
	}
	return n != 0
}

func TestCompileSemantics(t *testing.T) {
	for _, tc := range filterCases {
		t.Run(tc.expr, func(t *testing.T) {
			prog, err := Compile(tc.expr, 65535)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			want := map[string]bool{}
			for _, p := range tc.match {
				want[p.name] = true
			}
			for _, p := range allPackets {
				if got := run(t, prog, p); got != want[p.name] {
					t.Errorf("%s: matched=%v, want %v", p.name, got, want[p.name])
				}
			}
		})
	}
}

func TestCompileAcceptValue(t *testing.T) {
	prog, err := Compile("tcp", 1234)
	if err != nil {
		t.Fatal(err)
	}
	vm, _ := bpf.NewVM(prog)
	if n, _ := vm.Run(tcpIn8080.frame()); n != 1234 {
		t.Errorf("accepted packet returned %d, want 1234", n)
	}
}

func TestCompileErrors(t *testing.T) {
	for _, expr := range []string{
		"",
		"   ",
		"tcp[tcpflags] & tcp-syn != 0",
		"host example.com",
		"port http",
		"net 10.0.0.1",
		"portrange 80",
		"icmp port 80",
		"arp port 80",
		"tcp and",
		"(tcp or udp",
		"tcp udp",
		"ether host 00:11:22:33:44:55",
		"src",
		"vlan 10",
	} {
		if _, err := Compile(expr, 65535); err == nil {
			t.Errorf("Compile(%q) succeeded, want an error", expr)
		}
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("!(tcp or udp) and !port 22")
	want := []string{"!", "(", "tcp", "or", "udp", ")", "and", "!", "port", "22"}
	if len(got) != len(want) {
		t.Fatalf("tokenize = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokenize = %q, want %q", got, want)
		}
	}
}

// TestAgainstTcpdump checks every filterCases expression against real
// tcpdump/libpcap on the same frames, when tcpdump is installed - the
// reference implementation, not just this package's own expectations.
func TestAgainstTcpdump(t *testing.T) {
	tcpdump, err := exec.LookPath("tcpdump")
	if err != nil {
		t.Skip("tcpdump not installed")
	}
	file := filepath.Join(t.TempDir(), "all.pcap")
	var buf bytes.Buffer
	hdr := make([]byte, 24)
	binary.LittleEndian.PutUint32(hdr[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(hdr[4:], 2)
	binary.LittleEndian.PutUint16(hdr[6:], 4)
	binary.LittleEndian.PutUint32(hdr[16:], 65535)
	binary.LittleEndian.PutUint32(hdr[20:], 1)
	buf.Write(hdr)
	for i, p := range allPackets {
		f := p.frame()
		rec := make([]byte, 16)
		binary.LittleEndian.PutUint32(rec[0:], uint32(i+1))
		binary.LittleEndian.PutUint32(rec[8:], uint32(len(f)))
		binary.LittleEndian.PutUint32(rec[12:], uint32(len(f)))
		buf.Write(rec)
		buf.Write(f)
	}
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range filterCases {
		out, err := exec.Command(tcpdump, "-nn", "-tt", "-r", file, tc.expr).CombinedOutput()
		if err != nil {
			t.Errorf("tcpdump %q: %v\n%s", tc.expr, err, out)
			continue
		}
		got := map[int]bool{}
		for _, line := range strings.Split(string(out), "\n") {
			// -tt prints the epoch timestamp first; each frame's
			// timestamp is its 1-based index in allPackets.
			sec, _, _ := strings.Cut(strings.Fields(line + " x")[0], ".")
			if n, err := strconv.Atoi(sec); err == nil {
				got[n-1] = true
			}
		}
		want := map[string]bool{}
		for _, p := range tc.match {
			want[p.name] = true
		}
		for i, p := range allPackets {
			if got[i] != want[p.name] {
				t.Errorf("%q, %s: tcpdump matched=%v, this package's expectation=%v", tc.expr, p.name, got[i], want[p.name])
			}
		}
	}
}
