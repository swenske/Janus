package main

import (
	"net/http"
	"slices"

	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/exporter"
)

// The Controller's Prometheus endpoint (docs/cluster.md section 6: an
// orchestrator of its own sees the figures the auto-scaling engine
// judges): GET /api/metrics, a reader's - a session or an API token
// (Prometheus: authorization Bearer). The clusters' members and load,
// read every 15 s by clusterScaler for every cluster, and their
// auto-scaling; what each member runs and how it is as the reconcile
// loop last saw it - the desired version or not, drift, a failed apply
// or check, its peers (what the alerts of examples/prometheus and the
// Grafana dashboard grafana/janus-clusters.json read); the text format
// the nodes' exporter writes.

func (a *app) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = exporter.Write(w, a.metricFamilies())
}

// ratio is a percentage as Prometheus wants it, 0 to 1; ok false when
// it isn't known (negative).
func ratio(pct float64) (float64, bool) {
	if pct < 0 {
		return 0, false
	}
	return pct / 100, true
}

func (a *app) metricFamilies() []exporter.Family {
	info := exporter.Family{Name: "janus_controller_build_info", Help: "The Controller's version.", Type: "gauge",
		Samples: []exporter.Sample{{Labels: exporter.L("version", version), Value: 1}}}
	members := exporter.Family{Name: "janus_cluster_members", Help: "A cluster's members.", Type: "gauge"}
	busy := exporter.Family{Name: "janus_cluster_operation_running", Help: "1 while an operation runs on the cluster (an apply, a scale, a recycle...).", Type: "gauge"}
	mode := exporter.Family{Name: "janus_cluster_autoscale_mode", Help: "The cluster's horizontal auto-scaling mode (observe or on), 1; absent when off.", Type: "gauge"}
	paused := exporter.Family{Name: "janus_cluster_autoscale_paused", Help: "1 while auto-scaling is paused - its members set from outside.", Type: "gauge"}
	judged := exporter.Family{Name: "janus_cluster_autoscale_judged_members", Help: "The member count auto-scaling judges: the cluster's, or the one it simulates while observing.", Type: "gauge"}
	read := exporter.Family{Name: "janus_cluster_load_members", Help: "Members that answered the last load reading.", Type: "gauge"}
	idle := exporter.Family{Name: "janus_cluster_load_haproxy_idle_ratio", Help: "HAProxy's idle time, the mean over the members.", Type: "gauge"}
	cpu := exporter.Family{Name: "janus_cluster_load_cpu_busy_ratio", Help: "The CPUs' busy time without steal, the mean over the members.", Type: "gauge"}
	steal := exporter.Family{Name: "janus_cluster_load_cpu_steal_ratio", Help: "The CPUs' steal time, the mean over the members.", Type: "gauge"}
	conns := exporter.Family{Name: "janus_cluster_load_connections_ratio", Help: "Connections over maxconn, the highest among the members.", Type: "gauge"}
	at := exporter.Family{Name: "janus_cluster_load_timestamp_seconds", Help: "When the last load reading was taken.", Type: "gauge"}
	desired := exporter.Family{Name: "janus_cluster_desired_version", Help: "The version of its configuration the cluster's members should run; 0 before the first.", Type: "gauge"}
	failed := exporter.Family{Name: "janus_cluster_operation_failed", Help: "1 when the cluster's last operation failed, until the next one starts.", Type: "gauge"}

	var list []*clusters.Cluster
	if a.clusters != nil {
		list = a.clusters.Store.List()
	}
	ms := newMemberFamilies()
	for _, c := range list {
		l := exporter.L("cluster_id", c.ID, "cluster_name", c.Name)
		members.Samples = append(members.Samples, exporter.Sample{Labels: l, Value: float64(len(c.Members))})
		busy.Samples = append(busy.Samples, exporter.Sample{Labels: l, Value: exporter.Bool(a.clusters.isBusy(c.ID))})
		desired.Samples = append(desired.Samples, exporter.Sample{Labels: l, Value: float64(c.Desired)})
		failed.Samples = append(failed.Samples, exporter.Sample{Labels: l, Value: exporter.Bool(c.Error != "")})
		st := a.clusters.Store.State(c.ID)
		ms.add(c, st)
		if as := c.Autoscale; as != nil && as.Mode != clusters.ScaleOff {
			mode.Samples = append(mode.Samples, exporter.Sample{Labels: exporter.L("cluster_id", c.ID, "cluster_name", c.Name, "mode", as.Mode), Value: 1})
			paused.Samples = append(paused.Samples, exporter.Sample{Labels: l, Value: exporter.Bool(as.PausedBy != "")})
			if n := st.Scale.Members; n > 0 {
				judged.Samples = append(judged.Samples, exporter.Sample{Labels: l, Value: float64(n)})
			}
		}
		s := a.clusterScaler.load(c.ID)
		if s == nil {
			continue
		}
		read.Samples = append(read.Samples, exporter.Sample{Labels: l, Value: float64(s.Members)})
		at.Samples = append(at.Samples, exporter.Sample{Labels: l, Value: float64(s.At.UnixMilli()) / 1000})
		for _, x := range []struct {
			f   *exporter.Family
			pct float64
		}{{&idle, s.IdlePercent}, {&cpu, s.BusyPercent}, {&steal, s.StealPercent}, {&conns, s.ConnPercent}} {
			if v, ok := ratio(x.pct); ok {
				x.f.Samples = append(x.f.Samples, exporter.Sample{Labels: l, Value: v})
			}
		}
	}
	return append([]exporter.Family{info, members, busy, desired, failed, mode, paused, judged, read, idle, cpu, steal, conns, at}, ms.families()...)
}

