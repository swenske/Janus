// Role enforcement: mTLS (internal/pki, wired up in cmd/janusd)
// proves *who* is calling - this file decides *what* they're allowed to
// call, based on the role(s) carried in their verified client
// certificate's Subject.Organization.
//
// requiredRoles is fail-closed by design: a method with no entry defaults
// to admin-only rather than being silently open. Every RPC in
// api/proto/janus/v1alpha1 is listed below deliberately, so a new RPC
// that forgets to be added here is caught immediately (it'll be
// admin-only until someone decides otherwise, never accidentally
// reader-accessible).
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
//
// The Controller's certificate (pki.RoleController) has no right of its
// own: it says whom it acts for - janus-as-user, janus-as-roles in the
// call's metadata - and the call gets that user's roles. Every call that
// isn't read-only is logged with who made it.
package api

import (
	"context"
	"log"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/internal/pki"
)

var (
	adminOnly = []string{pki.RoleAdmin}
	operators = []string{pki.RoleAdmin, pki.RoleOperator}
	readers   = []string{pki.RoleAdmin, pki.RoleOperator, pki.RoleReader}
)

var requiredRoles = map[string][]string{
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

// Caller is whom an RPC runs for.
type Caller struct {
	Name  string // the certificate's common name, or the user the Controller acts for
	Roles []string
	Via   string // the Controller certificate's name, when it acts for Name
}

func (c Caller) String() string {
	s := c.Name + " (" + strings.Join(c.Roles, ", ") + ")"
	if c.Via != "" {
		s += " via " + c.Via
	}
	return s
}

type callerKey struct{}

// CallerFrom is whom the RPC running with ctx is for.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// UnaryAuthInterceptor enforces requiredRoles for unary RPCs.
func UnaryAuthInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	caller, err := authorize(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	audit(caller, info.FullMethod)
	return handler(context.WithValue(ctx, callerKey{}, caller), req)
}

// StreamAuthInterceptor enforces requiredRoles for streaming RPCs.
func StreamAuthInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	caller, err := authorize(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	audit(caller, info.FullMethod)
	return handler(srv, callerStream{ss, context.WithValue(ss.Context(), callerKey{}, caller)})
}

type callerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s callerStream) Context() context.Context { return s.ctx }

// audit logs a call that isn't read-only, with whom it's for.
func audit(c Caller, fullMethod string) {
	if slices.Contains(rolesFor(fullMethod), pki.RoleReader) {
		return
	}
	log.Printf("api: %s: %s", strings.TrimPrefix(fullMethod, "/janus.v1alpha1."), c)
}

func rolesFor(fullMethod string) []string {
	if required, ok := requiredRoles[fullMethod]; ok {
		return required
	}
	// Fail closed: an RPC we forgot to classify is admin-only, never
	// silently open to readers.
	return adminOnly
}

func checkRole(ctx context.Context, fullMethod string) error {
	_, err := authorize(ctx, fullMethod)
	return err
}

// humanRoles are the roles the Controller may act with.
var humanRoles = []string{pki.RoleAdmin, pki.RoleOperator, pki.RoleReader}

// userNamePattern is a name the Controller may act for (as a client
// certificate's common name).
var userNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._@:+-]{0,63}$`)

// authorize finds whom the call is for and checks they may make it.
func authorize(ctx context.Context, fullMethod string) (Caller, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return Caller{}, status.Error(codes.Unauthenticated, "no peer information")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return Caller{}, status.Error(codes.Unauthenticated, "no client certificate presented")
	}
	leaf := tlsInfo.State.PeerCertificates[0]
	caller := Caller{Name: leaf.Subject.CommonName, Roles: leaf.Subject.Organization}

	if slices.Contains(leaf.Subject.Organization, pki.RoleController) {
		md, _ := metadata.FromIncomingContext(ctx)
		users, roles := md.Get(pki.AsUserKey), md.Get(pki.AsRolesKey)
		if len(users) != 1 || len(roles) != 1 || !userNamePattern.MatchString(users[0]) {
			return Caller{}, status.Errorf(codes.PermissionDenied, "a Controller certificate acts for a user: %s and %s are required", pki.AsUserKey, pki.AsRolesKey)
		}
		as := strings.Split(roles[0], ",")
		for _, r := range as {
			if !slices.Contains(humanRoles, r) {
				return Caller{}, status.Errorf(codes.PermissionDenied, "%s: unknown role %q", pki.AsRolesKey, r)
			}
		}
		caller = Caller{Name: users[0], Roles: as, Via: leaf.Subject.CommonName}
	}

	required := rolesFor(fullMethod)
	for _, have := range caller.Roles {
		if slices.Contains(required, have) {
			return caller, nil
		}
	}
	return Caller{}, status.Errorf(codes.PermissionDenied, "%s requires role %v, %s has %v", fullMethod, required, caller.Name, caller.Roles)
}
