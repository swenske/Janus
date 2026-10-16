package api

import (
	"context"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/pki"
)

// AuditLog is the node's audit: every call that isn't read-only, once
// it's done, and every call refused - an event whose type is the method
// ("SystemService/Reboot") and whose payload an AuditEntry. janusd sets
// it, kept on STATE on a node; nil: none.
var AuditLog *events.Log

// AuditCapacity is how many entries the audit keeps.
const AuditCapacity = 2000

// AuditEntry is one call, as the audit keeps it.
type AuditEntry struct {
	Caller  string   `json:"caller"`
	Roles   []string `json:"roles,omitempty"`
	Via     string   `json:"via,omitempty"` // the Controller that acted for Caller
	Fleet   bool     `json:"fleet,omitempty"`
	Scoped  bool     `json:"scoped,omitempty"`
	Domains []string `json:"domains,omitempty"`
	// Peer is the client's address.
	Peer string `json:"peer,omitempty"`
	// Code is how the call ended: OK, PermissionDenied, InvalidArgument...
	Code  string `json:"code"`
	Error string `json:"error,omitempty"`
	// Denied: refused before it ran.
	Denied     bool  `json:"denied,omitempty"`
	DurationMs int64 `json:"duration_ms"`
}

// recordCall adds a call to the audit: refused, or done and not
// read-only.
func recordCall(ctx context.Context, fullMethod string, c Caller, denied bool, err error, took time.Duration) {
	if AuditLog == nil || (!denied && slices.Contains(rolesFor(fullMethod), pki.RoleReader)) {
		return
	}
	if denied && c.Name == "" {
		c = presentedCaller(ctx)
	}
	st := status.Convert(err)
	e := AuditEntry{
		Caller: c.Name, Roles: c.Roles, Via: c.Via, Fleet: c.Fleet, Scoped: c.Scoped, Domains: c.Domains,
		Code: st.Code().String(), Denied: denied, DurationMs: took.Milliseconds(),
	}
	if err != nil {
		e.Error = st.Message()
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		e.Peer = p.Addr.String()
	}
	AuditLog.Publish(strings.TrimPrefix(fullMethod, "/janus.v1alpha1."), e)
}

// presentedCaller is who a refused call says it is: its certificate's
// name and roles - or, from a Controller, the user it acts for.
func presentedCaller(ctx context.Context) Caller {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return Caller{Name: "(no peer)"}
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return Caller{Name: "(no certificate)"}
	}
	leaf := info.State.PeerCertificates[0]
	c := Caller{Name: leaf.Subject.CommonName, Roles: leaf.Subject.Organization}
	if slices.Contains(c.Roles, pki.RoleController) {
		md, _ := metadata.FromIncomingContext(ctx)
		if u := md.Get(pki.AsUserKey); len(u) == 1 {
			c = Caller{Name: u[0], Via: leaf.Subject.CommonName}
			if r := md.Get(pki.AsRolesKey); len(r) == 1 {
				c.Roles = strings.Split(r[0], ",")
			}
		}
	}
	return c
}

// Audit streams the audit: what it holds after since_id, then new
// entries as they come.
func (s *System) Audit(req *janusv1alpha1.EventsRequest, stream janusv1alpha1.SystemService_AuditServer) error {
	if AuditLog == nil {
		return status.Error(codes.Unavailable, "this janusd keeps no audit")
	}
	return streamLog(stream.Context(), AuditLog, req.GetSinceId(), stream.Send)
}
