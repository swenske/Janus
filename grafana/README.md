# Grafana

`janus-nodes.json` is the Grafana dashboard of the Janus exporter every
node serves, `janus-clusters.json` a cluster's - its members' exporters
and the Controller's metrics. What they show and how to load them:
[docs/metrics.md](../docs/metrics.md#grafana-dashboard),
[the clusters'](../docs/metrics.md#the-clusters-dashboard).

`dashboard_test.go` and `clusters_test.go` build them, and hold them to
what janusd and the Controller export: a metric they query must exist,
and a new metric gets a panel or a reason not to. Change them there,
then:

```sh
go test ./grafana -run TestDashboard -update
```

`make examples-check` parses their queries with promtool.
