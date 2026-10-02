package nodeproxy

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
