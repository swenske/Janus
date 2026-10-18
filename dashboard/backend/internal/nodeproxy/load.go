package nodeproxy

import (
	"context"
	"errors"
	"sync"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/nodemetrics"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// Load is one reading of a node's load, for auto-scaling
// (dashboard/backend: autoscale.go).
type Load struct {
	// CPUs online, and their cumulative ticks: the busy share between two
	// readings is (d(total) - d(idle) - d(steal)) / d(total). Steal: the
	// time the hypervisor ran something else while a vCPU had work - zero
	// from a node before v2026.10.14-2.
	CPUs                                       int
	CPUTotalTicks, CPUIdleTicks, CPUStealTicks uint64
	MemTotal, MemAvailable                     uint64 // bytes
	OOMKills                                   uint64
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
		l.CPUTotalTicks, l.CPUIdleTicks, l.CPUStealTicks = st.GetCpuTotalTicks(), st.GetCpuIdleTicks(), st.GetCpuStealTicks()
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

// ReadCounters reads what the node list's curves are drawn from
// (internal/nodemetrics): node's CPU ticks, the bytes its interfaces
// moved (the loopback's aside) and HAProxy's requests, in parallel.
// HAProxy not answering isn't an error - Counters.HAProxy says so.
func ReadCounters(ctx context.Context, node *store.Node) (*nodemetrics.Counters, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	hap := janusv1alpha1.NewHAProxyServiceClient(conn)
	c := &nodemetrics.Counters{}
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
	wg.Add(3)
	go func() {
		defer wg.Done()
		st, err := sys.SystemStat(ctx, &emptypb.Empty{})
		if err != nil {
			fail(err)
			return
		}
		c.TotalTicks, c.IdleTicks = st.GetCpuTotalTicks(), st.GetCpuIdleTicks()
	}()
	go func() {
		defer wg.Done()
		n, err := sys.NetworkDeviceStats(ctx, &emptypb.Empty{})
		if err != nil {
			fail(err)
			return
		}
		for _, d := range n.GetDevices() {
			if d.GetName() != "lo" {
				c.RxBytes += d.GetRxBytes()
				c.TxBytes += d.GetTxBytes()
			}
		}
	}()
	go func() {
		defer wg.Done()
		i, err := hap.ShowInfo(ctx, &emptypb.Empty{})
		if err != nil {
			return
		}
		c.Requests, c.HAProxy = i.GetCumulativeRequests(), true
	}()
	wg.Wait()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return c, nil
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
