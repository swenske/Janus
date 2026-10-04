package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/pki"
)

// Access is AccessService: the fleet a node trusts besides its own CA
// (internal/pki/fleet.go).
type Access struct {
	janusv1alpha1.UnimplementedAccessServiceServer
	Fleet *pki.Fleet
	// LocalCA is the node's own CA, now.
	LocalCA func() *pki.CA
}

func (a *Access) state() *janusv1alpha1.TrustState {
	st := &janusv1alpha1.TrustState{}
	root := a.Fleet.Root()
	if root == nil {
		return st
	}
	st.RootCert = pemCert(root.Raw)
	if b, cas := a.Fleet.Bundle(); b != nil {
		st.BundleVersion, st.BundleIssuedUnix = b.Version, b.Issued.Unix()
		for _, c := range cas {
			st.IssuingCas = append(st.IssuingCas, pemCert(c.Raw))
		}
	}
	return st
}

func (a *Access) TrustGet(context.Context, *emptypb.Empty) (*janusv1alpha1.TrustState, error) {
	return a.state(), nil
}

func (a *Access) TrustSet(ctx context.Context, req *janusv1alpha1.TrustSetRequest) (*janusv1alpha1.TrustState, error) {
	if err := a.Fleet.Set(req.GetRootCert(), req.GetBundle()); err != nil {
		if errors.Is(err, pki.ErrFleetRootMismatch) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	st := a.state()
	caller, _ := CallerFrom(ctx)
	log.Printf("access: fleet root %s, bundle version %d (%d issuing CAs) - by %s", fingerprint(a.Fleet.Root().Raw), st.BundleVersion, len(st.IssuingCas), caller)
	events.Publish("access.trust_set", map[string]any{"root": fingerprint(a.Fleet.Root().Raw), "bundle_version": st.BundleVersion, "by": caller.String()})
	return st, nil
}

func (a *Access) TrustReset(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.TrustState, error) {
	if !viaLocalCA(ctx, a.LocalCA().Cert.Raw) {
		return nil, status.Error(codes.PermissionDenied, "only a certificate of the node's own CA may forget its fleet")
	}
	if err := a.Fleet.Reset(); err != nil {
		return nil, status.Errorf(codes.Internal, "forget the fleet: %v", err)
	}
	caller, _ := CallerFrom(ctx)
	log.Printf("access: fleet forgotten - by %s", caller)
	events.Publish("access.trust_reset", map[string]any{"by": caller.String()})
	return a.state(), nil
}

// viaLocalCA reports whether the call's client certificate chains to the
// node's own CA (local, DER) - not to its fleet.
func viaLocalCA(ctx context.Context, local []byte) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return false
	}
	for _, chain := range info.State.VerifiedChains {
		if len(chain) > 0 && string(chain[len(chain)-1].Raw) == string(local) {
			return true
		}
	}
	return false
}

// fingerprint is a certificate's SHA-256, in hex.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