// memberFamilies are each member's: what it runs against the cluster's
// desired version, drift, the Controller's last apply or check on it,
// and its peers - the cluster's state (clusters.State) as the reconcile
// loop last saw it, every 10 minutes and after each operation. Labels:
// cluster_id, cluster_name, member (+ domain), like janus_cluster_info
// on the nodes.
type memberFamilies struct {
	applied, current, drift, released, failing, drained, peers, peersUp, synced, checked exporter.Family
}

func newMemberFamilies() *memberFamilies {
	g := func(name, help string) exporter.Family {
		return exporter.Family{Name: name, Help: help, Type: "gauge"}
	}
	return &memberFamilies{
		applied:  g("janus_cluster_member_applied_version", "The version of the cluster's configuration the member last took whole; 0: none."),
		current:  g("janus_cluster_member_on_desired_version", "1 when the member runs the cluster's desired version."),
		drift:    g("janus_cluster_member_drift", "1 for each managed domain whose content on the member isn't what was applied: changed on the node since."),
		released: g("janus_cluster_member_released", "1 for each managed domain the member's own users took back (ClusterRelease)."),
		failing:  g("janus_cluster_member_error", "1 when the Controller's last apply or check on the member failed."),
		drained:  g("janus_cluster_member_drained", "1 while the member is drained: out of service."),
		peers:    g("janus_cluster_member_peers", "The member's HAProxy peers, as the Controller last saw them."),
		peersUp:  g("janus_cluster_member_peers_up", "Those of its peers it was connected to, as the Controller last saw them."),
		synced:   g("janus_cluster_member_tables_synced", "1 when every peer had acknowledged every stick table, as the Controller last saw them."),
		checked:  g("janus_cluster_member_checked_timestamp_seconds", "When the Controller last checked the member."),
	}
}

// add is cluster c's members, st their state.
func (f *memberFamilies) add(c *clusters.Cluster, st clusters.State) {
	put := func(fam *exporter.Family, l []exporter.Label, v float64) {
		fam.Samples = append(fam.Samples, exporter.Sample{Labels: l, Value: v})
	}
	for _, m := range c.Members {
		s := st.Members[m.Name]
		l := exporter.L("cluster_id", c.ID, "cluster_name", c.Name, "member", m.Name)
		put(&f.applied, l, float64(s.Applied))
		put(&f.current, l, exporter.Bool(c.Desired == 0 || s.Applied == c.Desired))
		for _, d := range c.Managed {
			ld := exporter.L("cluster_id", c.ID, "cluster_name", c.Name, "member", m.Name, "domain", d)
			put(&f.drift, ld, exporter.Bool(slices.Contains(s.Drift, d)))
			put(&f.released, ld, exporter.Bool(slices.Contains(s.Released, d)))
		}
		put(&f.failing, l, exporter.Bool(s.Error != ""))
		put(&f.drained, l, exporter.Bool(s.Drained))
		if s.Checked.IsZero() {
			continue // never looked at: its peers aren't known
		}
		put(&f.peers, l, float64(s.PeersTotal))
		put(&f.peersUp, l, float64(s.PeersUp))
		put(&f.synced, l, exporter.Bool(s.TablesSynced))
		put(&f.checked, l, float64(s.Checked.UnixMilli())/1000)
	}
}

func (f *memberFamilies) families() []exporter.Family {
	return []exporter.Family{f.applied, f.current, f.drift, f.released, f.failing, f.drained, f.peers, f.peersUp, f.synced, f.checked}
}
