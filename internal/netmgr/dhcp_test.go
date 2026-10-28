package netmgr

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"golang.org/x/net/bpf"
)

func TestLeaseFromACK(t *testing.T) {
	hw, _ := net.ParseMAC("52:54:00:00:00:01")
	offer, err := dhcpv4.New(dhcpv4.WithHwAddr(hw), dhcpv4.WithYourIP(net.IPv4(10, 0, 2, 15)),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.IPv4(10, 0, 2, 2))))
	if err != nil {
		t.Fatal(err)
	}
	ack, err := dhcpv4.New(dhcpv4.WithHwAddr(hw), dhcpv4.WithYourIP(net.IPv4(10, 0, 2, 15)),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.IPv4(10, 0, 2, 2))),
		dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))),
		dhcpv4.WithOption(dhcpv4.OptRouter(net.IPv4(0, 0, 0, 0), net.IPv4(10, 0, 2, 2))),
		dhcpv4.WithOption(dhcpv4.OptDNS(net.IPv4(10, 0, 2, 3), net.IPv4(0, 0, 0, 0))),
		dhcpv4.WithOption(dhcpv4.OptNTPServers(net.IPv4(10, 0, 2, 4))),
		dhcpv4.WithOption(dhcpv4.OptDomainName("example.lan")),
		dhcpv4.WithOption(dhcpv4.OptHostName("lb1")),
		dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(2*time.Minute)),
		dhcpv4.WithOption(dhcpv4.Option{Code: dhcpv4.OptionInterfaceMTU, Value: dhcpv4.OptionGeneric{Data: []byte{0x05, 0xdc}}}))
	if err != nil {
		t.Fatal(err)
	}
	now := 42 * time.Second // since the boot
	l, err := leaseFromACK("eth0", "52:54:00:00:00:01", offer, ack, now)
	if err != nil {
		t.Fatal(err)
	}
	want := Lease{Interface: "eth0", MAC: "52:54:00:00:00:01", Address: "10.0.2.15/24", Router: "10.0.2.2", Server: "10.0.2.2",
		DNS: []string{"10.0.2.3"}, NTP: []string{"10.0.2.4"}, Domain: "example.lan", Hostname: "lb1", MTU: 1500,
		Obtained: now, Renew: now + 60*time.Second, Rebind: now + 105*time.Second, Expires: now + 120*time.Second}
	got := *l
	got.Offer, got.ACK = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lease:\n got %+v\nwant %+v", got, want)
	}
	// What a renewal is built from survives the round trip.
	if a, err := dhcpv4.FromBytes(l.ACK); err != nil || !a.YourIPAddr.Equal(net.IPv4(10, 0, 2, 15)) {
		t.Errorf("the ACK kept: %v %v", a, err)
	}
	if l.kernel() || l.expired(now) || !l.expired(now+2*time.Minute) {
		t.Error("expiry")
	}

	// No address, no lease.
	nak, _ := dhcpv4.New(dhcpv4.WithHwAddr(hw), dhcpv4.WithMessageType(dhcpv4.MessageTypeAck))
	if _, err := leaseFromACK("eth0", "52:54:00:00:00:01", offer, nak, now); err == nil {
		t.Error("an acknowledgement without an address must be refused")
	}
}

func TestLeaseFiles(t *testing.T) {
	RunDir = t.TempDir()
	now := time.Hour
	live := &Lease{Interface: "eth0", MAC: "52:54:00:00:00:01", Address: "10.0.2.15/24", Expires: now + time.Hour}
	old := &Lease{Interface: "eth1", MAC: "52:54:00:00:00:02", Address: "10.0.3.15/24", Expires: now - time.Second}
	for _, l := range []*Lease{live, old} {
		if err := saveLease(l); err != nil {
			t.Fatal(err)
		}
	}
	got := loadLeases(now)
	if len(got) != 1 || got[live.MAC] == nil || got[live.MAC].Address != live.Address {
		t.Fatalf("loadLeases = %v, want eth0's alone (eth1's expired)", got)
	}
	forgetLease(live.MAC)
	if got := loadLeases(now); len(got) != 0 {
		t.Errorf("forgotten: %v", got)
	}
}

// scriptedTransport answers from a script; the client's clock is fake,
// moved by its sleeps.
type scriptedTransport struct {
	mu        sync.Mutex
	clock     *time.Duration
	acquire_  []error // nil: a lease
	renew_    []error
	addr      string
	requested []netip.Addr
	calls     []string
}

func (s *scriptedTransport) next(list *[]error) error {
	if len(*list) == 0 {
		return errors.New("script over")
	}
	err := (*list)[0]
	*list = (*list)[1:]
	return err
}

func (s *scriptedTransport) lease() *Lease {
	now := *s.clock
	return &Lease{Interface: "eth0", MAC: "m", Address: s.addr, Obtained: now,
		Renew: now + time.Minute, Rebind: now + 105*time.Second, Expires: now + 2*time.Minute}
}

