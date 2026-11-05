package api

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"regexp"
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
	// RescueVolume: the window is a rescue volume's - Rescue answers, and
	// OnRescue is called once it did.
	RescueVolume bool
	OnRescue     func()

	mu sync.Mutex
	// done is the call that paired the node: the same one again, its
	// answer lost on the way, gets the same answer for a while.
	done struct {
		key  [32]byte
		resp *janusv1alpha1.PairResponse
		at   time.Time
	}
	// rescued is Rescue's done.
	rescued struct {
		key  [32]byte
		resp *janusv1alpha1.RescueResponse
		at   time.Time
	}
}

// PairReplay is how long the call that paired the node may come again,
// its answer lost on the way: janusd keeps the window's port that long.
const PairReplay = 30 * time.Second

func callKey(parts ...[]byte) [32]byte {
	h := sha256.New()
	for _, b := range parts {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(b))))
		h.Write(b)
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// check takes a call's secret: the window's error as the call's, a
// wrong one logged.
func (p *Pairing) check(ctx context.Context, secret []byte) error {
	err := p.Window.Check(peerIP(ctx), secret)
	if err == nil {
		return nil
	}
	var wrong *pairing.ErrWrong
	if errors.As(err, &wrong) {
		if wrong.Blocked {
			// A paused source: rate-limited, and not audited again - the
			// interceptor drops ResourceExhausted (security review #17).
			return status.Error(codes.ResourceExhausted, err.Error())
		}
		log.Printf("pairing: a wrong code from %s - %d tries left", peerAddr(ctx), wrong.Left)
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return status.Error(codes.FailedPrecondition, err.Error())
}

func (p *Pairing) Pair(ctx context.Context, req *janusv1alpha1.PairRequest) (*janusv1alpha1.PairResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := callKey(req.GetSecret(), req.GetRootCert(), req.GetBundle())
	if p.done.resp != nil && p.done.key == key && time.Since(p.done.at) < PairReplay {
		return p.done.resp, nil
	}
	if err := p.check(ctx, req.GetSecret()); err != nil {
		return nil, err
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

// rescueName is what Rescue takes for a name: the certificate's
// rescue:<name> - logged and audited by the node for every call it makes.
var rescueName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,62}$`)

func (p *Pairing) Rescue(ctx context.Context, req *janusv1alpha1.RescueRequest) (*janusv1alpha1.RescueResponse, error) {
	if !p.RescueVolume {
		// Count it against the source: a repeated Rescue on a pairing
		// window would otherwise be audited without limit and evict the
		// audit (security review 2026-11-05 #17).
		if pairing.IsBlocked(p.Window.Fail(peerIP(ctx))) {
			return nil, status.Error(codes.ResourceExhausted, "too many attempts from your address: wait and try again")
		}
		return nil, status.Error(codes.FailedPrecondition, "no rescue volume: only a node started with one (janusctl image rescue-volume) gives a rescue certificate")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := callKey(req.GetSecret(), req.GetPublicKey(), []byte(req.GetName()))
	if p.rescued.resp != nil && p.rescued.key == key && time.Since(p.rescued.at) < PairReplay {
		return p.rescued.resp, nil
	}
	if err := p.check(ctx, req.GetSecret()); err != nil {
		return nil, err
	}
	// The secret is right: a key or a name refused leaves the window
	// open for a right one.
	if !rescueName.MatchString(req.GetName()) {
		return nil, status.Error(codes.InvalidArgument, "name: letters, digits, . _ @ - (64 at most), starting with a letter or a digit")
	}
	pub, err := parseAdminPublicKey(req.GetPublicKey())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "public_key: %v", err)
	}
	ca := p.Access.Local.CA()
	cn := "rescue:" + req.GetName()
	certPEM, err := ca.IssueFor(pub, pki.IssueOptions{
		CommonName: cn, Roles: []string{pki.RoleAdmin},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, Validity: pairing.RescueValidity,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "issue: %v", err)
	}
	p.Window.Close("rescued")
	log.Printf("rescue: an admin certificate for %s, an hour, issued to %s - the rescue window is closed", cn, peerAddr(ctx))
	events.Publish("access.rescue_issued", map[string]any{"name": cn, "from": peerAddr(ctx), "valid_for": pairing.RescueValidity.String()})
	if p.OnRescue != nil {
		p.OnRescue()
	}
	resp := &janusv1alpha1.RescueResponse{CaCert: ca.CertPEM, Cert: certPEM}
	p.rescued.key, p.rescued.resp, p.rescued.at = key, resp, time.Now()
	return resp, nil
}

// PairingInterceptor audits every call to the pairing window - who
// tried, from where, how it ended (AuditLog): nobody presents a
// certificate there.
func PairingInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	// A paused source (ResourceExhausted) is already rate-limited: not
	// audited again, so a flood can't evict the audit (security review #17).
	if AuditLog != nil && status.Code(err) != codes.ResourceExhausted {
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

// peerIP is the caller's address without the ephemeral port: the key the
// window counts wrong codes under, so a flood from one host (which opens a
// fresh port each call) is one source (security review 2026-11-05 #17).
func peerIP(ctx context.Context) string {
	addr := peerAddr(ctx)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
