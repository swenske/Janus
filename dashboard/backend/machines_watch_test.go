package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

func TestRegistrationWarning(t *testing.T) {
	for _, tc := range []struct {
		line, warning, hint string
	}{
		{"2026/10/03 11:52:32 selfregister: reach the Controller at ctl.example.net:8443: dial udp 10.1.0.123:8443: connect: network is unreachable - retrying in 5s",
			"The node can't register: reach the Controller at ctl.example.net:8443: dial udp 10.1.0.123:8443: connect: network is unreachable.", "no route to the Controller"},
		{"selfregister: reach the Controller at ctl.example.net:8443: dial udp: lookup ctl.example.net on 127.0.0.1:53: read udp 127.0.0.1:41000->127.0.0.1:53: read: connection refused - retrying in 1m20s",
			"lookup ctl.example.net", "Its DNS doesn't resolve"},
		{"selfregister: registration failed: Post \"https://10.0.2.2:8443/register\": dial tcp 10.0.2.2:8443: connect: connection refused - retrying in 10s",
			"connection refused.", "Nothing listens there"},
		{"selfregister: registration failed: Post \"https://10.0.2.2:8443/register\": tls: failed to verify certificate: x509: certificate is valid for 10.0.0.1, not 10.0.2.2 - retrying in 2m0s",
			"x509", "doesn't name that address"},
		{"selfregister: registration failed: Post \"https://10.0.2.2:8443/register\": context deadline exceeded (Client.Timeout exceeded while awaiting headers) - retrying in 40s",
			"Client.Timeout", "a firewall"},
		// Images up to v2026.10.03-2 try once a boot.
		{"selfregister: registration failed, will retry on next boot: POST https://10.0.2.2:8443/register: Post \"https://10.0.2.2:8443/register\": dial tcp 10.0.2.2:8443: connect: connection refused",
			"connection refused.", "reset the machine"},
		{"selfregister: determine address to advertise to Controller at ctl:8443: dial udp 10.1.0.123:8443: connect: network is unreachable",
			"reach the Controller at ctl:8443", "reset the machine"},
		{"selfregister: successfully announced to Controller at ctl:8443, awaiting approval", "not on its token", "approving it links it"},
	} {
		w, said := registrationWarning("\x1b[0m" + tc.line)
		if !strings.Contains(w, tc.warning) || !strings.Contains(w, tc.hint) || said == "" {
			t.Errorf("%q:\n got %q (%q)\nwant %q ... %q", tc.line, w, said, tc.warning, tc.hint)
		}
		if strings.Contains(w, "retrying in") {
			t.Errorf("%q: the retry delay stayed in %q", tc.line, w)
		}
	}
	for _, line := range []string{
		"selfregister: announcing to Controller at ctl:8443 as node1 (10.0.0.5:9505)",
		"selfregister: admitted by Controller at ctl:8443",
		"janusd: listening on :9505",
	} {
		if w, _ := registrationWarning(line); w != "" {
			t.Errorf("%q gave a warning: %q", line, w)
		}
	}
}

func TestConsoleLines(t *testing.T) {
	var got []string
	c := &consoleLines{line: func(l string) { got = append(got, l) }}
	for _, chunk := range []string{"\x1b[1mfirst\x1b[0m li", "ne\r\nsecond\n", "third, unfinished"} {
		_, _ = c.Write([]byte(chunk))
	}
	if strings.Join(got, "|") != "first line|second" {
		t.Errorf("lines: %q", got)
	}
}

