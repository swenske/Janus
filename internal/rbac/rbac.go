// Package rbac is which role may call which RPC on a node: janusd
// enforces it (internal/api/authz.go), the Controller reads it to show a
// page only what its account may do.
//
// Three roles. RoleReader is scoped to observability/status only.
// Notably NOT reader-accessible: List/Read/Copy/Dmesg/Logs/DiskUsage/
// PacketCapture - these don't mutate anything, but they can expose
// sensitive file contents or traffic, which is a different risk than
// "can this identity see a metric." RoleOperator runs what's set up:
// HAProxy's configuration and runtime state (maps, ACLs, certificates,
// files, ACME, servers' state), services and their logs, reboots - not
// how the node is set up (network, firewall, VRRP/BGP/Consul, upgrades,
// reset, its files, packet capture, credentials). RoleAdmin: everything.
package rbac

import (
	"slices"
	"strings"

	"github.com/swenske/Janus/internal/pki"
)

var (
	adminOnly = []string{pki.RoleAdmin}
	operators = []string{pki.RoleAdmin, pki.RoleOperator}
	readers   = []string{pki.RoleAdmin, pki.RoleOperator, pki.RoleReader}
)

// Required is the roles that may call each RPC (its full method name).
// Fail closed: an RPC missing here is admin-only (RolesFor).
var Required = map[string][]string{
	// SystemService
	"/janus.v1alpha1.SystemService/Version":                     readers,
	"/janus.v1alpha1.SystemService/Hostname":                    readers,
	"/janus.v1alpha1.SystemService/Reboot":                      operators,
	"/janus.v1alpha1.SystemService/Shutdown":                    operators,
	"/janus.v1alpha1.SystemService/Restart":                     operators,
	"/janus.v1alpha1.SystemService/Reset":                       adminOnly,
	"/janus.v1alpha1.SystemService/ApplyConfiguration":          adminOnly,
	"/janus.v1alpha1.SystemService/Events":                      readers,
	"/janus.v1alpha1.SystemService/Dmesg":                       adminOnly, // kernel log can leak boot secrets/paths
	"/janus.v1alpha1.SystemService/Logs":                        operators, // service logs can leak request data: not readers
	"/janus.v1alpha1.SystemService/Stats":                       readers,
	"/janus.v1alpha1.SystemService/SystemStat":                  readers,
	"/janus.v1alpha1.SystemService/Memory":                      readers,
	"/janus.v1alpha1.SystemService/CPUInfo":                     readers,
	"/janus.v1alpha1.SystemService/LoadAvg":                     readers,
	"/janus.v1alpha1.SystemService/DiskStats":                   readers,
	"/janus.v1alpha1.SystemService/DiskUsage":                   adminOnly, // walks arbitrary paths
	"/janus.v1alpha1.SystemService/NetworkDeviceStats":          readers,
	"/janus.v1alpha1.SystemService/Netstat":                     readers,
	"/janus.v1alpha1.SystemService/Mounts":                      readers,
	"/janus.v1alpha1.SystemService/Processes":                   readers,
	"/janus.v1alpha1.SystemService/ServiceList":                 readers,
	"/janus.v1alpha1.SystemService/ServiceStart":                operators,
	"/janus.v1alpha1.SystemService/ServiceStop":                 operators,
	"/janus.v1alpha1.SystemService/ServiceRestart":              operators,
	"/janus.v1alpha1.SystemService/List":                        adminOnly, // filesystem access
	"/janus.v1alpha1.SystemService/Read":                        adminOnly, // can read secrets/keys
	"/janus.v1alpha1.SystemService/Copy":                        adminOnly, // can read secrets/keys
	"/janus.v1alpha1.SystemService/PacketCapture":               adminOnly, // can capture unencrypted traffic
	"/janus.v1alpha1.SystemService/MetaWrite":                   adminOnly,
	"/janus.v1alpha1.SystemService/MetaDelete":                  adminOnly,
	"/janus.v1alpha1.SystemService/GenerateClientConfiguration": adminOnly, // issuing credentials is itself a privileged operation
	"/janus.v1alpha1.SystemService/MetricsConfigGet":            readers,
	"/janus.v1alpha1.SystemService/MetricsConfigSet":            adminOnly,
	"/janus.v1alpha1.SystemService/NodeExporterConfigGet":       readers,
	"/janus.v1alpha1.SystemService/NodeExporterConfigSet":       adminOnly,

	// LifecycleService - installing/upgrading/rolling back the machine
	// is always privileged, no reader carve-out.
	"/janus.v1alpha1.LifecycleService/Install":           adminOnly,
	"/janus.v1alpha1.LifecycleService/Upgrade":           adminOnly,
	"/janus.v1alpha1.LifecycleService/Rollback":          adminOnly,
	"/janus.v1alpha1.LifecycleService/UploadReleaseFile": adminOnly, // writes to persistent STATE storage, same trust level as Upgrade itself

	// AccessService - a fleet's trust decides who else gets in: admin,
	// and TrustReset only through the node's own CA (access.go).
	"/janus.v1alpha1.AccessService/TrustGet":      readers,
	"/janus.v1alpha1.AccessService/TrustSet":      adminOnly,
	"/janus.v1alpha1.AccessService/TrustReset":    adminOnly,
	"/janus.v1alpha1.AccessService/LocalCARotate": adminOnly,

	// HAProxyService
	"/janus.v1alpha1.HAProxyService/GetConfig":         readers,
	"/janus.v1alpha1.HAProxyService/ApplyConfig":       operators,
	"/janus.v1alpha1.HAProxyService/ValidateConfig":    readers, // no side effects - validates the caller's own input
	"/janus.v1alpha1.HAProxyService/Reload":            operators,
	"/janus.v1alpha1.HAProxyService/Stats":             readers,
	"/janus.v1alpha1.HAProxyService/ShowInfo":          readers,
	"/janus.v1alpha1.HAProxyService/BackendList":       readers,
	"/janus.v1alpha1.HAProxyService/ServerSetState":    operators,
	"/janus.v1alpha1.HAProxyService/MapList":           readers,
	"/janus.v1alpha1.HAProxyService/MapGet":            readers,
	"/janus.v1alpha1.HAProxyService/MapUpdate":         operators,
	"/janus.v1alpha1.HAProxyService/ACLUpdate":         operators,
	"/janus.v1alpha1.HAProxyService/CertificateList":   readers, // names/expiry only, not key material
	"/janus.v1alpha1.HAProxyService/CertificateUpload": operators,
	"/janus.v1alpha1.HAProxyService/CertificateDelete": operators,
	"/janus.v1alpha1.HAProxyService/FileList":          readers,
	"/janus.v1alpha1.HAProxyService/FileGet":           readers, // private keys are never read back
	"/janus.v1alpha1.HAProxyService/FilePut":           operators,
	"/janus.v1alpha1.HAProxyService/FileDelete":        operators,
	"/janus.v1alpha1.HAProxyService/ACMEStatus":        readers,
	"/janus.v1alpha1.HAProxyService/ACMEGetConfig":     readers, // secrets come back empty
	"/janus.v1alpha1.HAProxyService/ACMEApplyConfig":   operators,
	"/janus.v1alpha1.HAProxyService/ACMERenew":         operators,

	// NetworkService
	"/janus.v1alpha1.NetworkService/BGPStatus":            readers,
	"/janus.v1alpha1.NetworkService/BGPApplyConfig":       adminOnly,
	"/janus.v1alpha1.NetworkService/BGPGetConfig":         readers,
	"/janus.v1alpha1.NetworkService/VRRPStatus":           readers,
	"/janus.v1alpha1.NetworkService/VRRPApplyConfig":      adminOnly,
	"/janus.v1alpha1.NetworkService/VRRPGetConfig":        readers,
	"/janus.v1alpha1.NetworkService/ConsulStatus":         readers,
	"/janus.v1alpha1.NetworkService/ConsulApplyConfig":    adminOnly,
	"/janus.v1alpha1.NetworkService/ConsulGetConfig":      adminOnly, // the configuration may hold the gossip key and ACL tokens
	"/janus.v1alpha1.NetworkService/FirewallList":         readers,
	"/janus.v1alpha1.NetworkService/FirewallApplyRuleset": adminOnly,
	"/janus.v1alpha1.NetworkService/FirewallGetRuleset":   readers,
	"/janus.v1alpha1.NetworkService/FirewallConfirm":      adminOnly,
	"/janus.v1alpha1.NetworkService/FirewallSets":         readers,
	"/janus.v1alpha1.NetworkService/FirewallSetUpdate":    adminOnly,
	"/janus.v1alpha1.NetworkService/NetworkConfigGet":     readers, // no secrets in it
	"/janus.v1alpha1.NetworkService/NetworkConfigApply":   adminOnly,
	"/janus.v1alpha1.NetworkService/NetworkConfigConfirm": adminOnly,
	"/janus.v1alpha1.NetworkService/NetworkStatus":        readers,
}

// RolesFor is the roles that may call fullMethod - admin only for an RPC
// nobody classified, never silently open to readers.
func RolesFor(fullMethod string) []string {
	if required, ok := Required[fullMethod]; ok {
		return required
	}
	return adminOnly
}

// Allowed reports whether one of roles may call fullMethod.
func Allowed(fullMethod string, roles []string) bool {
	return slices.ContainsFunc(roles, func(r string) bool { return slices.Contains(RolesFor(fullMethod), r) })
}

// Allowing is the RPCs one of roles may call, as "Service/Method" - what
// the Controller hands a node's page.
func Allowing(roles []string) []string {
	var out []string
	for m := range Required {
		if Allowed(m, roles) {
			out = append(out, strings.TrimPrefix(m, "/janus.v1alpha1."))
		}
	}
	slices.Sort(out)
	return out
}