func (s *scriptedTransport) acquire(_ context.Context, requested netip.Addr) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "acquire")
	s.requested = append(s.requested, requested)
	if err := s.next(&s.acquire_); err != nil {
		return nil, err
	}
	return s.lease(), nil
}

func (s *scriptedTransport) renew(_ context.Context, _ *Lease) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "renew")
	if err := s.next(&s.renew_); err != nil {
		return nil, err
	}
	return s.lease(), nil
}

func TestDHCPClient(t *testing.T) {
	var clock time.Duration
	noAnswer := errors.New("no answer")
	st := &scriptedTransport{clock: &clock, addr: "10.0.2.15/24",
		acquire_: []error{noAnswer, noAnswer, nil, nil},
		// Renewals: one lost, one through; then none answers until the
		// lease expires; after the next lease, a refusal.
		renew_: []error{noAnswer, nil, noAnswer, noAnswer, noAnswer, noAnswer, noAnswer, errNAK},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	c := &dhcpClient{
		mac: "m", t: st,
		held:  &Lease{MAC: "m", Address: "10.0.2.99/24"}, // the kernel's: asked for again
		bound: func(l *Lease) { events = append(events, "bound@"+clock.String()) },
		lost: func(reason string) {
			events = append(events, reason+"@"+clock.String())
		},
		now: func() time.Duration { return clock },
		sleep: func(ctx context.Context, d time.Duration) bool {
			if len(events) >= 5 {
				cancel()
				return false
			}
			if d > 0 {
				clock += d
			}
			return ctx.Err() == nil
		},
	}
	c.run(ctx)
	// Two failed attempts (1 s, 2 s apart), the lease; renewal at T1
	// (60 s) fails, again 30 s later (half what's left) and through; the
	// next ones fail - retried at half what's left, 5 s at least - until
	// the lease runs out (2 min after the renewal), then a new lease at
	// once; its renewal refused.
	want := "bound@3s bound@1m33s expired@3m33s bound@3m33s refused@4m33s"
	if got := strings.Join(events, " "); got != want {
		t.Errorf("events:\n got %s\nwant %s\ncalls %v", got, want, st.calls)
	}
	if st.requested[0] != netip.MustParseAddr("10.0.2.99") || st.requested[3] != netip.MustParseAddr("10.0.2.15") {
		t.Errorf("requested addresses: %v", st.requested)
	}
}

func TestDHCPClientAdoptsALiveLease(t *testing.T) {
	var clock time.Duration
	st := &scriptedTransport{clock: &clock, addr: "10.0.2.15/24", renew_: []error{nil}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	held := &Lease{MAC: "m", Address: "10.0.2.15/24", Renew: clock + 10*time.Second, Expires: clock + time.Minute}
	c := &dhcpClient{
		mac: "m", t: st, held: held,
		bound: func(*Lease) { calls = append(calls, "bound@"+clock.String()) },
		lost:  func(string) { calls = append(calls, "lost") },
		now:   func() time.Duration { return clock },
		sleep: func(ctx context.Context, d time.Duration) bool {
			if len(calls) > 0 {
				cancel()
				return false
			}
			clock += d
			return true
		},
	}
	c.run(ctx)
	// No new lease asked: renewed at its T1.
	if strings.Join(calls, " ") != "bound@10s" || strings.Join(st.calls, " ") != "renew" {
		t.Errorf("calls %v, transport %v", calls, st.calls)
	}
}

func TestDHCPFilter(t *testing.T) {
	vm, err := bpf.NewVM(rawToInstructions(t, dhcpFilter))
	if err != nil {
		t.Fatal(err)
	}
	// IPv4 headers (20 bytes, or 24 with options), then UDP.
	pkt := func(proto byte, frag uint16, ihl int, dport uint16) []byte {
		b := make([]byte, ihl*4+8)
		b[0] = 0x40 | byte(ihl)
		b[6], b[7] = byte(frag>>8), byte(frag)
		b[9] = proto
		b[ihl*4+2], b[ihl*4+3] = byte(dport>>8), byte(dport)
		return b
	}
	for _, c := range []struct {
		name string
		pkt  []byte
		keep bool
	}{
		{"DHCP answer", pkt(17, 0, 5, 68), true},
		{"DHCP answer, IP options", pkt(17, 0, 6, 68), true},
		{"DF set", pkt(17, 0x4000, 5, 68), true},
		{"DNS", pkt(17, 0, 5, 53), false},
		{"TCP to 68", pkt(6, 0, 5, 68), false},
		{"a fragment's tail", pkt(17, 0x0010, 5, 68), false},
		{"to the server's port", pkt(17, 0, 5, 67), false},
	} {
		n, err := vm.Run(c.pkt)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (n > 0) != c.keep {
			t.Errorf("%s: kept %d bytes, want kept=%v", c.name, n, c.keep)
		}
	}
}

func rawToInstructions(t *testing.T, raw []bpf.RawInstruction) []bpf.Instruction {
	t.Helper()
	out, ok := bpf.Disassemble(raw)
	if !ok {
		t.Fatal("the filter doesn't disassemble")
	}
	return out
}
