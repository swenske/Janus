package api

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// TestGenerateClientConfiguration: the certificate carries the name it's
// for, its roles and the validity asked for - a year at most.
func TestGenerateClientConfiguration(t *testing.T) {
	ca, err := pki.NewCA("test")
	if err != nil {
		t.Fatal(err)
	}
	s := &System{LocalCA: func() *pki.CA { return ca }}
	issue := func(req *janusv1alpha1.GenerateClientConfigurationRequest) (*x509.Certificate, error) {
		resp, err := s.GenerateClientConfiguration(context.Background(), req)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(resp.GetCrt())
		if block == nil {
			t.Fatal("no certificate PEM")
		}
		return x509.ParseCertificate(block.Bytes)
	}
	until := func(c *x509.Certificate) time.Duration { return time.Until(c.NotAfter) }

	c, err := issue(&janusv1alpha1.GenerateClientConfigurationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject.CommonName != "client" || !slices.Equal(c.Subject.Organization, []string{pki.RoleAdmin}) || until(c) < pki.LeafValidity-time.Minute {
		t.Errorf("default: CN %q, roles %v, valid %s", c.Subject.CommonName, c.Subject.Organization, until(c))
	}

	c, err = issue(&janusv1alpha1.GenerateClientConfigurationRequest{Roles: []string{pki.RoleReader}, Name: "alice-laptop", TtlSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject.CommonName != "alice-laptop" || !slices.Equal(c.Subject.Organization, []string{pki.RoleReader}) || until(c) > time.Hour || until(c) < time.Hour-time.Minute {
		t.Errorf("named, 1h: CN %q, roles %v, valid %s", c.Subject.CommonName, c.Subject.Organization, until(c))
	}

	for _, req := range []*janusv1alpha1.GenerateClientConfigurationRequest{
		{Roles: []string{"os:root"}},
		{Name: "-dash"},
		{Name: "line\nbreak"},
		{Name: "a/b"},
		{Name: string(make([]byte, 65))},
		{TtlSeconds: 59},
		{TtlSeconds: uint32(pki.LeafValidity.Seconds()) + 1},
	} {
		if _, err := s.GenerateClientConfiguration(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v, want InvalidArgument", req, err)
		}
	}
}
