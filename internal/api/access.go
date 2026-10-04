package api

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

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
	// Local is the node's own CA; ServerCert, its server certificate -
	// both replaced by LocalCARotate.
	Local      *pki.Local
	ServerCert *pki.ServerCert
	// Console gets what LocalCARotate prints when the node made the new
	// admin key - janusd's stderr, the console: never its log ring,
	// which Logs serves.
	Console io.Writer
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
	log.Printf("access: fleet root %s, bundle version %d (%d issuing CAs) - by %s", pki.Fingerprint(a.Fleet.Root().Raw), st.BundleVersion, len(st.IssuingCas), caller)
	events.Publish("access.trust_set", map[string]any{"root": pki.Fingerprint(a.Fleet.Root().Raw), "bundle_version": st.BundleVersion, "by": caller.String()})
	return st, nil
}

func (a *Access) TrustReset(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.TrustState, error) {
	if !viaLocalCA(ctx, a.Local.CA().Cert.Raw) {
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

func (a *Access) LocalCARotate(ctx context.Context, req *janusv1alpha1.LocalCARotateRequest) (*janusv1alpha1.LocalCARotateResponse, error) {
	var pub crypto.PublicKey
	if len(req.GetAdminPublicKey()) > 0 {
		p, err := parseAdminPublicKey(req.GetAdminPublicKey())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "admin_public_key: %v", err)
		}
		pub = p
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "janus"
	}
	r, err := a.ServerCert.Rotate(a.Local, hostname, pki.LocalIPs(), pub)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "replace the node's CA: %v", err)
	}
	caller, _ := CallerFrom(ctx)
	log.Printf("access: the node's own CA replaced (SHA-256 %s) - every certificate the old one issued stops working - by %s", pki.Fingerprint(r.CA.Cert.Raw), caller)
	events.Publish("access.local_ca_rotated", map[string]any{"ca": pki.Fingerprint(r.CA.Cert.Raw), "by": caller.String()})
	resp := &janusv1alpha1.LocalCARotateResponse{CaCert: r.CA.CertPEM}
	if r.AdminKeyPEM == nil {
		resp.AdminCert = r.AdminCertPEM
		return resp, nil
	}
	now := time.Now().Format("2006/01/02 15:04:05")
	fmt.Fprintf(a.Console, "%s pki: the node's own CA was replaced (by %s)\n%s pki: CA CERTIFICATE (needed for janusctl's -ca flag):\n%s%s pki: ADMIN CERTIFICATE (save this now, it will not be printed again):\n%s%s",
		now, caller, now, r.CA.CertPEM, now, r.AdminCertPEM, r.AdminKeyPEM)
	return resp, nil
}

// parseAdminPublicKey reads a PKIX PEM public key a certificate may be
// issued for.
func parseAdminPublicKey(p []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(p)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM PUBLIC KEY")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, fmt.Errorf("ECDSA on %s: P-256 or P-384", k.Curve.Params().Name)
		}
	case ed25519.PublicKey:
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, fmt.Errorf("RSA %d bits: 2048 or more", k.N.BitLen())
		}
	default:
		return nil, fmt.Errorf("%T: ECDSA, Ed25519 or RSA", pub)
	}
	return pub, nil
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

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