// Every reader gets the console - with private keys hidden - and the
// hypervisor's console is opened once, then closed after the last one.
func TestConsoleHub(t *testing.T) {
	var h consoleHub
	opened, closed := 0, make(chan struct{})
	write, wrote := make(chan string), make(chan struct{})
	open := func(ctx context.Context, w io.Writer) error {
		opened++
		for {
			select {
			case s := <-write:
				_, _ = w.Write([]byte(s))
				wrote <- struct{}{}
			case <-ctx.Done():
				close(closed)
				return nil
			}
		}
	}
	var a, b syncBuffer
	subA := h.subscribe("m1", open, &a)
	subB := h.subscribe("m1", open, &b)
	for _, line := range []string{"boot\n-----BEGIN EC PRIVATE KEY-----\nsecret\n-----END EC PRIVATE KEY-----\n", "login:\n"} {
		write <- line
		<-wrote
	}
	h.unsubscribe(subA)
	write <- "after A left\n"
	<-wrote
	h.unsubscribe(subB)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the console stayed open after its last reader left")
	}
	if opened != 1 {
		t.Errorf("opened %d consoles, want 1", opened)
	}
	for name, buf := range map[string]*syncBuffer{"A": &a, "B": &b} {
		if s := buf.String(); strings.Contains(s, "secret") || !strings.Contains(s, "boot") || !strings.Contains(s, "login:") {
			t.Errorf("reader %s got %q", name, s)
		}
	}
	if strings.Contains(a.String(), "after A left") || !strings.Contains(b.String(), "after A left") {
		t.Errorf("after A left: A %q, B %q", a.String(), b.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A machine waiting for its node shows why the node can't register,
// read from its console - which a page can show at the same time - and
// the warning goes once the node is admitted.
func TestMachineWarning(t *testing.T) {
	a, fake := newTestApp(t)
	prevRetry, prevTick := watchRetry, watchTick
	watchRetry, watchTick = 100*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { watchRetry, watchTick = prevRetry, prevTick })
	fake.console = "booting\nselfregister: reach the Controller at ctl:8443: dial udp 10.1.0.123:8443: connect: network is unreachable - retrying in 5s\n"
	h := addTrustedHypervisor(t, a)
	m := &machines.Machine{
		Spec:  machines.Spec{Name: "node1", HypervisorID: h.ID, VCPUs: 1, MemoryMiB: 1024, NICs: []machines.NIC{{Network: "lab-mgmt", Name: "eth0", Mode: "dhcp"}}},
		Phase: machines.PhaseRegistering,
		Ref:   &hypervisor.MachineRef{UUID: "uuid-1", Name: "janus-node1"},
	}
	if err := a.machines.Add(m); err != nil {
		t.Fatal(err)
	}
	a.runner.watchNode(m.ID)
	a.runner.watchNode(m.ID) // one watcher a machine

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := a.machines.Get(m.ID)
		if strings.Contains(got.Warning, "network is unreachable") && strings.Contains(got.Warning, "no route to the Controller") {
			if last := got.Events[len(got.Events)-1].Message; !strings.HasPrefix(last, "the node says: reach the Controller at ctl:8443") {
				t.Errorf("history: %q", last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no warning: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A page reads the same console meanwhile.
	page := &syncBuffer{}
	open, err := a.machineConsole(m)
	if err != nil {
		t.Fatal(err)
	}
	sub := a.consoles.subscribe(m.ID, open, page)
	a.consoles.unsubscribe(sub)
	fake.mu.Lock()
	opened := fake.consoles
	fake.mu.Unlock()
	if opened != 1 {
		t.Errorf("%d consoles opened for the watcher and a page, want 1", opened)
	}

	a.runner.registered(m.ID, &store.Node{ID: "n1", Address: "10.0.0.5:9505"})
	got, _ := a.machines.Get(m.ID)
	if got.Warning != "" || got.Phase != machines.PhaseReady {
		t.Errorf("admitted: warning %q, phase %s", got.Warning, got.Phase)
	}
	// Its watcher lets go of the console.
	deadline = time.Now().Add(10 * time.Second)
	for {
		a.runner.mu.Lock()
		watching := a.runner.watching[m.ID]
		a.runner.mu.Unlock()
		if !watching {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still watching an admitted machine's console")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
