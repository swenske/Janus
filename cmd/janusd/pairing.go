package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/api"
	"github.com/swenske/Janus/internal/pairing"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/selfregister"
)

// pairingMarker, in -pki-dir, says the node's PKI was made by a janusd
// that pairs: a node whose admin credential (admin.crt) a first boot
// printed without it is from before the code, used that way - its own
// credential lets in there, as before, and it opens no window.
const pairingMarker = "pairing"

// markPairing marks a PKI made this boot (pairingMarker).
func markPairing(pkiDir string) {
	if err := os.WriteFile(filepath.Join(pkiDir, pairingMarker), []byte("this PKI was made by a janusd that pairs (internal/pairing)\n"), 0o600); err != nil {
		log.Printf("pairing: %v", err)
	}
}

// pairingWanted reports whether this node opens its pairing window
// (internal/pairing): it trusts no fleet, no Controller was provisioned
// - registering, or registered -, and it isn't a node from before the
// code (pairingMarker). A fleet that couldn't be read leaves it shut.
func pairingWanted(fleet *pki.Fleet, fleetErr error, pkiDir string) bool {
	if fleetErr != nil || fleet == nil || fleet.Root() != nil {
		return false
	}
	if cfg, err := selfregister.Read(selfregister.Dir); err != nil || cfg != nil {
		return false
	}
	_, admin := os.Stat(filepath.Join(pkiDir, "admin.crt"))
	_, marked := os.Stat(filepath.Join(pkiDir, pairingMarker))
	return admin != nil || marked == nil
}

// pairingListen is -pairing-addr resolved: "auto" is the default port
// on a node (-manage-host), nothing elsewhere - several janusd on one
// machine (tests, local-dev) would fight for it.
func pairingListen(flagValue string, manageHost bool) string {
	if flagValue != "auto" {
		return flagValue
	}
	if manageHost {
		return ":" + pairing.DefaultPort
	}
	return ""
}

// pairingWindow is the window open on this node.
type pairingWindow struct {
	code   pairing.Code
	addr   string
	window *pairing.Window
	srv    *grpc.Server
}

// openPairing opens the pairing window on addr: its own certificate, its
// own server serving PairingService alone, a new code - nil when it
// can't (nothing then lets in but what did before: say so).
func openPairing(addr string, local *pki.Local, access *api.Access, fleet *pki.Fleet, hostname string) *pairingWindow {
	leaf, err := pairing.NewLeaf(local.CA(), hostname, pki.LocalIPs())
	if err != nil {
		log.Printf("pairing: the window's certificate: %v - no pairing window", err)
		return nil
	}
	code, err := pairing.NewCode(leaf.Certificate[0])
	if err != nil {
		log.Printf("pairing: the code: %v - no pairing window", err)
		return nil
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("pairing: listen on %s: %v - no pairing window", addr, err)
		return nil
	}
	w := &pairingWindow{code: code, addr: addr, window: pairing.NewWindow(code.SecretBytes(), 0)}
	w.srv = grpc.NewServer(grpc.Creds(credentials.NewTLS(pairing.ServerTLS(leaf))), grpc.UnaryInterceptor(api.PairingInterceptor))
	janusv1alpha1.RegisterPairingServiceServer(w.srv, &api.Pairing{Window: w.window, Access: access})
	go func() {
		if err := w.srv.Serve(lis); err != nil {
			log.Printf("pairing: %v", err)
		}
	}()
	// The window closes once the node trusts a fleet - paired, given one
	// with TrustSet or by its Controller -, or after its tries: the port
	// with it.
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
	wait:
		for {
			select {
			case <-w.window.Done():
				break wait
			case <-tick.C:
				if fleet.Root() != nil {
					w.window.Close("the node trusts a fleet")
					break wait
				}
			}
		}
		why := w.window.Closed()
		log.Printf("pairing: the window on %s is closed: %s", addr, why)
		if why == "paired" {
			// The call that paired it may come again, its answer lost.
			time.Sleep(api.PairReplay)
		}
		w.srv.GracefulStop()
	}()
	log.Printf("pairing: the node trusts nothing yet - a pairing window on %s, its code on the console", addr)
	return w
}

// sayCode shows the window's code on the console - never in janusd's log,
// which Logs, the LOGS partition and log forwarding carry.
func (w *pairingWindow) sayCode() {
	fmt.Fprintf(os.Stderr, "pairing: code %s on %s - janusctl fleet adopt NAME -endpoint ADDRESS -code CODE, or the Controller's Add node\n", w.code, w.addr)
}
