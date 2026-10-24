package api

import (
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/internal/pki"
)

// Cluster locks (docs/cluster.md "Locks and drift"): on a cluster member,
// what the cluster manages - HAProxy's configuration, its files and
// certificates, VRRP, BGP, the firewall - is changed through the cluster,
// on every member alike. A write to a managed domain is refused unless
// the Controller makes it for that very cluster: its certificate, and the
// cluster's id in pki.ClusterApplyKey. A cluster without a Controller
// (janusctl cluster) has advisory locks: an admin's certificate sending
// the cluster's id gets through too - what keeps the node's page and a
// casual apply-config off, not an admin. The node's own users release a
// domain first (ClusterRelease) for an emergency change; the cluster then
// reports the member as drifted. What runs live - servers' state, maps
// and ACLs, stick tables, sessions, the firewall's sets, ACME - stays
// each node's. Secrets pulled from another member (ClusterPull) don't go
// through these RPCs.

// ClusterLock is the node's cluster lock on a domain (janusd sets it,
// internal/cluster.Store.Lock): the cluster's id and name when it manages
// the domain, and whether its locks are advisory. Nil: nothing is locked.
var ClusterLock func(domain string) (id, name string, locked, advisory bool)

// lockDomains are the writes each managed domain covers.
var lockDomains = map[string]string{
	"/janus.v1alpha1.HAProxyService/ApplyConfig":          "haproxy",
	"/janus.v1alpha1.HAProxyService/FilePut":              "files",
	"/janus.v1alpha1.HAProxyService/FileDelete":           "files",
	"/janus.v1alpha1.HAProxyService/CertificateUpload":    "files",
	"/janus.v1alpha1.HAProxyService/CertificateDelete":    "files",
	"/janus.v1alpha1.NetworkService/VRRPApplyConfig":      "vrrp",
	"/janus.v1alpha1.NetworkService/BGPApplyConfig":       "bgp",
	"/janus.v1alpha1.NetworkService/FirewallApplyRuleset": "firewall",
}

var domainWords = map[string]string{
	"haproxy":  "HAProxy's configuration",
	"files":    "HAProxy's files and certificates",
	"vrrp":     "VRRP's configuration",
	"bgp":      "BGP's configuration",
	"firewall": "the firewall's ruleset",
}

// checkClusterLock refuses a write to a domain the node's cluster manages,
// unless the Controller makes it for that cluster - or, the locks
// advisory, an admin does. req is the request, nil for a stream: a
// validate-only call writes nothing.
func checkClusterLock(fullMethod string, req any, c Caller) error {
	domain, ok := lockDomains[fullMethod]
	if !ok || ClusterLock == nil {
		return nil
	}
	if v, ok := req.(interface{ GetValidateOnly() bool }); ok && v.GetValidateOnly() {
		return nil
	}
	id, name, locked, advisory := ClusterLock(domain)
	if !locked {
		return nil
	}
	if c.ClusterApply == id && (c.Via != "" || (advisory && slices.Contains(c.Roles, pki.RoleAdmin))) {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition, "%s is managed by cluster %s: change it on the cluster, or release this node's lock first (janusctl cluster release %s) - the cluster then reports this node as drifted", domainWords[domain], name, domain)
}
