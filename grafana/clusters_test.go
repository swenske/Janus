package grafana

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// janus-clusters.json, the Grafana dashboard of a cluster (docs/cluster.md,
// docs/metrics.md): its members as their Janus exporters tell it - joined
// to janus_cluster_info, the one metric that names the cluster and the
// member -, the virtual address, peers and stick tables, the members'
// traffic and load from HAProxy's own exporter and node_exporter, and the
// cluster as the Controller sees it (GET /api/metrics, its own job). Built
// with janus-nodes.json (dashboard_test.go):
//
//	go test ./grafana -run TestDashboard -update

const clustersFile = "janus-clusters.json"

// The clusters dashboard's selectors, by what serves the metric: the Janus
// exporter's job, the Controller's job and cluster, HAProxy's and
// node_exporter's jobs.
const (
	janusJob = `job="$job"`
	ctl      = `job="$controller", cluster_name="$cluster"`
	hapJob   = `job="$haproxy"`
	nodeJob  = `job="$node"`
	// info: the cluster's members now, their member name.
	info = `janus_cluster_info{` + janusJob + `, cluster_name="$cluster"}`
)

// members: info, and for an hour the last membership of a node whose
// exporter stopped answering - a member down stays in the table, a node
// that left doesn't.
const members = `(` + info + ` or (last_over_time(` + info + `[1h]) unless on (instance) up{` + janusJob + `} == 1))`

// externalMetrics are the metrics of other exporters the dashboard
// queries, and whose: HAProxy's own (Janus's HAProxy is built with it,
// docs/metrics.md "HAProxy's own metrics" - names checked on 3.4.7) and
// node_exporter (the prometheus-node-exporter extension).
var externalMetrics = map[string]string{
	"haproxy_frontend_sessions_total":      "HAProxy",
	"haproxy_frontend_http_requests_total": "HAProxy",
	"haproxy_process_current_connections":  "HAProxy",
	"haproxy_process_idle_time_percent":    "HAProxy",
	"haproxy_process_nbthread":             "HAProxy (the HAProxy job variable)",
	"node_cpu_seconds_total":               "node_exporter",
	"node_memory_MemAvailable_bytes":       "node_exporter",
	"node_memory_MemTotal_bytes":           "node_exporter",
	"node_uname_info":                      "node_exporter (the node_exporter job variable)",
}

// The metrics the Controller exports that no panel shows, and why.
var controllerNotShown = map[string]string{
	"janus_controller_build_info":          "the Controller's own release, not a cluster's",
	"janus_cluster_autoscale_mode":         "the auto-scaling's policy: the cluster's page says it",
	"janus_cluster_autoscale_paused":       "the auto-scaling's policy: the cluster's page says it",
	"janus_cluster_load_timestamp_seconds": "the load panel draws each reading at its time",
}

// of keeps expr, a node's metric by instance, to the cluster's members,
// and gives it their member label.
func of(expr string) string {
	return "(" + expr + ") * on (instance) group_left (member) " + info
}

// ofMembers is of over members: a member whose exporter is silent too.
func ofMembers(expr string) string {
	return "(" + expr + ") * on (instance) group_left (member) " + members
}

// host is expr with its instance's host as label host: "lb1.example.net:8405"
// -> "lb1.example.net" - how a member's other exporters are found, on the
// same host as its Janus exporter, on their own port.
func host(expr string) string {
	return `label_replace(` + expr + `, "host", "$1", "instance", "(.+?)(?::[0-9]+)?")`
}

// onHost keeps expr, another exporter's metric by instance, to the
// cluster's members - by host - and gives it their member label.
func onHost(expr string) string {
	return host(expr) + " * on (host) group_left (member) " + host(info)
}

// encodeOf is encode over the cluster's members.
func encodeOf(metric, by string, states []string) string {
	terms := make([]string, len(states))
	for i, s := range states {
		terms[i] = fmt.Sprintf(`sum by (%s) (%s)`, by, of(fmt.Sprintf(`%s{%s, state="%s"}`, metric, janusJob, s)))
		if i > 0 {
			terms[i] = fmt.Sprintf("%d * %s", i+1, terms[i])
		}
	}
	return strings.Join(terms, " + ")
}

// incrementsOf is increments over the cluster's members.
func incrementsOf(metric, by string) string {
	s := metric + "{" + janusJob + "}"
	return fmt.Sprintf("(sum by (%s) (%s) - sum by (%s) (%s offset $__interval * on (instance) group_left (member) %s offset $__interval)) > 0",
		by, of(s), by, s, info)
}

