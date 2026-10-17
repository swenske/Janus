package api

import (
	"context"
	"time"

	"google.golang.org/grpc/stats"
)

// ConnStats is a grpc stats.Handler recording when each connection was
// opened, for the RPCs that must come over a connection newer than some
// change (FirewallConfirm: an established connection survives any
// ruleset, so only a new one proves the node is still reachable) - and,
// with Clients, the connections open and their calls (ClientList).
type ConnStats struct {
	Clients *Clients
}

type connOpenedKey struct{}

func (s ConnStats) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	ctx = context.WithValue(ctx, connOpenedKey{}, time.Now())
	if s.Clients != nil {
		ctx = s.Clients.tag(ctx, info)
	}
	return ctx
}

func (s ConnStats) HandleConn(ctx context.Context, st stats.ConnStats) {
	if _, end := st.(*stats.ConnEnd); end && s.Clients != nil {
		s.Clients.end(ctx)
	}
}

func (s ConnStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	if s.Clients != nil {
		ctx = s.Clients.tagRPC(ctx)
	}
	return ctx
}

func (s ConnStats) HandleRPC(ctx context.Context, st stats.RPCStats) {
	if s.Clients != nil {
		s.Clients.handleRPC(ctx, st)
	}
}

// ConnOpened is when the connection carrying ctx's RPC was opened - the
// zero time without ConnStats installed.
func ConnOpened(ctx context.Context) time.Time {
	t, _ := ctx.Value(connOpenedKey{}).(time.Time)
	return t
}
