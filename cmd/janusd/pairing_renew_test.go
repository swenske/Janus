package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/internal/api"
	"github.com/swenske/Janus/internal/pairing"
	"github.com/swenske/Janus/internal/pki"
)

// TestPairingWindowRenews: an open pairing window gets a new certificate
// and code every pairingRenewal - a client holding the previous code no
// longer trusts the window (its 59 bits name the old certificate), the
// previous secret no longer opens it, the new code does (security
// review 2026-11-05 #28).
func TestPairingWindowRenews(t *testing.T) {
	ca, err := pki.NewCA("node")
	if err != nil {
		t.Fatal(err)
	}
	local := pki.NewLocal(ca)
	open := func(every time.Duration) *pairingWindow {
		t.Helper()
		fleet, err := pki.OpenFleet(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		prev := pairingRenewal
		pairingRenewal = every
		w := openPairing("127.0.0.1:0", local, &api.Access{Local: local, Fleet: fleet}, fleet, "node", time.Time{}, nil, nil)
		pairingRenewal = prev
		if w == nil {
			t.Fatal("no window")
		}
		t.Cleanup(func() { w.window.Close("the test is over") })
		return w
	}

	// By itself, every pairingRenewal.
	w := open(200 * time.Millisecond)
	_, first := w.current()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, c := w.current(); c != first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the window was never renewed")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// What a renewal does, on a window that holds still otherwise.
	w = open(time.Hour)
	_, before := w.current()
	w.renew(local, "node")
	_, after := w.current()
	if after == before {
		t.Fatal("renew changed nothing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pair := func(code pairing.Code) error {
		_, _, err := pairing.Pair(ctx, w.listening.String(), code.String(), []byte("not a root"), nil)
		return err
	}
	if err := pair(before); err == nil || !strings.Contains(err.Error(), "isn't the one its code names") {
		t.Errorf("the previous code: %v", err)
	}
	if err := pair(pairing.Code{Prefix: after.Prefix, Secret: before.Secret}); err == nil || strings.Contains(err.Error(), "code names") || status.Code(err) == codes.InvalidArgument {
		t.Errorf("the previous secret on the new certificate: %v", err)
	}
	// The new code: past the certificate and the secret, refused only for
	// the fleet it brings.
	if err := pair(after); status.Code(err) != codes.InvalidArgument {
		t.Errorf("the new code: %v", err)
	}
}
