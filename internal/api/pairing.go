package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/pairing"
	"github.com/swenske/Janus/internal/pki"
)

// Pairing is PairingService, a node's pairing window (internal/pairing):
// served on its own port by its own server - never the API's, which
// asks every caller for a certificate.
type Pairing struct {
	janusv1alpha1.UnimplementedPairingServiceServer
	Window *pairing.Window
	// Access applies the fleet - as TrustSet does - and answers with the
	// node's trust.
	Access *Access

	mu sync.Mutex
	// done is the call that paired the node: the same one again, its
	// answer lost on the way, gets the same answer for a while.
	done struct {
		key  [32]byte
		resp *janusv1alpha1.PairResponse
		at   time.Time
	}
}

// PairReplay is how long the call that paired the node may come again,
// its answer lost on the way: janusd keeps the window's port that long.
const PairReplay = 30 * time.Second

func pairKey(req *janusv1alpha1.PairRequest) [32]byte {
	h := sha256.New()
	for _, b := range [][]byte{req.GetSecret(), req.GetRootCert(), req.GetBundle()} {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(b))))
		h.Write(b)
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

func (p *Pairing) Pair(ctx context.Context, req *janusv1alpha1.PairRequest) (*janusv1alpha1.PairResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := pairKey(req)
	if p.done.resp != nil && p.done.key == key && time.Since(p.done.at) < PairReplay {
		return p.done.resp, nil
	}
	if err := p.Window.Check(req.GetSecret()); err != nil {
		var wrong *pairing.ErrWrong
		if errors.As(err, &wrong) {
			log.Printf("pairing: a wrong code from %s - %d tries left", peerAddr(ctx), wrong.Left)
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	// The secret is right: a fleet refused leaves the window open for a
	// right one.
	if err := p.Access.Fleet.Set(req.GetRootCert(), req.GetBundle()); err != nil {
		if errors.Is(err, pki.ErrFleetRootMismatch) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p.Window.Close("paired")
	st := p.Access.state()
	root := pki.Fingerprint(p.Access.Fleet.Root().Raw)
	log.Printf("pairing: paired from %s - the node trusts the fleet of root %s, bundle version %d (%d issuing CAs)", peerAddr(ctx), root, st.BundleVersion, len(st.IssuingCas))
	events.Publish("access.paired", map[string]any{"root": root, "bundle_version": st.BundleVersion, "from": peerAddr(ctx)})
	resp := &janusv1alpha1.PairResponse{CaCert: p.Access.Local.CA().CertPEM, Trust: st}
	p.done.key, p.done.resp, p.done.at = key, resp, time.Now()
	return resp, nil
}

// PairingInterceptor audits every call to the pairing window - who
// tried, from where, how it ended (AuditLog): nobody presents a
// certificate there.
func PairingInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	if AuditLog != nil {
		st := status.Convert(err)
		e := AuditEntry{Caller: "pairing code", Peer: peerAddr(ctx), Code: st.Code().String(), Denied: err != nil, DurationMs: time.Since(start).Milliseconds()}
		if err != nil {
			e.Error = st.Message()
		}
		AuditLog.Publish(strings.TrimPrefix(info.FullMethod, "/janus.v1alpha1."), e)
	}
	return resp, err
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "?"
}