// rangeMapping names and colors the values from..to (nil: unbounded),
// index-th in the legend.
func rangeMapping(from, to any, text, color string, index int) obj {
	return obj{"type": "range", "options": obj{"from": from, "to": to, "result": obj{"text": text, "color": color, "index": index}}}
}

// A state timeline without data says "Data does not have a time field",
// not its no-value text (Grafana 11.6 to 13.0), and one whose every series
// is hidden fails: the cluster's timelines draw -1 instead - "none" - for
// each member without data (orNone, orNoneBy) or the cluster
// (orNoneCluster), while it has members.

// orNone is expr, by member and label, or -1 with label "-" for each member
// without any.
func orNone(expr, label string) string {
	return expr + ` or on (member) label_replace(max by (member) (0 * ` + info + `) - 1, "` + label + `", "-", "", "")`
}

// orNoneBy is expr, by member, or -1 for each member without it.
func orNoneBy(expr string) string {
	return expr + " or on (member) (max by (member) (0 * " + info + ") - 1)"
}

// orNoneCluster is expr or, when it has nothing, -1 with label "-"
// (label "": none).
func orNoneCluster(expr, label string) string {
	none := "(0 * count(" + info + ") - 1)"
	if label != "" {
		none = `label_replace(` + none + `, "` + label + `", "-", "", "")`
	}
	return expr + " or on () " + none
}

// plain: legends that are member names, not hosts to shorten.
func plain(p obj) obj {
	p["transformations"] = []any{}
	return p
}

// dashboardsLink lists the other Janus dashboards: nodes, clusters.
var dashboardsLink = obj{"title": "Janus dashboards", "type": "dashboards", "tags": []any{"janus"}, "asDropdown": true,
	"includeVars": false, "keepTime": true, "icon": "external link", "targetBlank": false, "tooltip": ""}

// narrow lets a table's columns be narrower than Grafana's 150 px: a
// member's line holds them all.
func narrow(p obj) obj {
	defaults(p)["custom"].(obj)["minWidth"] = 60
	return p
}

// noValue is what a panel says without data.
func noValue(p obj, text string) obj {
	defaults(p)["noValue"] = text
	return p
}

