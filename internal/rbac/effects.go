package rbac

import "slices"

// Effects: what an RPC does to the node - read it, or change it -,
// whoever may call it. Roles don't say it (Logs is an operator's yet
// only reads, PacketCapture a read no reader opens): the Controller's
// Read-only switch hides what changes a node and keeps what reads it,
// diagnosis included. Validating a configuration is the apply RPC with
// validate_only for most modules - a change RPC whose Validate button the
// pages keep (the page's may(method, 'validate')); HAProxy's own
// ValidateConfig is a read.
const (
	EffectRead   = "read"
	EffectChange = "change"
)

// Effect is each RPC's effect ("Service/Method", as Allowing names them).
// An RPC missing here changes something (EffectOf): never offered while
// a page is read-only.
var Effect = map[string]string{
	"SystemService/Version":                     EffectRead,
	"SystemService/Hostname":                    EffectRead,
	"SystemService/Reboot":                      EffectChange,
	"SystemService/Shutdown":                    EffectChange,
	"SystemService/Restart":                     EffectChange,
	"SystemService/Reset":                       EffectChange,
	"SystemService/ApplyConfiguration":          EffectChange,
	"SystemService/Events":                      EffectRead,
	"SystemService/Dmesg":                       EffectRead,
	"SystemService/Logs":                        EffectRead,
	"SystemService/LogBoots":                    EffectRead,
	"SystemService/Audit":                       EffectRead,
	"SystemService/LogForwardingGet":            EffectRead,
	"SystemService/LogForwardingSet":            EffectChange,
	"SystemService/LogForwardingTest":           EffectRead, // one test line to a draft's target: diagnosis, the node keeps its forwarding
	"SystemService/Stats":                       EffectRead,
	"SystemService/SystemStat":                  EffectRead,
	"SystemService/Memory":                      EffectRead,
	"SystemService/CPUInfo":                     EffectRead,
	"SystemService/LoadAvg":                     EffectRead,
	"SystemService/DiskStats":                   EffectRead,
	"SystemService/DiskUsage":                   EffectRead,
	"SystemService/NetworkDeviceStats":          EffectRead,
	"SystemService/Netstat":                     EffectRead,
	"SystemService/ClientList":                  EffectRead,
	"SystemService/Mounts":                      EffectRead,
	"SystemService/Processes":                   EffectRead,
	"SystemService/ServiceList":                 EffectRead,
	"SystemService/ServiceStart":                EffectChange,
	"SystemService/ServiceStop":                 EffectChange,
	"SystemService/ServiceRestart":              EffectChange,
	"SystemService/List":                        EffectRead,
	"SystemService/Read":                        EffectRead,
	"SystemService/Copy":                        EffectRead,
	"SystemService/PacketCapture":               EffectRead, // diagnosis
	"SystemService/Probe":                       EffectRead, // diagnosis: connects, changes nothing
	"SystemService/MetaWrite":                   EffectChange,
	"SystemService/MetaDelete":                  EffectChange,
	"SystemService/GenerateClientConfiguration": EffectChange, // a new credential
	"SystemService/CertificateRequest":          EffectChange, // a new key on the node
	"SystemService/StateExport":                 EffectRead,
	"SystemService/StateImport":                 EffectChange,
	"SystemService/StateExportGrant":            EffectChange, // opens the node to another one
	"SystemService/StateImportFrom":             EffectChange,
	"SystemService/MetricsConfigGet":            EffectRead,
	"SystemService/MetricsConfigSet":            EffectChange,
	"SystemService/ConsoleBudgetGet":            EffectRead,
	"SystemService/ConsoleBudgetSet":            EffectChange,
	"SystemService/NodeExporterConfigGet":       EffectRead,
	"SystemService/NodeExporterConfigSet":       EffectChange,
	"SystemService/SysctlList":                  EffectRead,
	"SystemService/SysctlApply":                 EffectChange,
	"SystemService/SysctlConfirm":               EffectChange,
	"SystemService/SysctlCancel":                EffectChange,
	"SystemService/SysctlHistory":               EffectRead,

	"LifecycleService/Install":           EffectChange,
	"LifecycleService/Upgrade":           EffectChange,
	"LifecycleService/Rollback":          EffectChange,
	"LifecycleService/UploadReleaseFile": EffectChange,

	"AccessService/TrustGet":      EffectRead,
	"AccessService/TrustSet":      EffectChange,
	"AccessService/TrustReset":    EffectChange,
	"AccessService/LocalCARotate": EffectChange,

	"HAProxyService/GetConfig":              EffectRead,
	"HAProxyService/ApplyConfig":            EffectChange,
	"HAProxyService/ValidateConfig":         EffectRead, // haproxy -c on the caller's text: nothing saved
	"HAProxyService/ConfigHistory":          EffectRead,
	"HAProxyService/ConfigVersion":          EffectRead,
	"HAProxyService/Reload":                 EffectChange,
	"HAProxyService/Stats":                  EffectRead,
	"HAProxyService/ShowInfo":               EffectRead,
	"HAProxyService/BackendList":            EffectRead,
	"HAProxyService/ServerSetState":         EffectChange,
	"HAProxyService/MapList":                EffectRead,
	"HAProxyService/MapGet":                 EffectRead,
	"HAProxyService/MapUpdate":              EffectChange,
	"HAProxyService/ACLUpdate":              EffectChange,
	"HAProxyService/CertificateList":        EffectRead,
	"HAProxyService/CertificateUpload":      EffectChange,
	"HAProxyService/CertificateDelete":      EffectChange,
	"HAProxyService/FileList":               EffectRead,
	"HAProxyService/FileGet":                EffectRead,
	"HAProxyService/FilePut":                EffectChange,
	"HAProxyService/FileDelete":             EffectChange,
	"HAProxyService/ACMEStatus":             EffectRead,
	"HAProxyService/ACMEGetConfig":          EffectRead,
	"HAProxyService/ACMEApplyConfig":        EffectChange,
	"HAProxyService/ACMERenew":              EffectChange,
	"HAProxyService/ACMECheckPath":          EffectRead, // diagnosis: asks the CA's directory, orders nothing
	"HAProxyService/ACMECertificateRequest": EffectChange,
	"HAProxyService/ACMEInstallCertificate": EffectChange,
	"HAProxyService/MapLookup":              EffectRead,
	"HAProxyService/ACLLookup":              EffectRead,
	"HAProxyService/TableList":              EffectRead,
	"HAProxyService/TableShow":              EffectRead,
	"HAProxyService/TableClear":             EffectChange,
	"HAProxyService/SessionList":            EffectRead,
	"HAProxyService/SessionShutdown":        EffectChange,
	"HAProxyService/ErrorsShow":             EffectRead,

	"NetworkService/BGPStatus":            EffectRead,
	"NetworkService/BGPApplyConfig":       EffectChange,
	"NetworkService/BGPGetConfig":         EffectRead,
	"NetworkService/VRRPStatus":           EffectRead,
	"NetworkService/VRRPApplyConfig":      EffectChange,
	"NetworkService/VRRPGetConfig":        EffectRead,
	"NetworkService/ConsulStatus":         EffectRead,
	"NetworkService/ConsulApplyConfig":    EffectChange,
	"NetworkService/ConsulGetConfig":      EffectRead,
	"NetworkService/ConsulManagedGet":     EffectRead,
	"NetworkService/ConsulManagedSet":     EffectChange,
	"NetworkService/FirewallList":         EffectRead,
	"NetworkService/FirewallApplyRuleset": EffectChange,
	"NetworkService/FirewallGetRuleset":   EffectRead,
	"NetworkService/FirewallConfirm":      EffectChange,
	"NetworkService/FirewallSets":         EffectRead,
	"NetworkService/FirewallSetUpdate":    EffectChange,
	"NetworkService/NetworkConfigGet":     EffectRead,
	"NetworkService/NetworkConfigApply":   EffectChange,
	"NetworkService/NetworkConfigConfirm": EffectChange,
	"NetworkService/NetworkStatus":        EffectRead,
	"NetworkService/NeighborList":         EffectRead,

	"ClusterService/ClusterGet":          EffectRead,
	"ClusterService/ClusterSet":          EffectChange,
	"ClusterService/ClusterLeave":        EffectChange,
	"ClusterService/ClusterRelease":      EffectChange,
	"ClusterService/ClusterStatus":       EffectRead,
	"ClusterService/ClusterDrain":        EffectChange,
	"ClusterService/ClusterUndrain":      EffectChange,
	"ClusterService/ClusterPull":         EffectChange,
	"ClusterService/ClusterExport":       EffectRead,
	"ClusterService/ClusterFlows":        EffectRead,
	"ClusterService/ClusterHandoff":      EffectChange,
	"ClusterService/TLSTicketKeysRotate": EffectChange,

	"PairingService/Pair":   EffectChange,
	"PairingService/Rescue": EffectChange,
}

// EffectOf is fullMethod's effect ("/janus.v1alpha1.Service/Method" or
// "Service/Method") - EffectChange for an RPC nobody classified.
func EffectOf(fullMethod string) string {
	if e, ok := Effect[trim(fullMethod)]; ok {
		return e
	}
	return EffectChange
}

// Reading is the RPCs of methods ("Service/Method") that only read - what
// a page in read-only mode still offers of those its account may call.
func Reading(methods []string) []string {
	var out []string
	for _, m := range methods {
		if EffectOf(m) == EffectRead {
			out = append(out, trim(m))
		}
	}
	slices.Sort(out)
	return out
}
