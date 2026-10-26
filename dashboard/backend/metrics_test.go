package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The clusters' health as the Controller sees it (metrics.go
// memberFamilies): what each member runs against the desired version,
// drift, released domains, a failed check, peers - what the alerts of
// examples/prometheus/janus-rules.yml and grafana/janus-clusters.json
// read.
func TestClusterHealthMetrics(t *testing.T) {
	a, f, nodes := clusterApp(t, "lb-1", "lb-2")
	c := newCluster(t, a, nodes, "lb-1", "lb-2")
	ctx := context.Background()
	l := `cluster_id="` + c.ID + `",cluster_name="edge"`
	lm := func(member string) string { return l + `,member="` + member + `"` }
	has := func(t *testing.T, m string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(m, w+"\n") {
				t.Errorf("metrics without %s:\n%s", w, m)
			}
		}
	}
	hasNot := func(t *testing.T, m string, prefixes ...string) {
		t.Helper()
		for _, p := range prefixes {
			if strings.Contains(m, p) {
				t.Errorf("metrics with %s:\n%s", p, m)
			}
		}
	}

	// Recorded, nothing applied nor checked yet: no version wanted, so
	// every member runs it; peers unknown.
	m := scrapeMetrics(t, a)
	has(t, m,
		`janus_cluster_desired_version{`+l+`} 0`,
		`janus_cluster_operation_failed{`+l+`} 0`,
		`janus_cluster_member_applied_version{`+lm("lb-1")+`} 0`,
		`janus_cluster_member_on_desired_version{`+lm("lb-2")+`} 1`,
		`janus_cluster_member_drift{`+lm("lb-1")+`,domain="haproxy"} 0`,
		`janus_cluster_member_drift{`+lm("lb-1")+`,domain="files"} 0`,
		`janus_cluster_member_error{`+lm("lb-1")+`} 0`,
		`janus_cluster_member_drained{`+lm("lb-1")+`} 0`,
	)
	hasNot(t, m, "janus_cluster_member_peers{", "janus_cluster_member_tables_synced{", "janus_cluster_member_checked_timestamp_seconds{")

	// Version 1 on both, then someone changes lb-2's haproxy.cfg after
	// taking it back, and lb-2 loses its peer.
	if err := a.clusters.Create(ctx, c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	f.nodes["lb-2"].cfg = []byte("global # by hand\n")
	f.nodes["lb-2"].info.Managed = []string{"files"}
	f.nodes["lb-2"].alone = true
	a.clusters.reconcile(ctx, c)
	m = scrapeMetrics(t, a)
	has(t, m,
		`janus_cluster_desired_version{`+l+`} 1`,
		`janus_cluster_member_applied_version{`+lm("lb-1")+`} 1`,
		`janus_cluster_member_applied_version{`+lm("lb-2")+`} 1`,
		`janus_cluster_member_on_desired_version{`+lm("lb-1")+`} 1`,
		`janus_cluster_member_drift{`+lm("lb-2")+`,domain="haproxy"} 1`,
		`janus_cluster_member_drift{`+lm("lb-2")+`,domain="files"} 0`,
		`janus_cluster_member_drift{`+lm("lb-1")+`,domain="haproxy"} 0`,
		`janus_cluster_member_released{`+lm("lb-2")+`,domain="haproxy"} 1`,
		`janus_cluster_member_released{`+lm("lb-2")+`,domain="files"} 0`,
		`janus_cluster_member_peers{`+lm("lb-1")+`} 1`,
		`janus_cluster_member_peers_up{`+lm("lb-1")+`} 1`,
		`janus_cluster_member_peers_up{`+lm("lb-2")+`} 0`,
		`janus_cluster_member_tables_synced{`+lm("lb-1")+`} 1`,
		`janus_cluster_member_error{`+lm("lb-2")+`} 0`,
	)
	if !strings.Contains(m, `janus_cluster_member_checked_timestamp_seconds{`+lm("lb-2")+`} `) {
		t.Errorf("no check time for lb-2:\n%s", m)
	}

	// lb-1 doesn't answer the next check: its error, its peers as they
	// were.
	f.fail["lb-1 ClusterGet"] = errors.New("unavailable")
	a.clusters.reconcile(ctx, c)
	has(t, scrapeMetrics(t, a),
		`janus_cluster_member_error{`+lm("lb-1")+`} 1`,
		`janus_cluster_member_peers_up{`+lm("lb-1")+`} 1`,
	)
	delete(f.fail, "lb-1 ClusterGet")

	// An apply that stops on lb-2: lb-1 runs version 2, the cluster still
	// wants 1, lb-2 is in error and so is the operation.
	v2, err := a.clusters.Store.NewVersion(c.ID, "admin", "v2", map[string][]byte{"haproxy.cfg": []byte("global # v2\n")})
	if err != nil {
		t.Fatal(err)
	}
	f.fail["lb-2 HAProxyApply"] = errors.New("refused")
	if !a.clusters.start(c.ID, "apply", "admin", func(ctx context.Context) error { return a.clusters.Apply(ctx, c.ID, v2.Version, nil, "admin") }) {
		t.Fatal("not started")
	}
	a.clusters.wait()
	has(t, scrapeMetrics(t, a),
		`janus_cluster_desired_version{`+l+`} 1`,
		`janus_cluster_operation_failed{`+l+`} 1`,
		`janus_cluster_operation_running{`+l+`} 0`,
		`janus_cluster_member_applied_version{`+lm("lb-1")+`} 2`,
		`janus_cluster_member_on_desired_version{`+lm("lb-1")+`} 0`,
		`janus_cluster_member_on_desired_version{`+lm("lb-2")+`} 1`,
		`janus_cluster_member_error{`+lm("lb-2")+`} 1`,
	)

	// No cluster: none of them.
	if err := a.clusters.Store.Remove(c.ID); err != nil {
		t.Fatal(err)
	}
	hasNot(t, scrapeMetrics(t, a), "janus_cluster_")
}
