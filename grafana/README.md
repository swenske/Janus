# Grafana

`janus-nodes.json` is the Grafana dashboard of the Janus exporter every
node serves - what it shows and how to load it:
[docs/metrics.md](../docs/metrics.md#grafana-dashboard).

`dashboard_test.go` builds it, and holds it to what janusd exports: a
metric it queries must exist, and a new metric gets a panel or a reason
not to. Change it there, then:

```sh
go test ./grafana -run TestDashboard -update
```

`make examples-check` parses its queries with promtool.