func clustersDashboard() obj {
	b := &builder{}
	green1Red := thresholds("green", 1, "red")
	fixed := func(color string) obj { return obj{"mode": "fixed", "fixedColor": color} }
	const noController = "Controller not scraped"
	// The -1 of orNone and its kin.
	noVRRP := state{-1, "no VRRP", "transparent"}
	noControllerState := state{-1, "no Controller data", "transparent"}

	// The cluster now.
	b.add(stat("Members",
		"The cluster's members whose Janus exporter answers (janus_cluster_info), those that stopped answering "+
			"within the hour, and the members the Controller records - when it is scraped.",
		b.pos(4, 4, 0), []q{
			{ref: "A", expr: "count(" + info + ") or vector(0)", legend: "answering", instant: true},
			{ref: "B", expr: "count(" + members + " unless on (instance) " + info + ") or vector(0)", legend: "silent", instant: true},
			{ref: "C", expr: "max(janus_cluster_members{" + ctl + "})", legend: "Controller", instant: true},
		}, thresholds("text"), byRef("B", prop("thresholds", green1Red))))
	vip := b.add(stat("Virtual address",
		"The member holding each VRRP instance (MASTER) - [VRRP]("+docs+"vrrp.md), the keepalived extension. "+
			"Two names for one instance: split brain. Nothing: no member holds it, or the cluster has no VRRP.",
		b.pos(4, 4, 4), []q{
			{ref: "A", expr: "max by (exported_instance, member) (" + of(`janus_vrrp_instance_state{`+janusJob+`, state="MASTER"}`) + " == 1)",
				legend: "{{exported_instance}} · {{member}}", instant: true},
		}, thresholds("green")))
	vip["options"].(obj)["textMode"] = "name"
	noValue(vip, "-")
	masters := b.add(stat("VRRP masters",
		"Members that are MASTER of each VRRP instance: one. None: nobody answers for the address; two or more: "+
			"split brain (the JanusClusterVRRPNoMaster and JanusClusterVRRPSplitBrain alerts).",
		b.pos(4, 4, 8), []q{
			{ref: "A", expr: "sum by (exported_instance) (" + of(`janus_vrrp_instance_state{`+janusJob+`, state="MASTER"}`) + ")",
				legend: "{{exported_instance}}", instant: true},
		}, thresholds("red", 1, "green", 2, "red")))
	defaults(masters)["mappings"] = []any{mapping(state{0, "no master", "red"}, state{1, "one master", "green"}),
		rangeMapping(2, nil, "split brain", "red", 2)}
	masters["options"].(obj)["textMode"] = "value_and_name"
	noValue(masters, "no VRRP")
	b.add(stat("Peers",
		"HAProxy's connections from each member to the others (janus_cluster_peer_up): the stick tables replicate "+
			"over them.",
		b.pos(4, 3, 12), []q{
			{ref: "A", expr: "sum(" + of(`janus_cluster_peer_up{`+janusJob+`}`) + ") or vector(0)", legend: "up", instant: true},
			{ref: "B", expr: "count(" + of(`janus_cluster_peer_up{`+janusJob+`}`) + " == 0) or vector(0)", legend: "down", instant: true},
		}, thresholds("text"), byRef("B", prop("thresholds", green1Red))))
	b.add(stat("Stick tables",
		"Each member's stick tables, per peer: synced while the peer keeps up with the member's updates "+
			"(janus_cluster_peer_table_synced).",
		b.pos(4, 4, 15), []q{
			{ref: "A", expr: "count(" + of(`janus_cluster_peer_table_synced{`+janusJob+`}`) + " == 1) or vector(0)", legend: "synced", instant: true},
			{ref: "B", expr: "count(" + of(`janus_cluster_peer_table_synced{`+janusJob+`}`) + " == 0) or vector(0)", legend: "not synced", instant: true},
		}, thresholds("text"), byRef("B", prop("thresholds", thresholds("green", 1, "orange")))))
	cfg := b.add(stat("Configuration",
		"The Controller's view ([clusters]("+docs+"guide/clusters.md)): the version its members should run, the "+
			"members that don't, those whose configuration changed on the node (drift), and those its last apply or "+
			"check failed on.",
		b.pos(4, 5, 19), []q{
			{ref: "A", expr: "max(janus_cluster_desired_version{" + ctl + "})", legend: "version", instant: true},
			{ref: "B", expr: "count(janus_cluster_member_on_desired_version{" + ctl + "} == 0) or 0 * max(janus_cluster_desired_version{" + ctl + "})",
				legend: "other version", instant: true},
			{ref: "C", expr: "count(max by (member) (janus_cluster_member_drift{" + ctl + "}) == 1) or 0 * max(janus_cluster_desired_version{" + ctl + "})",
				legend: "drifted", instant: true},
			{ref: "D", expr: "count(janus_cluster_member_error{" + ctl + "} == 1) or 0 * max(janus_cluster_desired_version{" + ctl + "})",
				legend: "failing", instant: true},
		}, thresholds("text", 1, "red"), byRef("A", prop("thresholds", thresholds("text")))))
	noValue(cfg, noController)
	b.y += 4

	// The members.
	b.row("Members", false)
	vrrpOrder := []string{"MASTER", "FAULT", "BACKUP", "INIT", "STOP"}
	nodeCols := []struct{ ref, expr, column string }{
		{"A", "max by (instance, member) (" + ofMembers(`up{`+janusJob+`}`) + ")", "Exporter"},
		{"B", "max by (instance, member, version) (" + of(`janus_build_info{`+janusJob+`}`) + ")", ""},
		{"C", "max by (instance, member) (" + of(`janus_haproxy_up{`+janusJob+`}`) + ")", "HAProxy"},
		{"D", "min by (instance, member) (" + encodeOf("janus_vrrp_instance_state", "instance, member, exported_instance", vrrpOrder) + ")", "VRRP"},
		{"E", "max by (instance, member) (" + of(`janus_vrrp_instance_effective_priority{`+janusJob+`}`) + ")", "Priority"},
		{"F", "sum by (instance, member) (" + of(`janus_cluster_peer_up{`+janusJob+`}`) + ")", "Peers up"},
		{"G", "sum by (instance, member) (" + of(`1 - janus_cluster_peer_up{`+janusJob+`}`) + ")", "Peers down"},
		{"H", "min by (instance, member) (" + of(`janus_cluster_peer_table_synced{`+janusJob+`}`) + ")", "Stick tables"},
	}
	var nodeQs []q
	exclude := []string{"Time"}
	rename := map[string]string{"member": "Member", "instance": "Node", "version": "Janus"}
	order := []string{"member", "instance", "version"}
	for _, c := range nodeCols {
		nodeQs = append(nodeQs, q{ref: c.ref, expr: c.expr, table: true})
		v := "Value #" + c.ref
		if c.column == "" {
			exclude = append(exclude, v)
			continue
		}
		rename[v] = c.column
		order = append(order, v)
	}
	b.add(narrow(table("Members",
		"One line per member, as its Janus exporter tells it - a member whose exporter stopped answering stays an "+
			"hour, Exporter down. VRRP: MASTER when it holds one of the cluster's VRRP instances at least, else its "+
			"worst state; Priority: its highest, after tracking. Stick tables: synced when every peer keeps up with "+
			"every table. - : none (no VRRP, a member alone).",
		b.pos(7, 24, 0), nodeQs, []any{merge, organize(exclude, rename, order...)}, "Member",
		byName("Member", prop("custom.width", 140)),
		byName("Node", prop("mappings", shortNameMappings), prop("custom.width", 140)),
		byName("Janus", prop("custom.width", 150)),
		byName("Exporter", prop("mappings", []any{mapping(state{1, "up", "green"}, state{0, "down", "red"})}),
			prop("custom.cellOptions", colorText)),
		byName("HAProxy", prop("mappings", []any{mapping(state{1, "serving", "green"}, state{0, "down", "red"})}),
			prop("custom.cellOptions", colorText)),
		byName("VRRP", prop("mappings", []any{mapping(state{1, "MASTER", "green"}, state{2, "FAULT", "red"}, state{3, "BACKUP", "blue"},
			state{4, "INIT", "orange"}, state{5, "STOP", "text"})}), prop("custom.cellOptions", colorText), prop("noValue", "-")),
		byName("Priority", prop("decimals", 0), prop("noValue", "-")),
		byName("Peers up", prop("decimals", 0), prop("noValue", "-")),
		byName("Peers down", prop("decimals", 0), prop("noValue", "-"), prop("thresholds", green1Red), prop("custom.cellOptions", colorText)),
		byName("Stick tables", prop("mappings", []any{mapping(state{1, "synced", "green"}, state{0, "not synced", "orange"})}),
			prop("custom.cellOptions", colorText), prop("noValue", "-")),
	)))
	b.y += 7
	ctlCols := []struct{ ref, expr, column string }{
		{"A", "max by (member) (janus_cluster_member_applied_version{" + ctl + "})", "Version"},
		{"B", "max by (member) (janus_cluster_member_on_desired_version{" + ctl + "})", "Desired"},
		{"C", "sum by (member) (janus_cluster_member_drift{" + ctl + "})", "Drifted"},
		{"D", "sum by (member) (janus_cluster_member_released{" + ctl + "})", "Released"},
		{"E", "max by (member) (janus_cluster_member_error{" + ctl + "})", "Apply or check"},
		{"F", "max by (member) (janus_cluster_member_drained{" + ctl + "})", "Service"},
		{"G", "max by (member) (janus_cluster_member_peers_up{" + ctl + "})", "Peers up"},
		{"H", "max by (member) (janus_cluster_member_peers{" + ctl + "})", "Peers"},
		{"I", "max by (member) (janus_cluster_member_tables_synced{" + ctl + "})", "Stick tables"},
		{"J", "max by (member) (janus_cluster_member_checked_timestamp_seconds{" + ctl + "}) * 1000", "Checked"},
	}
	var ctlQs []q
	rename = map[string]string{"member": "Member"}
	order = []string{"member"}
	for _, c := range ctlCols {
		ctlQs = append(ctlQs, q{ref: c.ref, expr: c.expr, table: true})
		rename["Value #"+c.ref] = c.column
		order = append(order, "Value #"+c.ref)
	}
	ctlTable := b.add(narrow(table("Members as the Controller sees them",
		"What the [Controller]("+docs+"guide/clusters.md) records of each member - every 10 minutes, every "+
			"minute while one fails, and after each operation. Version: the one it last took whole; Desired: "+
			"whether that is the cluster's; Drifted: managed domains changed on the node since (the next apply "+
			"puts them back); Released: domains its users took back; Apply or check: the Controller's last on it "+
			"(the cluster's page says why it failed); Service: drained or not; then its peers.",
		b.pos(6, 24, 0), ctlQs, []any{merge, organize([]string{"Time"}, rename, order...)}, "Member",
		byName("Member", prop("custom.width", 140)),
		byName("Version", prop("decimals", 0)),
		byName("Desired", prop("mappings", []any{mapping(state{1, "runs it", "green"}, state{0, "runs another", "red"})}),
			prop("custom.cellOptions", colorText)),
		byName("Drifted", prop("decimals", 0), prop("thresholds", thresholds("green", 1, "orange")), prop("custom.cellOptions", colorText)),
		byName("Released", prop("decimals", 0), prop("thresholds", thresholds("green", 1, "orange")), prop("custom.cellOptions", colorText)),
		byName("Apply or check", prop("mappings", []any{mapping(state{0, "succeeded", "green"}, state{1, "failed", "red"})}),
			prop("custom.cellOptions", colorText)),
		byName("Service", prop("mappings", []any{mapping(state{0, "in service", "green"}, state{1, "drained", "blue"})}),
			prop("custom.cellOptions", colorText)),
		byName("Peers up", prop("decimals", 0), prop("noValue", "-")),
		byName("Peers", prop("decimals", 0), prop("noValue", "-")),
		byName("Stick tables", prop("mappings", []any{mapping(state{1, "synced", "green"}, state{0, "not synced", "orange"})}),
			prop("custom.cellOptions", colorText), prop("noValue", "-")),
		byName("Checked", prop("unit", "dateTimeFromNow"), prop("noValue", "never")),
	)))
	noValue(ctlTable, noController)
	b.y += 6

	// The virtual address.
	b.row("Virtual address", false)
	b.add(plain(timeline("Who holds the address",
		"Each member's state in each VRRP instance over time: the MASTER holds the virtual address.",
		b.pos(7, 12, 0), []q{
			{ref: "A", expr: orNone(encodeOf("janus_vrrp_instance_state", "exported_instance, member", []string{"MASTER", "BACKUP", "FAULT", "INIT", "STOP"}), "exported_instance"),
				legend: "{{exported_instance}} · {{member}}"},
		}, []state{{1, "MASTER", "green"}, {2, "BACKUP", "blue"}, {3, "FAULT", "red"}, {4, "INIT", "orange"}, {5, "STOP", "text"}, noVRRP}, true)))
	mastersOver := b.add(plain(timeline("Masters",
		"Members MASTER of each VRRP instance over time: one. None: nobody answered for the address; two or more: "+
			"split brain.",
		b.pos(7, 12, 12), []q{
			{ref: "A", expr: orNoneCluster("sum by (exported_instance) ("+of(`janus_vrrp_instance_state{`+janusJob+`, state="MASTER"}`)+")", "exported_instance"),
				legend: "{{exported_instance}}"},
		}, []state{{0, "no master", "red"}, {1, "one master", "green"}, noVRRP}, false)))
	defaults(mastersOver)["mappings"] = append(defaults(mastersOver)["mappings"].([]any), rangeMapping(2, nil, "split brain", "red", 2))
	b.y += 7
	b.add(plain(eventsOver(info, "Failovers",
		"Times a member became MASTER of a VRRP instance (janus_vrrp_instance_became_master_total), per interval: "+
			"it took the address over. Nothing drawn: no failover.",
		b.pos(6, 12, 0), []q{
			{ref: "A", expr: incrementsOf("janus_vrrp_instance_became_master_total", "exported_instance, member"),
				legend: "{{member}} took {{exported_instance}}"},
		})))
	prio := b.add(plain(timeseries("VRRP priority",
		"Each member's priority in each VRRP instance after tracking: HAProxy not serving lowers it (track_file), "+
			"and the highest becomes MASTER.",
		b.pos(6, 12, 12), []q{
			{ref: "A", expr: "max by (exported_instance, member) (" + of(`janus_vrrp_instance_effective_priority{`+janusJob+`}`) + ")",
				legend: "{{exported_instance}} · {{member}}"},
		}, "none")))
	defaults(prio)["decimals"] = 0
	b.y += 6

	// Peers and stick tables.
	b.row("Peers and stick tables", false)
	b.add(plain(timeline("Peers",
		"HAProxy's connection from each member to each of its peers over time ([clusters]("+docs+"guide/clusters.md)).",
		b.pos(7, 12, 0), []q{
			{ref: "A", expr: orNone("max by (member, peer) ("+of(`janus_cluster_peer_up{`+janusJob+`}`)+")", "peer"), legend: "{{member}} → {{peer}}"},
		}, []state{{1, "up", "green"}, {0, "down", "red"}, {-1, "no peer", "transparent"}}, false)))
	b.add(plain(timeline("Stick tables",
		"Each member's stick tables over time: synced while every peer keeps up with its updates - all "+
			"acknowledged, or under traffic within two seconds every one made before.",
		b.pos(7, 12, 12), []q{
			{ref: "A", expr: orNone("min by (member, table) ("+of(`janus_cluster_peer_table_synced{`+janusJob+`}`)+")", "table"), legend: "{{member}} · {{table}}"},
		}, []state{{1, "synced", "green"}, {0, "not synced", "orange"}, {-1, "no peer", "transparent"}}, false)))
	b.y += 7

	// Traffic and load: HAProxy's own exporter and node_exporter, on the
	// members' hosts.
	b.row("Traffic and load", false)
	hapDesc := " From HAProxy's own exporter (the HAProxy job; [HAProxy's own metrics](" + docs + "metrics.md#haproxys-own-metrics)), " +
		"on the same host as the member's Janus exporter."
	nodeDesc := " From node_exporter (the node job; [the prometheus-node-exporter extension](" + docs + "metrics.md#the-node-exporter)), " +
		"on the same host as the member's Janus exporter."
	traffic := []struct {
		title, desc, expr, unit string
	}{
		{"Sessions", "Sessions each member's frontends accepted, per second." + hapDesc,
			"sum by (member) (" + onHost(`sum by (instance) (rate(haproxy_frontend_sessions_total{`+hapJob+`}[$__rate_interval]))`) + ")", "ops"},
		{"HTTP requests", "HTTP requests each member's frontends received, per second." + hapDesc,
			"sum by (member) (" + onHost(`sum by (instance) (rate(haproxy_frontend_http_requests_total{`+hapJob+`}[$__rate_interval]))`) + ")", "reqps"},
		{"Connections", "Each member's HAProxy's connections now." + hapDesc,
			"sum by (member) (" + onHost(`sum by (instance) (haproxy_process_current_connections{`+hapJob+`})`) + ")", "none"},
		{"HAProxy busy", "The share of time each member's HAProxy threads weren't idle." + hapDesc,
			"max by (member) (" + onHost(`1 - avg by (instance) (haproxy_process_idle_time_percent{`+hapJob+`}) / 100`) + ")", "percentunit"},
		{"CPU busy", "The share of time each member's CPUs weren't idle." + nodeDesc,
			"max by (member) (" + onHost(`1 - avg by (instance) (rate(node_cpu_seconds_total{`+nodeJob+`, mode="idle"}[$__rate_interval]))`) + ")", "percentunit"},
		{"Memory used", "The share of each member's memory not available to new processes." + nodeDesc,
			"max by (member) (" + onHost(`1 - node_memory_MemAvailable_bytes{`+nodeJob+`} / node_memory_MemTotal_bytes{`+nodeJob+`}`) + ")", "percentunit"},
	}
	for i, x := range traffic {
		p := b.add(plain(timeseries(x.title, x.desc, b.pos(7, 8, 8*(i%3)), []q{{ref: "A", expr: x.expr, legend: "{{member}}"}}, x.unit)))
		defaults(p)["min"] = 0
		if x.unit == "percentunit" {
			defaults(p)["max"] = 1
		}
		if i%3 == 2 {
			b.y += 7
		}
	}

	// The cluster as the Controller sees it.
	b.row("Controller", false)
	versions := b.add(plain(timeseries("Versions",
		"The version of the cluster's configuration its members should run (desired), and the one each member "+
			"last took whole. An apply moves them; a member left on another one is in the Configuration tile.",
		b.pos(9, 12, 0), []q{
			{ref: "A", expr: "max(janus_cluster_desired_version{" + ctl + "})", legend: "desired"},
			{ref: "B", expr: "max by (member) (janus_cluster_member_applied_version{" + ctl + "})", legend: "{{member}}"},
		}, "none", byRef("A", prop("custom.lineStyle", obj{"fill": "dash", "dash": []any{10, 10}}), prop("custom.lineWidth", 3),
			prop("color", fixed("text"))))))
	defaults(versions)["decimals"] = 0
	noValue(versions, noController)
	health := b.add(plain(timeline("Members' health",
		"Each member as the Controller recorded it: whether its last apply or check failed, each managed domain "+
			"changed on the node since it was applied (drift), drained.",
		b.pos(9, 12, 12), []q{
			{ref: "A", expr: orNoneBy("max by (member) (janus_cluster_member_error{" + ctl + "})"), legend: "{{member}} · apply or check"},
			{ref: "B", expr: "max by (member, domain) (janus_cluster_member_drift{" + ctl + "})", legend: "{{member}} · {{domain}}"},
			{ref: "C", expr: orNoneBy("max by (member) (janus_cluster_member_drained{" + ctl + "})"), legend: "{{member}} · service"},
		}, []state{{0, "succeeded", "green"}, {1, "failed", "red"}, noControllerState}, false,
		byRef("B", prop("mappings", []any{mapping(state{0, "as applied", "green"}, state{1, "drifted", "orange"})})),
		byRef("C", prop("mappings", []any{mapping(state{0, "in service", "green"}, state{1, "drained", "blue"}, noControllerState)})))))
	noValue(health, noController)
	b.y += 9
	ops := b.add(plain(timeline("Operations",
		"An operation running on the cluster - an apply, a member added or removed, a scale, a recycle - and "+
			"whether the last one failed (until the next starts).",
		b.pos(5, 12, 0), []q{
			{ref: "A", expr: orNoneCluster("max(janus_cluster_operation_running{"+ctl+"})", ""), legend: "running"},
			{ref: "B", expr: orNoneCluster("max(janus_cluster_operation_failed{"+ctl+"})", ""), legend: "last one"},
		}, []state{{0, "none", "transparent"}, {1, "running", "blue"}, noControllerState}, false,
		byRef("B", prop("mappings", []any{mapping(state{0, "succeeded", "green"}, state{1, "failed", "red"}, noControllerState)})))))
	noValue(ops, noController)
	count := b.add(plain(timeseries("Member count",
		"The cluster's members, those that answered the Controller's last load reading (every 15 s), and the "+
			"count [auto-scaling]("+docs+"guide/clusters.md#scaling) judges - the one it simulates while observing.",
		b.pos(5, 12, 12), []q{
			{ref: "A", expr: "max(janus_cluster_members{" + ctl + "})", legend: "members"},
			{ref: "B", expr: "max(janus_cluster_load_members{" + ctl + "})", legend: "answered"},
			{ref: "C", expr: "max(janus_cluster_autoscale_judged_members{" + ctl + "})", legend: "judged"},
		}, "none")))
	defaults(count)["decimals"], defaults(count)["min"] = 0, 0
	noValue(count, noController)
	b.y += 5
	load := b.add(plain(timeseries("Load",
		"The members' load as the Controller reads it every 15 s - what horizontal auto-scaling judges: HAProxy's "+
			"idle time and the CPUs' busy and steal time, the means over the members, and the highest share of "+
			"maxconn in use. Steal above 10 %: the hosts are saturated.",
		b.pos(7, 24, 0), []q{
			{ref: "A", expr: "max(janus_cluster_load_haproxy_idle_ratio{" + ctl + "})", legend: "HAProxy idle"},
			{ref: "B", expr: "max(janus_cluster_load_cpu_busy_ratio{" + ctl + "})", legend: "CPU busy"},
			{ref: "C", expr: "max(janus_cluster_load_cpu_steal_ratio{" + ctl + "})", legend: "CPU steal"},
			{ref: "D", expr: "max(janus_cluster_load_connections_ratio{" + ctl + "})", legend: "connections / maxconn"},
		}, "percentunit")))
	defaults(load)["min"], defaults(load)["max"] = 0, 1
	noValue(load, noController)

	annotation := func(name, color, expr, text string) obj {
		return obj{"datasource": ds, "enable": true, "name": name, "iconColor": color, "expr": expr,
			"step": "1m", "titleFormat": name, "textFormat": text}
	}
	variable := func(name, label, query, current string) obj {
		v := obj{"name": name, "label": label, "type": "query", "datasource": ds, "definition": query,
			"query":   obj{"qryType": 1, "query": query, "refId": "PrometheusVariableQueryEditor-VariableQuery"},
			"refresh": 2, "sort": 1, "hide": 0, "includeAll": false, "multi": false, "options": []any{}, "regex": "",
			"current": obj{}}
		if current != "" {
			v["current"] = obj{"text": current, "value": current}
		}
		return v
	}
	return obj{
		"title": "Janus · Clusters",
		"uid":   "janus-clusters",
		"description": "A Janus cluster's members - VRRP, peers, stick tables, traffic - and the cluster as the " +
			"Controller sees it - " + docs + "metrics.md",
		"tags":          []any{"janus", "haproxy"},
		"editable":      true,
		"graphTooltip":  1,
		"time":          obj{"from": "now-24h", "to": "now"},
		"timepicker":    obj{},
		"timezone":      "browser",
		"refresh":       "1m",
		"schemaVersion": 41,
		"version":       1,
		"links": []any{
			obj{"title": "Metrics", "type": "link", "icon": "doc", "targetBlank": true,
				"url": docs + "metrics.md", "tooltip": "What each metric means"},
			obj{"title": "Clusters", "type": "link", "icon": "doc", "targetBlank": true,
				"url": docs + "guide/clusters.md", "tooltip": "Running a cluster"},
			dashboardsLink,
		},
		"annotations": obj{"list": []any{
			obj{"builtIn": 1, "datasource": obj{"type": "grafana", "uid": "-- Grafana --"}, "enable": true, "hide": true,
				"iconColor": "rgba(0, 211, 255, 1)", "name": "Annotations & Alerts", "type": "dashboard"},
			annotation("VRRP failover", "orange",
				"("+of(`increase(janus_vrrp_instance_became_master_total{`+janusJob+`}[2m])`)+") > 0",
				"{{member}} took {{exported_instance}}"),
			annotation("Configuration applied", "green",
				"("+of(`changes(janus_haproxy_config_last_apply_timestamp_seconds{`+janusJob+`}[2m])`)+") > 0", "{{member}}"),
			annotation("janusd started", "blue", "("+of(`changes(janus_daemon_start_time_seconds{`+janusJob+`}[2m])`)+") > 0", "{{member}}"),
		}},
		"templating": obj{"list": []any{
			obj{"name": "datasource", "label": "Data source", "type": "datasource", "query": "prometheus",
				"current": obj{}, "hide": 0, "refresh": 1, "regex": "", "includeAll": false, "multi": false, "options": []any{}},
			variable("job", "Janus job", "label_values(janus_cluster_info, job)", "janus"),
			variable("cluster", "Cluster", `label_values(janus_cluster_info{job="$job"}, cluster_name)`, ""),
			variable("controller", "Controller job", "label_values(janus_controller_build_info, job)", "janus-controller"),
			variable("haproxy", "HAProxy job", "label_values(haproxy_process_nbthread, job)", "haproxy"),
			variable("node", "node_exporter job", "label_values(node_uname_info, job)", "node"),
		}},
		"panels": b.panels,
	}
}

