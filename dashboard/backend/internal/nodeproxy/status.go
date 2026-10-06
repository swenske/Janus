package nodeproxy

import (
	"context"
	"sync"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// NodeStatus is the at-a-glance state the node list shows for each node.
type NodeStatus struct {
	Reachable     bool   `json:"reachable"`
	Error         string `json:"error,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	Version       string `json:"version,omitempty"`
	ActiveSlot    string `json:"active_slot,omitempty"`
	KernelVersion string `json:"kernel_version,omitempty"`
	// KernelTrack, HAProxyVersion and HAProxyBranch: what the node's image
	// says it is built with (VersionResponse) - empty for a node that
	// doesn't say.
	KernelTrack     string `json:"kernel_track,omitempty"`
	HAProxyVersion  string `json:"haproxy_version,omitempty"`
	HAProxyBranch   string `json:"haproxy_branch,omitempty"`
	BootTimeUnix    uint64 `json:"boot_time_unix,omitempty"`
	HAProxyState    string `json:"haproxy_state,omitempty"`
	HAProxyHealth   string `json:"haproxy_health,omitempty"`
	LatestRelease   string `json:"latest_release,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	// SecurityUpdate: the newer releases fix vulnerabilities this node
	// has, the worst this severe (ReleaseInfo.SecurityUpdate).
	SecurityUpdate string `json:"security_update,omitempty"`
	// Support: the HAProxy branch the node's schematic pins leaves the
	// releases' offer soon, or left it (EndOfSupport).
	Support *Support `json:"support,omitempty"`
}

// Status queries node over its shared connection. ctx bounds the whole
// thing - the node list shouldn't wait on one unreachable node.
func Status(ctx context.Context, node *store.Node) NodeStatus {
	conn, err := dialNode(node)
	if err != nil {
		return NodeStatus{Error: err.Error()}
	}
	c := janusv1alpha1.NewSystemServiceClient(conn)
	empty := &emptypb.Empty{}

	var (
		st             NodeStatus
		img            NodeImage
		mu             sync.Mutex
		wg             sync.WaitGroup
		firstErr       error
		haveVersionErr bool
	)
	record := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	wg.Add(4)
	go func() {
		defer wg.Done()
		v, err := c.Version(ctx, empty)
		if err != nil {
			record(err)
			mu.Lock()
			haveVersionErr = true
			mu.Unlock()
			return
		}
		mu.Lock()
		st.Version, st.ActiveSlot, st.KernelVersion = v.GetVersion(), v.GetActiveSlot(), v.GetKernelVersion()
		st.KernelTrack = v.GetKernel().GetVariant()
		st.HAProxyVersion, st.HAProxyBranch = v.GetHaproxy().GetVersion(), v.GetHaproxy().GetVariant()
		sc := NodeSchematic(v)
		img = NodeImage{Extensions: sc.Extensions(), HAProxy: sc.HAProxyBranch(), Kernel: sc.KernelTrack()}
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		h, err := c.Hostname(ctx, empty)
		if err == nil {
			mu.Lock()
			st.Hostname = h.GetHostname()
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		s, err := c.SystemStat(ctx, empty)
		if err == nil {
			mu.Lock()
			st.BootTimeUnix = s.GetBootTimeUnix()
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		services, err := c.ServiceList(ctx, empty)
		if err != nil {
			return
		}
		for _, s := range services.GetServices() {
			if s.GetId() == "haproxy" {
				mu.Lock()
				st.HAProxyState, st.HAProxyHealth = s.GetState(), s.GetHealth()
				mu.Unlock()
			}
		}
	}()
	wg.Wait()

	if haveVersionErr {
		st.Error = status.Convert(firstErr).Message()
		return st
	}
	st.Reachable = true
	if rel, err := getLatestRelease(ctx); err == nil {
		st.LatestRelease = rel.TagName
		st.UpdateAvailable = rel.TagName != "" && rel.TagName != st.Version
		st.SecurityUpdate, _ = rel.SecurityUpdate(st.Version, "node", img)
	}
	if sup := EndOfSupport(ctx, img.HAProxy); sup != nil && (sup.Soon || sup.Retired) {
		st.Support = sup
	}
	return st
}
