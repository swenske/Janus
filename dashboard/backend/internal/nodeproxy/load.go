package nodeproxy

import (
	"context"
	"errors"
	"sync"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Load is one reading of a node's load, for auto-scaling
// (dashboard/backend: autoscale.go).
type Load struct {
	// CPUs online, and their cumulative ticks: the busy share between two
	// readings is 1 - d(idle)/d(total).
	CPUs                        int
	CPUTotalTicks, CPUIdleTicks uint64
	MemTotal, MemAvailable      uint64 // bytes
	OOMKills                    uint64
	// HAProxy: its idle time (show info's Idle_pct), its connections and
	// limit - IdlePercent -1 when it doesn't answer.
	IdlePercent        float64
	CurrConns, MaxConn uint64
}

// ReadLoad reads node's load: its CPUs, memory and HAProxy, in parallel.
// HAProxy not answering isn't an error - IdlePercent says so.
func ReadLoad(ctx context.Context, node *store.Node) (*Load, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	hap := janusv1alpha1.NewHAProxyServiceClient(conn)
	l := &Load{IdlePercent: -1}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, errors.New(status.Convert(err).Message()))
	}
	wg.Add(4)
	go func() {
		defer wg.Done()
		st, err := sys.SystemStat(ctx, &emptypb.Empty{})
		if err != nil {
			fail(err)
			return
		}
		l.CPUTotalTicks, l.CPUIdleTicks = st.GetCpuTotalTicks(), st.GetCpuIdleTicks()
	}()
	go func() {
		defer wg.Done()
		m, err := sys.Memory(ctx, &emptypb.Empty{})
		if err != nil {
			fail(err)
			return
		}
		l.MemTotal, l.MemAvailable, l.OOMKills = m.GetTotalBytes(), m.GetAvailableBytes(), m.GetOomKills()
	}()
	go func() {
		defer wg.Done()
		c, err := sys.CPUInfo(ctx, &emptypb.Empty{})
		if err != nil {
			fail(err)
			return
		}
		l.CPUs = len(c.GetCpus())
	}()
	go func() {
		defer wg.Done()
		i, err := hap.ShowInfo(ctx, &emptypb.Empty{})
		if err != nil {
			return
		}
		l.IdlePercent = float64(i.GetIdlePercent())
		l.CurrConns, l.MaxConn = uint64(i.GetCurrentConnections()), uint64(i.GetMaxConnections())
	}()
	wg.Wait()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return l, nil
}

// HAProxyConfig is node's HAProxy configuration.
func HAProxyConfig(ctx context.Context, node *store.Node) ([]byte, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	resp, err := janusv1alpha1.NewHAProxyServiceClient(conn).GetConfig(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, errors.New(status.Convert(err).Message())
	}
	return resp.GetConfig(), nil
}
