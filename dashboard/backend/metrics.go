package main

import (
	"net/http"

	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/exporter"
)

// The Controller's Prometheus endpoint (docs/cluster.md section 6: an
// orchestrator of its own sees the figures the auto-scaling engine
// judges): GET /api/metrics, a reader's - a session or an API token
// (Prometheus: authorization Bearer). The clusters' members and load,
// read every 15 s by clusterScaler for every cluster, and their
// auto-scaling; the text format the nodes' exporter writes.

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

	var list []*clusters.Cluster
	if a.clusters != nil {
		list = a.clusters.Store.List()
	}
	for _, c := range list {
		l := exporter.L("cluster_id", c.ID, "cluster_name", c.Name)
		members.Samples = append(members.Samples, exporter.Sample{Labels: l, Value: float64(len(c.Members))})
		busy.Samples = append(busy.Samples, exporter.Sample{Labels: l, Value: exporter.Bool(a.clusters.isBusy(c.ID))})
		if as := c.Autoscale; as != nil && as.Mode != clusters.ScaleOff {
			mode.Samples = append(mode.Samples, exporter.Sample{Labels: exporter.L("cluster_id", c.ID, "cluster_name", c.Name, "mode", as.Mode), Value: 1})
			paused.Samples = append(paused.Samples, exporter.Sample{Labels: l, Value: exporter.Bool(as.PausedBy != "")})
			if n := a.clusters.Store.State(c.ID).Scale.Members; n > 0 {
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
	return []exporter.Family{info, members, busy, mode, paused, judged, read, idle, cpu, steal, conns, at}
}
