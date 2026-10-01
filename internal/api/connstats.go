package api

import (
	"context"
	"time"

	"google.golang.org/grpc/stats"
)

// ConnStats is a grpc stats.Handler recording when each connection was
// opened, for the RPCs that must come over a connection newer than some
// change (FirewallConfirm: an established connection survives any
// ruleset, so only a new one proves the node is still reachable).
type ConnStats struct{}

type connOpenedKey struct{}

func (ConnStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return context.WithValue(ctx, connOpenedKey{}, time.Now())
}
func (ConnStats) HandleConn(context.Context, stats.ConnStats)                     {}
func (ConnStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (ConnStats) HandleRPC(context.Context, stats.RPCStats)                       {}

// ConnOpened is when the connection carrying ctx's RPC was opened - the
// zero time without ConnStats installed.
func ConnOpened(ctx context.Context) time.Time {
	t, _ := ctx.Value(connOpenedKey{}).(time.Time)
	return t
}