// Every selector keeps to what the variables chose: a Janus node metric
// (or up) to the Janus job and, through janus_cluster_info, to the
// cluster's members; a Controller metric to the Controller's job and the
// cluster; HAProxy's and node_exporter's to their jobs and, by host, to
// the members.
func TestClustersDashboardSelects(t *testing.T) {
	selector := regexp.MustCompile(`\b((?:janus|haproxy|node)_[A-Za-z_]+|up)\b(\{[^}]*\})?`)
	ctlMetrics := controllerExported(t)
	for _, e := range exprs(t, clustersDashboard()) {
		joined := strings.Contains(e, info)
		for _, m := range selector.FindAllStringSubmatch(e, -1) {
			name, sel := m[1], m[2]
			var want []string
			switch {
			case name == "janus_cluster_info":
				want = []string{janusJob, `cluster_name="$cluster"`}
			case slices.Contains(ctlMetrics, name):
				want = []string{ctl}
			case strings.HasPrefix(name, "haproxy_"):
				want = []string{hapJob}
			case strings.HasPrefix(name, "node_"):
				want = []string{nodeJob}
			default:
				want = []string{janusJob}
			}
			for _, w := range want {
				if !strings.Contains(sel, w) {
					t.Errorf("%s%s doesn't keep to %s: %s", name, sel, w, e)
				}
			}
			if !slices.Contains(ctlMetrics, name) && !joined {
				t.Errorf("%s isn't kept to the cluster's members (janus_cluster_info): %s", name, e)
			}
		}
	}
}
