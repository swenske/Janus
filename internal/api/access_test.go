package api

import (
	"bytes"
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

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
