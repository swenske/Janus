package nodeproxy

import (
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// A real move (lgslbpub01, 2026-10-02): the node's "applying" message was
// lost with its old address, so nothing told the Controller the trial
// had started. Confirming must still find the node on its new address.
func TestConfirmLoopFindsTheNewAddress(t *testing.T) {
	res := &networkApplyResult{
		Candidates:   []string{"172.16.1.150:9505", "172.16.1.151:9505"},
		RevertAtUnix: time.Now().Add(10 * time.Second).Unix(),
	}
	var tried []string
	ep, ok := confirmLoop(res, func(ep string) (string, error) {
		tried = append(tried, ep)
		if ep == "172.16.1.150:9505" {
			return "", status.Error(codes.Unavailable, "no route to host")
		}
		return "172.16.1.151", nil
	})
	if !ok || ep != "172.16.1.151:9505" || !res.Confirmed || res.ConfirmedVia != "172.16.1.151" || res.Error != "" {
		t.Fatalf("got %q %v %+v", ep, ok, res)
	}
	if strings.Join(tried, ",") != "172.16.1.150:9505,172.16.1.151:9505" {
		t.Errorf("tried %v", tried)
	}
}

// The request never reached the node - a connection from before it
// rebooted, also seen there: the node says nothing is on trial, and the
// Controller says so at once instead of waiting out the window.
func TestConfirmLoopStopsWhenNothingIsOnTrial(t *testing.T) {
	res := &networkApplyResult{
		Candidates:   []string{"172.16.1.150:9505", "172.16.1.151:9505"},
		RevertAtUnix: time.Now().Add(30 * time.Second).Unix(),
	}
	calls := 0
	start := time.Now()
	_, ok := confirmLoop(res, func(string) (string, error) {
		calls++
		return "", status.Error(codes.FailedPrecondition, "no network configuration is on trial")
	})
	if ok || res.Confirmed || calls != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("ok=%v calls=%d after %v: %+v", ok, calls, time.Since(start), res)
	}
	if !strings.Contains(res.Error, "nothing awaiting confirmation") || !strings.Contains(res.Error, "apply it again") {
		t.Errorf("error %q", res.Error)
	}
}

func TestConfirmLoopGivesUpBeforeTheRevert(t *testing.T) {
	res := &networkApplyResult{
		Candidates:   []string{"192.0.2.1:9505"},
		RevertAtUnix: time.Now().Add(2 * time.Second).Unix(),
	}
	_, ok := confirmLoop(res, func(string) (string, error) {
		return "", status.Error(codes.Unavailable, "no route to host")
	})
	if ok || !strings.Contains(res.Error, "no route to host") || !strings.Contains(res.Error, "goes back to its previous configuration") {
		t.Fatalf("ok=%v %+v", ok, res)
	}
}

func TestConnectionLost(t *testing.T) {
	for code, want := range map[codes.Code]bool{
		codes.Unavailable:        true,
		codes.DeadlineExceeded:   true,
		codes.Canceled:           true,
		codes.InvalidArgument:    false,
		codes.FailedPrecondition: false,
		codes.Internal:           false,
	} {
		if got := connectionLost(status.Error(code, "x")); got != want {
			t.Errorf("%v: %v", code, got)
		}
	}
}

// silentApplyStream is a NetworkConfigApply stream: its messages, then - silent
// - nothing until the call is cancelled (a connection to an address the
// node dropped), or the end.
type silentApplyStream struct {
	msgs     []*janusv1alpha1.NetworkConfigApplyResponse
	silent   bool
	canceled chan struct{}
}

func (s *silentApplyStream) Recv() (*janusv1alpha1.NetworkConfigApplyResponse, error) {
	if len(s.msgs) > 0 {
		m := s.msgs[0]
		s.msgs = s.msgs[1:]
		return m, nil
	}
	if !s.silent {
		return nil, io.EOF
	}
	<-s.canceled
	return nil, status.Error(codes.Canceled, "context canceled")
}

// A move: the change takes the address the call came in on, and the
// stream goes silent with it - its first messages lost too
// (controller-libvirt-test's part 4e: none arrived) -: given up shortly
// after the call's start, not at its 30 s deadline (34 s per move on
// v2026.11.17's lab).
func TestReadApplyGivesUpASilentStream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		msgs  []*janusv1alpha1.NetworkConfigApplyResponse
		stage string
	}{
		{"nothing arrived", nil, "checking whether the node applied it"},
		{"after applying", []*janusv1alpha1.NetworkConfigApplyResponse{{Stage: "validating"}, {Stage: "applying", RevertAtUnix: 1234}}, "connection lost while applying"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &silentApplyStream{silent: true, canceled: make(chan struct{}), msgs: tc.msgs}
			res := &networkApplyResult{}
			start := time.Now()
			lost, err := readApply(s, func() { close(s.canceled) }, 50*time.Millisecond, res)
			if err != nil || !lost {
				t.Fatalf("lost %v, err %v", lost, err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("gave up after %s", d)
			}
			if !strings.Contains(strings.Join(res.Stages, "\n"), tc.stage) {
				t.Errorf("stages %q", res.Stages)
			}
		})
	}
}

// A change that keeps the address: the node's last message, its
// addresses, read to the end - the grace never cuts it.
func TestReadApplyReadsTheLastMessage(t *testing.T) {
	cancelled := false
	s := &silentApplyStream{msgs: []*janusv1alpha1.NetworkConfigApplyResponse{
		{Stage: "validating"},
		{Stage: "applying", RevertAtUnix: 1234},
		{Stage: "awaiting-confirmation", Addresses: []string{"192.0.2.10/24"}, RevertAtUnix: 1235},
	}}
	res := &networkApplyResult{}
	lost, err := readApply(s, func() { cancelled = true }, 0, res)
	if err != nil || lost || cancelled {
		t.Fatalf("lost %v, err %v, cancelled %v", lost, err, cancelled)
	}
	if res.RevertAtUnix != 1235 || len(res.Addresses) != 1 {
		t.Errorf("result %+v", res)
	}
}

// The node refuses the configuration: its error, not a lost connection.
func TestReadApplyRefused(t *testing.T) {
	s := &refusingStream{}
	if lost, err := readApply(s, func() {}, 0, &networkApplyResult{}); lost || status.Code(err) != codes.InvalidArgument {
		t.Errorf("lost %v, err %v", lost, err)
	}
}

type refusingStream struct{ sent bool }

func (s *refusingStream) Recv() (*janusv1alpha1.NetworkConfigApplyResponse, error) {
	if !s.sent {
		s.sent = true
		return &janusv1alpha1.NetworkConfigApplyResponse{Stage: "validating"}, nil
	}
	return nil, status.Error(codes.InvalidArgument, "interfaces[0]: no such MAC")
}
