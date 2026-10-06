package rbac

import "slices"

// Domains: what an RPC is about, so a permission can be narrowed to some
// of them - a token that may run HAProxy only (the Controller's scoped
// tokens and grants, enforced by the Controller on every call it relays
// and by the node on the domains the Controller's certificate names).
// Every RPC is in exactly one; DomainObserve - reading a node's state -
// comes with every permission, as far as its role reaches.
const (
	DomainObserve  = "observe"
	DomainHAProxy  = "haproxy"
	DomainServices = "services" // services, reboots, their logs
	DomainNetwork  = "network"  // network, firewall, VRRP, BGP, Consul
	DomainSystem   = "system"   // updates, access, files, capture, exporters, reset
)

// NodeDomains are the domains a permission may name for a node's RPCs.
var NodeDomains = []string{DomainHAProxy, DomainServices, DomainNetwork, DomainSystem}

// Domain is each RPC's domain ("Service/Method", as Allowing names them).
var Domain = map[string]string{
	"SystemService/Version":                     DomainObserve,
	"SystemService/Hostname":                    DomainObserve,
	"SystemService/Reboot":                      DomainServices,
	"SystemService/Shutdown":                    DomainServices,
	"SystemService/Restart":                     DomainServices,
	"SystemService/Reset":                       DomainSystem,
	"SystemService/ApplyConfiguration":          DomainSystem,
	"SystemService/Events":                      DomainObserve,
	"SystemService/Dmesg":                       DomainSystem,
	"SystemService/Logs":                        DomainServices,
	"SystemService/Stats":                       DomainObserve,
	"SystemService/SystemStat":                  DomainObserve,
	"SystemService/Memory":                      DomainObserve,
	"SystemService/CPUInfo":                     DomainObserve,
	"SystemService/LoadAvg":                     DomainObserve,
	"SystemService/DiskStats":                   DomainObserve,
	"SystemService/DiskUsage":                   DomainSystem,
	"SystemService/NetworkDeviceStats":          DomainObserve,
	"SystemService/Netstat":                     DomainObserve,
	"SystemService/Mounts":                      DomainObserve,
	"SystemService/Processes":                   DomainObserve,
	"SystemService/ServiceList":                 DomainObserve,
	"SystemService/ServiceStart":                DomainServices,
	"SystemService/ServiceStop":                 DomainServices,
	"SystemService/ServiceRestart":              DomainServices,
	"SystemService/List":                        DomainSystem,
	"SystemService/Read":                        DomainSystem,
	"SystemService/Copy":                        DomainSystem,
	"SystemService/PacketCapture":               DomainSystem,
	"SystemService/MetaWrite":                   DomainSystem,
	"SystemService/MetaDelete":                  DomainSystem,
	"SystemService/GenerateClientConfiguration": DomainSystem,
	"SystemService/MetricsConfigGet":            DomainObserve,
	"SystemService/MetricsConfigSet":            DomainSystem,
	"SystemService/NodeExporterConfigGet":       DomainObserve,
	"SystemService/NodeExporterConfigSet":       DomainSystem,
	"SystemService/SysctlList":                  DomainObserve,
	"SystemService/SysctlApply":                 DomainSystem,
	"SystemService/SysctlConfirm":               DomainSystem,
	"SystemService/SysctlCancel":                DomainSystem,
	"SystemService/SysctlHistory":               DomainObserve,

	"LifecycleService/Install":           DomainSystem,
	"LifecycleService/Upgrade":           DomainSystem,
	"LifecycleService/Rollback":          DomainSystem,
	"LifecycleService/UploadReleaseFile": DomainSystem,

	"AccessService/TrustGet":      DomainObserve,
	"AccessService/TrustSet":      DomainSystem,
	"AccessService/TrustReset":    DomainSystem,
	"AccessService/LocalCARotate": DomainSystem,

	// HAProxy's state is everyone's to read; its configuration - files,
	// maps, certificates, Let's Encrypt - is the haproxy domain's.
	"HAProxyService/Stats":             DomainObserve,
	"HAProxyService/ShowInfo":          DomainObserve,
	"HAProxyService/BackendList":       DomainObserve,
	"HAProxyService/GetConfig":         DomainHAProxy,
	"HAProxyService/ApplyConfig":       DomainHAProxy,
	"HAProxyService/ValidateConfig":    DomainHAProxy,
	"HAProxyService/Reload":            DomainHAProxy,
	"HAProxyService/ServerSetState":    DomainHAProxy,
	"HAProxyService/MapList":           DomainHAProxy,
	"HAProxyService/MapGet":            DomainHAProxy,
	"HAProxyService/MapUpdate":         DomainHAProxy,
	"HAProxyService/ACLUpdate":         DomainHAProxy,
	"HAProxyService/CertificateList":   DomainHAProxy,
	"HAProxyService/CertificateUpload": DomainHAProxy,
	"HAProxyService/CertificateDelete": DomainHAProxy,
	"HAProxyService/FileList":          DomainHAProxy,
	"HAProxyService/FileGet":           DomainHAProxy,
	"HAProxyService/FilePut":           DomainHAProxy,
	"HAProxyService/FileDelete":        DomainHAProxy,
	"HAProxyService/ACMEStatus":        DomainHAProxy,
	"HAProxyService/ACMEGetConfig":     DomainHAProxy,
	"HAProxyService/ACMEApplyConfig":   DomainHAProxy,
	"HAProxyService/ACMERenew":         DomainHAProxy,

	// The network's state is everyone's; its configuration the network
	// domain's.
	"NetworkService/NetworkStatus":        DomainObserve,
	"NetworkService/BGPStatus":            DomainObserve,
	"NetworkService/VRRPStatus":           DomainObserve,
	"NetworkService/ConsulStatus":         DomainObserve,
	"NetworkService/FirewallList":         DomainObserve,
	"NetworkService/BGPApplyConfig":       DomainNetwork,
	"NetworkService/BGPGetConfig":         DomainNetwork,
	"NetworkService/VRRPApplyConfig":      DomainNetwork,
	"NetworkService/VRRPGetConfig":        DomainNetwork,
	"NetworkService/ConsulApplyConfig":    DomainNetwork,
	"NetworkService/ConsulGetConfig":      DomainNetwork,
	"NetworkService/FirewallApplyRuleset": DomainNetwork,
	"NetworkService/FirewallGetRuleset":   DomainNetwork,
	"NetworkService/FirewallConfirm":      DomainNetwork,
	"NetworkService/FirewallSets":         DomainNetwork,
	"NetworkService/FirewallSetUpdate":    DomainNetwork,
	"NetworkService/NetworkConfigGet":     DomainNetwork,
	"NetworkService/NetworkConfigApply":   DomainNetwork,
	"NetworkService/NetworkConfigConfirm": DomainNetwork,
}

// DomainOf is method's domain ("Service/Method" or its full name):
// DomainSystem, the widest, for one nobody placed - never silently open.
func DomainOf(method string) string {
	if d, ok := Domain[trim(method)]; ok {
		return d
	}
	return DomainSystem
}

// InDomains reports whether method may be called with permission over
// domains - every domain when there's none named; observing always.
func InDomains(method string, domains []string) bool {
	if len(domains) == 0 {
		return true
	}
	d := DomainOf(method)
	return d == DomainObserve || slices.Contains(domains, d)
}

func trim(method string) string {
	const prefix = "/janus.v1alpha1."
	if len(method) > len(prefix) && method[:len(prefix)] == prefix {
		return method[len(prefix):]
	}
	return method
}
