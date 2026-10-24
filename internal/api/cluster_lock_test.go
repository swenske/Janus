package api

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// Every write a lock covers is an RPC the node serves.
func TestLockDomainsAreRPCs(t *testing.T) {
	for method, domain := range lockDomains {
		if _, ok := requiredRoles[method]; !ok {
			t.Errorf("%s (%s) isn't an RPC", method, domain)
		}
		if domainWords[domain] == "" {
			t.Errorf("%s: domain %q has no words", method, domain)
		}
	}
}

// A member's managed domains take writes from the Controller applying for
// that very cluster only - not from an admin's own certificate, even one
// that sends the cluster's id itself.
func TestClusterLock(t *testing.T) {
	const id = "7f3c2a1e-5b6d-4c8e-9f01-23456789abcd"
	defer func(prev func(string) (string, string, bool, bool)) { ClusterLock = prev }(ClusterLock)
	ClusterLock = func(domain string) (string, string, bool, bool) {
		return id, "edge", domain == "haproxy" || domain == "vrrp", false
	}
	admin := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "admin", Organization: []string{pki.RoleAdmin}}})
	ctl := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "janus-controller", Organization: []string{pki.RoleController}}})
	with := func(ctx context.Context, pairs ...string) context.Context {
		return metadata.NewIncomingContext(ctx, metadata.Pairs(pairs...))
	}
	controller := func(cluster string) context.Context {
		kv := []string{pki.AsUserKey, "cluster", pki.AsRolesKey, pki.RoleAdmin}
		if cluster != "" {
			kv = append(kv, pki.ClusterApplyKey, cluster)
		}
		return with(ctl, kv...)
	}
	const apply, filePut, vrrp, bgp = "/janus.v1alpha1.HAProxyService/ApplyConfig", "/janus.v1alpha1.HAProxyService/FilePut", "/janus.v1alpha1.NetworkService/VRRPApplyConfig", "/janus.v1alpha1.NetworkService/BGPApplyConfig"
	for _, c := range []struct {
		name   string
		ctx    context.Context
		method string
		req    any
		want   codes.Code
	}{
		{"an admin, a managed domain", admin, apply, nil, codes.FailedPrecondition},
		{"an admin sending the cluster's id itself", with(admin, pki.ClusterApplyKey, id), apply, nil, codes.FailedPrecondition},
		{"the Controller for no cluster", controller(""), apply, nil, codes.FailedPrecondition},
		{"the Controller for another cluster", controller("00000000-0000-4000-8000-000000000000"), apply, nil, codes.FailedPrecondition},
		{"the Controller for this cluster", controller(id), apply, nil, codes.OK},
		{"an admin, a domain not managed", admin, filePut, nil, codes.OK},
		{"an admin, another domain not managed", admin, bgp, &janusv1alpha1.BGPApplyConfigRequest{}, codes.OK},
		{"an admin, a managed domain, validating only", admin, vrrp, &janusv1alpha1.VRRPApplyConfigRequest{ValidateOnly: true}, codes.OK},
		{"an admin, a managed domain, applying", admin, vrrp, &janusv1alpha1.VRRPApplyConfigRequest{}, codes.FailedPrecondition},
		{"an admin, what isn't a write", admin, "/janus.v1alpha1.HAProxyService/Reload", nil, codes.OK},
	} {
		caller, err := authorize(c.ctx, c.method)
		if err == nil {
			err = checkClusterLock(c.method, c.req, caller)
		}
		if got := status.Code(err); got != c.want {
			t.Errorf("%s: %v (%v), want %v", c.name, got, err, c.want)
		}
	}
	ClusterLock = nil
	caller, _ := authorize(admin, apply)
	if err := checkClusterLock(apply, nil, caller); err != nil {
		t.Errorf("no cluster, no lock: %v", err)
	}
}

// A cluster without a Controller has advisory locks: an admin sending the
// cluster's id writes through them - janusctl cluster -, nobody else.
func TestClusterLockAdvisory(t *testing.T) {
	const id = "7f3c2a1e-5b6d-4c8e-9f01-23456789abcd"
	defer func(prev func(string) (string, string, bool, bool)) { ClusterLock = prev }(ClusterLock)
	ClusterLock = func(domain string) (string, string, bool, bool) { return id, "edge", domain == "haproxy", true }
	admin := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "admin", Organization: []string{pki.RoleAdmin}}})
	operator := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "op", Organization: []string{pki.RoleOperator}}})
	with := func(ctx context.Context, pairs ...string) context.Context {
		return metadata.NewIncomingContext(ctx, metadata.Pairs(pairs...))
	}
	const apply = "/janus.v1alpha1.HAProxyService/ApplyConfig"
	for _, c := range []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"an admin without the cluster's id", admin, codes.FailedPrecondition},
		{"an admin for another cluster", with(admin, pki.ClusterApplyKey, "00000000-0000-4000-8000-000000000000"), codes.FailedPrecondition},
		{"an admin for this cluster", with(admin, pki.ClusterApplyKey, id), codes.OK},
		{"an operator for this cluster", with(operator, pki.ClusterApplyKey, id), codes.FailedPrecondition},
	} {
		caller, err := authorize(c.ctx, apply)
		if err == nil {
			err = checkClusterLock(apply, nil, caller)
		}
		if got := status.Code(err); got != c.want {
			t.Errorf("%s: %v (%v), want %v", c.name, got, err, c.want)
		}
	}
}
