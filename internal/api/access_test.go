package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// TestTrustGet: what a fleet's client needs - the node's own CA to pin,
// the signed bundle to compare with its own.
func TestTrustGet(t *testing.T) {
	local, err := pki.NewCA("node")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := pki.OpenFleet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Access{Fleet: fleet, Local: pki.NewLocal(local)}
	st, _ := a.TrustGet(context.Background(), &emptypb.Empty{})
	if !bytes.Equal(st.GetLocalCaCert(), local.CertPEM) || len(st.GetRootCert()) != 0 || len(st.GetBundle()) != 0 {
		t.Fatalf("no fleet: %v", st)
	}
	root, _ := pki.NewCAFor("root", time.Hour)
	issuing, _ := root.IssueCA("issuing", time.Hour)
	signed, err := pki.SignBundle(root, pki.Bundle{Version: 3, Issued: time.Now(), IssuingCAs: []string{string(issuing.CertPEM)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.Set(root.CertPEM, signed); err != nil {
		t.Fatal(err)
	}
	st, _ = a.TrustGet(context.Background(), &emptypb.Empty{})
	if !bytes.Equal(st.GetBundle(), signed) || st.GetBundleVersion() != 3 || !bytes.Equal(st.GetLocalCaCert(), local.CertPEM) {
		t.Errorf("with a fleet: version %d, bundle %d bytes", st.GetBundleVersion(), len(st.GetBundle()))
	}
}

// TestLocalCARotateNeedsAKey: the new admin certificate is for the
// caller's key - a node makes none, prints none -, and its CA stays as
// it was when none is given.
func TestLocalCARotateNeedsAKey(t *testing.T) {
	dir := t.TempDir()
	b, err := pki.LoadOrBootstrap(dir, "node", nil)
	if err != nil {
		t.Fatal(err)
	}
	local := pki.NewLocal(b.CA)
	a := &Access{Local: local, ServerCert: pki.NewServerCert(b.CA, dir, b.ServerCert)}
	for _, key := range [][]byte{nil, []byte("-----BEGIN PUBLIC KEY-----\nMFkw\n-----END PUBLIC KEY-----\n")} {
		_, err := a.LocalCARotate(context.Background(), &janusv1alpha1.LocalCARotateRequest{AdminPublicKey: key})
		if status.Code(err) != codes.InvalidArgument || local.CA() != b.CA {
			t.Errorf("key %q: %v, CA replaced %v", key, err, local.CA() != b.CA)
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.LocalCARotate(context.Background(), &janusv1alpha1.LocalCARotateRequest{AdminPublicKey: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})})
	if err != nil || len(resp.GetAdminCert()) == 0 || !bytes.Equal(resp.GetCaCert(), local.CA().CertPEM) || local.CA() == b.CA {
		t.Fatalf("with a key: %v", err)
	}
}
