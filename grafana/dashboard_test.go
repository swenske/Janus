package grafana

import (
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// janus-nodes.json, the Grafana dashboard of the Janus exporter
// (docs/metrics.md), is built here, so that a change is a reviewable
// diff of this file and the tests below hold it to what janusd exports:
//
//	go test ./grafana -run TestDashboard -update
//
// It is Grafana's classic JSON model, checked on Grafana 11.6, 12.0 and
// 13.0. Its queries are PromQL that promtool parses (hack/examples-check.sh).

var update = flag.Bool("update", false, "rewrite janus-nodes.json")

const dashboardFile = "janus-nodes.json"

type obj = map[string]any

const docs = "https://janus.sw-servers.net/docs/"

var ds = obj{"type": "prometheus", "uid": "${datasource}"}

// sel selects the nodes the dashboard's variables chose.
const sel = `job="$job", instance=~"$instance"`

var up = "up{" + sel + "}"

// Node names in legends and table cells: "lb1.example.net:10056" ->
// "lb1", "192.0.2.10:10056" -> "192.0.2.10". A target relabeled to a
// plain name is left alone.
const (
	shortHost = `^([A-Za-z][A-Za-z0-9-]*)[.:][^ ]*`
	shortAddr = `^([0-9.]+|\[[0-9a-fA-F:]+\]):[0-9]+`
)

var (
	shortNames = []any{
		obj{"id": "renameByRegex", "options": obj{"regex": shortHost + "( · .*)?$", "renamePattern": "$1$2"}},
		obj{"id": "renameByRegex", "options": obj{"regex": shortAddr + "( · .*)?$", "renamePattern": "$1$2"}},
	}
	shortNameMappings = []any{
		obj{"type": "regex", "options": obj{"pattern": shortHost + "$", "result": obj{"text": "$1", "index": 0}}},
		obj{"type": "regex", "options": obj{"pattern": shortAddr + "$", "result": obj{"text": "$1", "index": 1}}},
	}
	colorText = obj{"type": "color-text"}
)

const (
	day  = 86400
	warn = 14 * day // the JanusCertificateExpiresSoon alert
	crit = 7 * day
)

// The extensions' columns, in this order (TestDashboardExtensions keeps
// it equal to extensions/).
var extensionColumns = []string{"bird", "consul", "keepalived", "letsencrypt", "nftables",
	"prometheus-node-exporter", "qemu-guest-agent"}

// The metrics janusd exports that no panel shows, and why.
var notShown = map[string]string{
	"janus_haproxy_starts_total":                   "reloads and crashes are shown apart; the rest are boots",
	"janus_network_trial_revert_timestamp_seconds": "the trial itself is shown (Awaiting confirmation)",
	"janus_firewall_configured":                    "the firewall's sets are shown",
	"janus_time_stratum":                           "the offset and the last synchronization say more",
	"janus_vrrp_instance_became_master_total":      "the VRRP timeline shows every change of state",
	"janus_bgp_protocol_up":                        "the BGP sessions and the held-down protocols are shown",
}

// q is one query. Range queries draw over time, instant ones are the
// value now (stat tiles), table ones a row per series.
type q struct {
	ref, expr, legend string
	instant, table    bool
}

func (t q) target() obj {
	o := obj{
		"datasource":   ds,
		"editorMode":   "code",
		"expr":         t.expr,
		"legendFormat": cmp.Or(t.legend, "__auto"),
		"refId":        t.ref,
		"range":        !t.instant && !t.table,
		"instant":      t.instant || t.table,
	}
	if t.table {
		o["format"] = "table"
	}
	return o
}

func targets(qs []q) []any {
	out := make([]any, len(qs))
	for i, t := range qs {
		out[i] = t.target()
	}
	return out
}

// thresholds: the base color, then (value, color) pairs.
func thresholds(base string, steps ...any) obj {
	s := []any{obj{"color": base, "value": nil}}
	for i := 0; i < len(steps); i += 2 {
		s = append(s, obj{"value": steps[i], "color": steps[i+1]})
	}
	return obj{"mode": "absolute", "steps": s}
}

// state is a value a mapping names and colors.
type state struct {
	value       any
	text, color string
}

func mapping(states ...state) obj {
	opts := obj{}
	for i, s := range states {
		opts[fmt.Sprint(s.value)] = obj{"text": s.text, "color": s.color, "index": i}
	}
	return obj{"type": "value", "options": opts}
}

func prop(id string, v any) obj { return obj{"id": id, "value": v} }

func override(matcher, option string, props ...obj) obj {
	ps := make([]any, len(props))
	for i, p := range props {
		ps[i] = p
	}
	return obj{"matcher": obj{"id": matcher, "options": option}, "properties": ps}
}

func byName(name string, props ...obj) obj { return override("byName", name, props...) }
func byRef(ref string, props ...obj) obj   { return override("byFrameRefID", ref, props...) }

// builder lays the panels out on Grafana's 24-column grid.
type builder struct {
	panels []any
	rows   []obj // the collapsed rows, which hold their panels
	id, y  int
}

func (b *builder) pos(h, w, x int) obj { return obj{"h": h, "w": w, "x": x, "y": b.y} }

func (b *builder) add(p obj) obj {
	b.id++
	p["id"] = b.id
	p["datasource"] = ds
	if n := len(b.rows); n > 0 {
		r := b.rows[n-1]
		r["panels"] = append(r["panels"].([]any), p)
	} else {
		b.panels = append(b.panels, p)
	}
	return p
}

func (b *builder) row(title string, collapsed bool) {
	b.id++
	r := obj{"type": "row", "id": b.id, "title": title, "collapsed": collapsed,
		"gridPos": obj{"h": 1, "w": 24, "x": 0, "y": b.y}, "panels": []any{}}
	b.panels = append(b.panels, r)
	if collapsed {
		b.rows = append(b.rows, r)
	}
	b.y++
}

func panel(kind, title, desc string, pos obj, qs []q) obj {
	return obj{"type": kind, "title": title, "description": desc, "gridPos": pos, "targets": targets(qs)}
}

func defaults(p obj) obj { return p["fieldConfig"].(obj)["defaults"].(obj) }

func stat(title, desc string, pos obj, qs []q, thr obj, overrides ...any) obj {
	p := panel("stat", title, desc, pos, qs)
	p["fieldConfig"] = obj{
		"defaults":  obj{"unit": "none", "color": obj{"mode": "thresholds"}, "thresholds": thr, "mappings": []any{}},
		"overrides": append([]any{}, overrides...),
	}
	text := "value"
	if len(qs) > 1 {
		text = "value_and_name"
	}
	p["options"] = obj{
		"colorMode": "value", "graphMode": "none", "justifyMode": "center", "orientation": "auto",
		"textMode": text, "wideLayout": true, "showPercentChange": false,
		"reduceOptions": obj{"calcs": []any{"lastNotNull"}, "fields": "", "values": false},
	}
	return p
}

func table(title, desc string, pos obj, qs []q, transformations []any, sortBy string, overrides ...any) obj {
	p := panel("table", title, desc, pos, qs)
	p["transformations"] = transformations
	p["fieldConfig"] = obj{
		"defaults": obj{
			"custom":     obj{"align": "auto", "cellOptions": obj{"type": "auto"}, "inspect": false, "filterable": false},
			"mappings":   []any{},
			"thresholds": thresholds("text"),
			"color":      obj{"mode": "thresholds"},
		},
		"overrides": append([]any{}, overrides...),
	}
	p["options"] = obj{"showHeader": true, "cellHeight": "sm", "footer": obj{"show": false},
		"sortBy": []any{obj{"displayName": sortBy, "desc": false}}}
	return p
}

// organize hides, renames and orders a table's columns.
func organize(exclude []string, rename map[string]string, order ...string) obj {
	ex, rn, idx := obj{}, obj{}, obj{}
	for _, f := range exclude {
		ex[f] = true
	}
	for k, v := range rename {
		rn[k] = v
	}
	for i, f := range order {
		idx[f] = i
	}
	return obj{"id": "organize", "options": obj{"excludeByName": ex, "renameByName": rn, "indexByName": idx}}
}

var merge = obj{"id": "merge", "options": obj{}}

func timeline(title, desc string, pos obj, qs []q, states []state, legend bool, overrides ...any) obj {
	p := panel("state-timeline", title, desc, pos, qs)
	p["transformations"] = shortNames
	p["maxDataPoints"] = 500
	p["fieldConfig"] = obj{
		"defaults": obj{
			"custom":     obj{"fillOpacity": 75, "lineWidth": 0, "spanNulls": false, "insertNulls": false},
			"color":      obj{"mode": "fixed", "fixedColor": "text"},
			"thresholds": thresholds("text"),
			"mappings":   []any{mapping(states...)},
		},
		"overrides": append([]any{}, overrides...),
	}
	p["options"] = obj{
		"mergeValues": true, "showValue": "auto", "alignValue": "left", "rowHeight": 0.8,
		"legend":  obj{"showLegend": legend, "displayMode": "list", "placement": "bottom"},
		"tooltip": obj{"mode": "single", "sort": "none"},
	}
	return p
}

func timeseries(title, desc string, pos obj, qs []q, unit string, overrides ...any) obj {
	p := panel("timeseries", title, desc, pos, qs)
	p["transformations"] = shortNames
	p["fieldConfig"] = obj{
		"defaults": obj{
			"unit": unit,
			"custom": obj{
				"drawStyle": "line", "lineWidth": 2, "fillOpacity": 0, "lineInterpolation": "stepAfter",
				"barAlignment": -1, "showPoints": "never", "pointSize": 4, "spanNulls": false,
				"axisBorderShow": false, "axisPlacement": "auto", "gradientMode": "none",
				"stacking":        obj{"mode": "none", "group": "A"},
				"thresholdsStyle": obj{"mode": "off"},
			},
			"color":      obj{"mode": "palette-classic"},
			"thresholds": thresholds("green"),
		},
		"overrides": append([]any{}, overrides...),
	}
	p["options"] = obj{
		"legend":  obj{"showLegend": true, "displayMode": "list", "placement": "bottom", "calcs": []any{}},
		"tooltip": obj{"mode": "multi", "sort": "desc"},
	}
	return p
}

// events is a timeseries of counter increments as stacked bars, one per
// minute at least, on a 0-1 axis while nothing happened.
func events(title, desc string, pos obj, qs []q, overrides ...any) obj {
	qs = append(qs, q{ref: "Z", expr: "0 * count(" + up + ")", legend: "baseline"})
	overrides = append([]any{hideBaseline}, overrides...)
	p := timeseries(title, desc, pos, qs, "none", overrides...)
	p["interval"] = "1m"
	d := defaults(p)
	d["min"], d["decimals"] = 0, 0
	c := d["custom"].(obj)
	c["drawStyle"], c["lineWidth"], c["fillOpacity"], c["lineInterpolation"] = "bars", 1, 80, "linear"
	c["stacking"] = obj{"mode": "normal", "group": "A"}
	c["axisSoftMax"] = 1
	return p
}

// The zero series events() adds so the panel draws its axes when nothing
// happened, kept out of the legend and the tooltip.
var hideBaseline = byRef("Z", prop("custom.hideFrom", obj{"legend": true, "tooltip": true, "viz": false}))

// increments is the exact increase of a counter per $__interval: no
// extrapolation (increase() draws 1.33 reloads), and a reset counts
// nothing. Only the intervals where it grew are drawn.
func increments(metric, by string) string {
	s := metric + "{" + sel + "}"
	return fmt.Sprintf("(sum by (%s) (%s) - sum by (%s) (%s offset $__interval)) > 0", by, s, by, s)
}

// encode numbers each state for a state timeline: 1 for the first of
// states, 2 for the next... of a metric that is 1 for its current state.
func encode(metric, by string, states []string) string {
	terms := make([]string, len(states))
	for i, s := range states {
		terms[i] = fmt.Sprintf(`sum by (%s) (%s{%s, state="%s"})`, by, metric, sel, s)
		if i > 0 {
			terms[i] = fmt.Sprintf("%d * %s", i+1, terms[i])
		}
	}
	return strings.Join(terms, " + ")
}

func dashboard() obj {
	b := &builder{}
	green1Red := thresholds("green", 1, "red")

	// Overview.
	b.add(stat("Nodes", "Janus exporters answering Prometheus, and those that don't.", b.pos(4, 4, 0), []q{
		{ref: "A", expr: "count(" + up + " == 1) or vector(0)", legend: "answering", instant: true},
		{ref: "B", expr: "count(" + up + " == 0) or vector(0)", legend: "silent", instant: true},
	}, thresholds("text"), byRef("B", prop("thresholds", green1Red))))
	b.add(stat("HAProxy", "Nodes whose HAProxy answers on its stats socket (janus_haproxy_up), and those whose doesn't.",
		b.pos(4, 4, 4), []q{
			{ref: "A", expr: "count(janus_haproxy_up{" + sel + "} == 1) or vector(0)", legend: "serving", instant: true},
			{ref: "B", expr: "count(janus_haproxy_up{" + sel + "} == 0) or vector(0)", legend: "down", instant: true},
		}, thresholds("text"), byRef("B", prop("thresholds", green1Red))))
	expiry := b.add(stat("Next certificate expiry",
		"The soonest expiry among every certificate the selected nodes report: their API's CA and server "+
			"certificates, and every certificate HAProxy has loaded. Orange under 14 days (the "+
			"JanusCertificateExpiresSoon alert), red under 7.",
		b.pos(4, 4, 8), []q{
			{ref: "A", expr: "min(janus_certificate_expiry_timestamp_seconds{" + sel + "}) - time()", instant: true},
		}, thresholds("red", crit, "orange", warn, "green")))
	defaults(expiry)["unit"], defaults(expiry)["decimals"] = "dtdurations", 2
	b.add(stat("Awaiting confirmation",
		"Nodes with an upgrade waiting for its health confirmation, a network configuration or a firewall "+
			"ruleset on trial: each reverts unless confirmed.",
		b.pos(4, 4, 12), []q{
			{ref: "A", expr: "sum(janus_upgrade_pending_confirmation{" + sel + "}) or vector(0)", legend: "upgrade", instant: true},
			{ref: "B", expr: "sum(janus_network_trial_pending{" + sel + "}) or vector(0)", legend: "network", instant: true},
			{ref: "C", expr: "sum(janus_firewall_trial_pending{" + sel + "}) or vector(0)", legend: "firewall", instant: true},
		}, thresholds("green", 1, "orange")))
	b.add(stat("SELinux",
		"Nodes not enforcing SELinux, and the denials their kernels logged over the dashboard's time range - "+
			"there should be none.",
		b.pos(4, 4, 16), []q{
			{ref: "A", expr: "count(janus_selinux_enforcing{" + sel + "} == 0) or vector(0)", legend: "permissive", instant: true},
			{ref: "B", expr: "round(sum(increase(janus_selinux_denials_total{" + sel + "}[$__range]))) or vector(0)", legend: "denials", instant: true},
		}, green1Red))
	b.add(stat("Kernel and STATE",
		"Processes the kernel killed for lack of memory over the time range, and errors the kernel recorded "+
			"on STATE (PKI, configuration).",
		b.pos(4, 4, 20), []q{
			{ref: "A", expr: "round(sum(increase(janus_kernel_oom_kills_total{" + sel + "}[$__range]))) or vector(0)", legend: "OOM kills", instant: true},
			{ref: "B", expr: "sum(janus_state_filesystem_errors{" + sel + "}) or vector(0)", legend: "STATE errors", instant: true},
		}, green1Red))
	b.y += 4

	// The fleet: a query per column, merged on the node.
	fleet := []struct{ ref, expr, column string }{
		{"A", "max by (instance) (" + up + ")", "Exporter"},
		{"B", "max by (instance, version) (janus_build_info{" + sel + "})", ""},
		{"C", `max by (instance, schematic) (label_replace(janus_build_info{` + sel + `}, "schematic", "$1", "schematic", "(.{8}).*"))`, ""},
		{"D", `max by (instance, haproxy) (label_replace(janus_component_info{` + sel + `, component="haproxy"}, "haproxy", "$1", "version", "(.*)"))`, ""},
		{"E", `max by (instance, kernel) (label_replace(label_join(janus_component_info{` + sel + `, component="kernel"}, "kernel", " (", "version", "variant"), "kernel", "$1)", "kernel", "(.*)"))`, ""},
		{"F", "max by (instance, slot) (janus_boot_info{" + sel + "})", ""},
		{"G", "max by (instance) (janus_haproxy_up{" + sel + "})", "HAProxy state"},
		{"H", "max by (instance) (janus_daemon_start_time_seconds{" + sel + "}) * 1000", "janusd started"},
		{"I", `max by (instance) (janus_boot_stage_seconds{` + sel + `, stage="api"})`, "Boot"},
		{"J", "max by (instance) (janus_haproxy_config_last_apply_timestamp_seconds{" + sel + "}) * 1000", "Config applied"},
		{"K", "max by (instance) (janus_time_synchronized{" + sel + "})", "NTP"},
		{"L", "max by (instance) (janus_time_offset_seconds{" + sel + "})", "Clock offset"},
		{"M", "max by (instance) (janus_selinux_enforcing{" + sel + "})", "SELinux"},
	}
	var fleetQs []q
	exclude := []string{"Time"}
	rename := map[string]string{"instance": "Node", "version": "Janus", "schematic": "Schematic",
		"haproxy": "HAProxy", "kernel": "Kernel", "slot": "Slot"}
	order := []string{"instance", "Value #A", "version", "schematic", "haproxy", "kernel", "slot"}
	for _, c := range fleet {
		fleetQs = append(fleetQs, q{ref: c.ref, expr: c.expr, table: true})
		v := "Value #" + c.ref
		if c.column == "" {
			exclude = append(exclude, v)
			continue
		}
		rename[v] = c.column
		if c.ref != "A" {
			order = append(order, v)
		}
	}
	b.add(table("Fleet",
		"One line per node: what it runs and how it is. Janus = the release, Schematic = the "+
			"[image schematic]("+docs+"image-factory.md), Slot = the A/B slot it booted, Boot = seconds from "+
			"the kernel's start to the API listening (firmware not counted), Clock offset = measured at the last "+
			"NTP synchronization.",
		b.pos(6, 24, 0), fleetQs, []any{merge, organize(exclude, rename, order...)}, "Node",
		byName("Node", prop("mappings", shortNameMappings), prop("custom.width", 160)),
		byName("Exporter", prop("mappings", []any{mapping(state{1, "up", "green"}, state{0, "down", "red"})}),
			prop("custom.cellOptions", colorText)),
		byName("HAProxy", prop("custom.width", 90)),
		byName("Slot", prop("custom.width", 60)),
		byName("HAProxy state", prop("mappings", []any{mapping(state{1, "serving", "green"}, state{0, "down", "red"})}),
			prop("custom.cellOptions", colorText)),
		byName("janusd started", prop("unit", "dateTimeFromNow")),
		byName("Boot", prop("unit", "s"), prop("decimals", 2), prop("custom.width", 80)),
		byName("Config applied", prop("unit", "dateTimeFromNow"), prop("noValue", "never")),
		byName("NTP", prop("mappings", []any{mapping(state{1, "synced", "green"}, state{0, "not synced", "red"})}),
			prop("custom.cellOptions", colorText), prop("custom.width", 90)),
		byName("Clock offset", prop("unit", "s"), prop("decimals", 1), prop("custom.width", 100)),
		byName("SELinux", prop("mappings", []any{mapping(state{1, "enforcing", "green"}, state{0, "permissive", "red"})}),
			prop("custom.cellOptions", colorText), prop("custom.width", 100)),
	))
	b.y += 6

	// HAProxy.
	b.row("HAProxy", false)
	b.add(timeline("HAProxy",
		`Whether each node's HAProxy answers on its stats socket (janus_haproxy_up). "no answer": the node's `+
			"exporter itself didn't answer.",
		b.pos(6, 12, 0), []q{
			{ref: "A", expr: "max by (instance) (janus_haproxy_up{" + sel + "}) or (max by (instance) (" + up + ") - 1)", legend: "{{instance}}"},
		}, []state{{1, "serving", "green"}, {0, "down", "red"}, {-1, "no answer", "purple"}}, true))
	b.add(events("Reloads, crashes and configurations",
		"Seamless reloads, HAProxy processes that exited without being stopped or replaced (crashes), and "+
			"configurations applied through the API, accepted or rejected - per interval. Nothing drawn: nothing "+
			"happened.",
		b.pos(6, 12, 12), []q{
			{ref: "A", expr: increments("janus_haproxy_reloads_total", "instance"), legend: "{{instance}} · reloads"},
			{ref: "B", expr: increments("janus_haproxy_unexpected_exits_total", "instance"), legend: "{{instance}} · crashes"},
			{ref: "C", expr: increments("janus_haproxy_config_applies_total", "instance, result"), legend: "{{instance}} · configuration {{result}}"},
		},
		override("byRegexp", ".*crashes", prop("color", obj{"mode": "fixed", "fixedColor": "red"})),
		override("byRegexp", ".*configuration rejected", prop("color", obj{"mode": "fixed", "fixedColor": "orange"})),
	))
	b.y += 6

	// Certificates.
	b.row("Certificates", false)
	b.add(table("Certificates",
		"Every certificate the nodes report: their API's CA and server certificates (source api), and every "+
			"certificate HAProxy has loaded, by file or store name (source haproxy). Orange under 14 days, red "+
			"under 7.",
		b.pos(9, 24, 0), []q{
			{ref: "A", expr: "max by (instance, source, certificate, cn) (janus_certificate_expiry_timestamp_seconds{" + sel + "}) - time()", table: true},
			{ref: "B", expr: "max by (instance, source, certificate, cn) (janus_certificate_expiry_timestamp_seconds{" + sel + "}) * 1000", table: true},
		},
		[]any{merge, organize([]string{"Time"},
			map[string]string{"instance": "Node", "source": "Source", "certificate": "Certificate", "cn": "Common name",
				"Value #A": "Expires in", "Value #B": "Expires"},
			"instance", "source", "certificate", "cn", "Value #B", "Value #A")},
		"Expires in",
		byName("Node", prop("mappings", shortNameMappings), prop("custom.width", 160)),
		byName("Source", prop("custom.width", 80)),
		byName("Expires", prop("unit", "dateTimeAsIso"), prop("custom.width", 180)),
		byName("Expires in", prop("unit", "dtdurations"), prop("decimals", 2), prop("custom.width", 220),
			prop("thresholds", thresholds("red", crit, "orange", warn, "green")), prop("custom.cellOptions", colorText)),
	))
	b.y += 9
	b.add(table("Let's Encrypt",
		"Certificates the [letsencrypt extension]("+docs+"letsencrypt.md) obtains: their state, when each was "+
			"last obtained and becomes due for renewal, and failed attempts in a row (orange from 1, red from 3).",
		b.pos(7, 18, 0), []q{
			{ref: "A", expr: "max by (instance, name, state) (janus_acme_certificate_state{" + sel + "} == 1)", table: true},
			{ref: "B", expr: "max by (instance, name) (janus_acme_certificate_last_success_timestamp_seconds{" + sel + "} > 0) * 1000", table: true},
			{ref: "C", expr: "max by (instance, name) (janus_acme_certificate_renew_timestamp_seconds{" + sel + "} > 0) * 1000", table: true},
			{ref: "D", expr: "max by (instance, name) (janus_acme_certificate_failures{" + sel + "})", table: true},
		},
		[]any{merge, organize([]string{"Time", "Value #A"},
			map[string]string{"instance": "Node", "name": "Certificate", "state": "State",
				"Value #B": "Obtained", "Value #C": "Renewal", "Value #D": "Failures"},
			"instance", "name", "state", "Value #B", "Value #C", "Value #D")},
		"Node",
		byName("Node", prop("mappings", shortNameMappings), prop("custom.width", 160)),
		byName("State", prop("mappings", []any{mapping(state{"valid", "valid", "green"}, state{"due", "due for renewal", "orange"},
			state{"pending", "not obtained yet", "orange"}, state{"expired", "expired", "red"})}),
			prop("custom.cellOptions", colorText)),
		byName("Obtained", prop("unit", "dateTimeFromNow"), prop("noValue", "never")),
		byName("Renewal", prop("unit", "dateTimeFromNow"), prop("noValue", "-")),
		byName("Failures", prop("thresholds", thresholds("green", 1, "orange", 3, "red")),
			prop("custom.cellOptions", colorText), prop("custom.width", 90)),
	))
	account := b.add(stat("ACME account",
		"Whether the ACME CA knows the account of each node that has Let's Encrypt certificates configured.",
		b.pos(7, 6, 18), []q{
			{ref: "A", expr: "max by (instance) (janus_acme_account_registered{" + sel + "}) and on (instance) " +
				"count by (instance) (janus_acme_certificate_state{" + sel + "})", legend: "{{instance}}", instant: true},
		}, thresholds("text")))
	defaults(account)["mappings"] = []any{mapping(state{1, "registered", "green"}, state{0, "not registered", "red"})}
	account["transformations"] = shortNames
	account["options"].(obj)["textMode"] = "value_and_name"
	b.y += 7

	// Extension services.
	b.row("Extension services", false)
	services := []state{{1, "running", "green"}, {2, "waiting for its configuration", "blue"},
		{3, "restarting", "red"}, {4, "stopped", "text"}}
	b.add(timeline("Services",
		"Each extension service janusd supervises ([image factory]("+docs+"image-factory.md)): running, "+
			"waiting for the files it needs (its configuration - normal until it is configured), restarting after "+
			"it exited (backoff), or stopped on purpose.",
		b.pos(8, 24, 0), []q{
			{ref: "A", expr: encode("janus_service_state", "instance, service", []string{"running", "waiting", "restarting", "stopped"}),
				legend: "{{instance}} · {{service}}"},
		}, services, true))
	b.y += 8
	b.add(table("Extensions",
		"The extensions built into each node's image ([image factory]("+docs+"image-factory.md)), and their "+
			"versions.",
		b.pos(6, 17, 0), []q{
			{ref: "A", expr: "max by (instance, extension, version) (janus_extension_info{" + sel + "})", table: true},
		},
		[]any{
			obj{"id": "groupingToMatrix", "options": obj{"columnField": "extension", "rowField": "instance",
				"valueField": "version", "emptyValue": "empty"}},
			organize(nil, map[string]string{`instance\extension`: "Node"}, append([]string{`instance\extension`}, extensionColumns...)...),
		},
		"Node",
		byName("Node", prop("mappings", shortNameMappings), prop("custom.width", 130)),
	))
	b.add(events("Service restarts",
		"Times each extension service exited and was restarted, per interval. Nothing drawn: no restart.",
		b.pos(6, 7, 17), []q{
			{ref: "A", expr: increments("janus_service_restarts_total", "instance, service"), legend: "{{instance}} · {{service}}"},
		}))
	b.y += 6

	// The node.
	b.row("Node", false)
	b.add(timeline("Awaiting confirmation",
		"An upgrade waiting for its health confirmation, a network configuration or a firewall ruleset on "+
			"trial: each reverts unless confirmed.",
		b.pos(7, 12, 0), []q{
			{ref: "A", expr: "max by (instance) (janus_upgrade_pending_confirmation{" + sel + "})", legend: "{{instance}} · upgrade"},
			{ref: "B", expr: "max by (instance) (janus_network_trial_pending{" + sel + "})", legend: "{{instance}} · network"},
			{ref: "C", expr: "max by (instance) (janus_firewall_trial_pending{" + sel + "})", legend: "{{instance}} · firewall"},
		}, []state{{0, "nothing pending", "transparent"}, {1, "awaiting confirmation", "orange"}}, false))
	boot := b.add(table("Last boot",
		"Seconds after the kernel started at which each stage of the last boot completed: the kernel handed "+
			"over to init, init started janusd, HAProxy served, the API listened. The firmware's own time comes "+
			"before and isn't counted.",
		b.pos(7, 12, 12), []q{
			{ref: "A", expr: "max by (instance, stage) (janus_boot_stage_seconds{" + sel + "})", table: true},
		},
		[]any{
			obj{"id": "groupingToMatrix", "options": obj{"columnField": "stage", "rowField": "instance", "valueField": "Value"}},
			organize(nil, map[string]string{`instance\stage`: "Node", "kernel": "kernel → init", "init": "janusd started",
				"haproxy": "HAProxy served", "api": "API listening"},
				`instance\stage`, "kernel", "init", "haproxy", "api"),
		},
		"Node",
		byName("Node", prop("mappings", shortNameMappings)),
	))
	defaults(boot)["unit"], defaults(boot)["decimals"] = "s", 2
	b.y += 7
	b.add(timeseries("Clock offset", "The offset janusd's NTP client measured at its last synchronization.",
		b.pos(7, 12, 0), []q{
			{ref: "A", expr: "max by (instance) (janus_time_offset_seconds{" + sel + "})", legend: "{{instance}}"},
		}, "s"))
	sync := b.add(timeseries("Time since the last NTP synchronization",
		"Over an hour (the dashed line) the JanusClockNotSynchronized alert fires.",
		b.pos(7, 12, 12), []q{
			{ref: "A", expr: "time() - max by (instance) (janus_time_last_sync_timestamp_seconds{" + sel + "})", legend: "{{instance}}"},
		}, "s"))
	defaults(sync)["min"] = 0
	defaults(sync)["thresholds"] = thresholds("transparent", 3600, "red")
	defaults(sync)["custom"].(obj)["thresholdsStyle"] = obj{"mode": "dashed"}
	b.y += 7
	calls := b.add(timeseries("API calls", "Calls to each node's gRPC API per second, refused ones included.",
		b.pos(7, 12, 0), []q{
			{ref: "A", expr: "sum by (instance) (rate(janus_api_requests_total{" + sel + "}[$__rate_interval]))", legend: "{{instance}}"},
		}, "reqps"))
	defaults(calls)["min"] = 0
	errs := b.add(timeseries("API errors",
		"Calls that didn't end OK, by gRPC code. PermissionDenied and Unauthenticated: callers refused "+
			"(the JanusAPIRefusals alert); NotFound, FailedPrecondition... are ordinary answers. Nothing drawn: "+
			"no error.",
		b.pos(7, 12, 12), []q{
			{ref: "A", expr: `sum by (instance, code) (rate(janus_api_requests_total{` + sel + `, code!="OK"}[$__rate_interval])) > 0`,
				legend: "{{instance}} · {{code}}"},
			{ref: "Z", expr: "0 * count(" + up + ")", legend: "baseline"},
		}, "reqps", hideBaseline))
	defaults(errs)["min"] = 0
	defaults(errs)["custom"].(obj)["axisSoftMax"] = 0.01
	b.y += 7

	// VRRP, BGP and the firewall: most nodes have none, so the row
	// starts collapsed.
	b.row("VRRP, BGP and firewall", true)
	vrrp := []string{"MASTER", "BACKUP", "FAULT", "INIT", "STOP"}
	// The exporter's VRRP instance label meets the target's own: Prometheus
	// keeps it as exported_instance.
	b.add(timeline("VRRP instances",
		"Each VRRP instance's state ([VRRP]("+docs+"vrrp.md), the keepalived extension). The instance's name "+
			"is the exported_instance label: Prometheus renames the exporter's instance label.",
		b.pos(6, 12, 0), []q{
			{ref: "A", expr: encode("janus_vrrp_instance_state", "instance, exported_instance", vrrp),
				legend: "{{instance}} · {{exported_instance}}"},
		}, []state{{1, "MASTER", "green"}, {2, "BACKUP", "blue"}, {3, "FAULT", "red"}, {4, "INIT", "orange"}, {5, "STOP", "text"}}, true))
	prio := b.add(timeseries("VRRP priority",
		"Each VRRP instance's priority after tracking (a failed track_file - HAProxy not serving - lowers it).",
		b.pos(6, 12, 12), []q{
			{ref: "A", expr: "max by (instance, exported_instance) (janus_vrrp_instance_effective_priority{" + sel + "})",
				legend: "{{instance}} · {{exported_instance}}"},
		}, "none"))
	defaults(prio)["decimals"] = 0
	b.y += 6
	b.add(timeline("BGP sessions",
		"Each BGP session ([BGP]("+docs+"bgp.md), the bird extension), and each haproxy_* protocol janusd "+
			"holds down while HAProxy doesn't answer.",
		b.pos(6, 12, 0), []q{
			{ref: "A", expr: "max by (instance, protocol, neighbor) (janus_bgp_session_established{" + sel + "})",
				legend: "{{instance}} · {{protocol}} ({{neighbor}})"},
			{ref: "B", expr: "max by (instance, protocol) (janus_bgp_protocol_held_down{" + sel + "})",
				legend: "{{instance}} · {{protocol}}"},
		}, []state{{1, "established", "green"}, {0, "not established", "red"}}, true,
		byRef("B", prop("mappings", []any{mapping(state{0, "announced", "green"}, state{1, "held down", "orange"})}))))
	routes := b.add(timeseries("BGP routes", "Routes each BIRD protocol imported and exported.",
		b.pos(6, 12, 12), []q{
			{ref: "A", expr: "sum by (instance, protocol, direction) (janus_bgp_routes{" + sel + "})",
				legend: "{{instance}} · {{protocol}} {{direction}}"},
		}, "none"))
	defaults(routes)["decimals"], defaults(routes)["min"] = 0, 0
	b.y += 6
	sets := b.add(timeseries("Firewall sets",
		"Elements in each named set of the live nftables ruleset ([firewall]("+docs+"firewall.md), the "+
			"nftables extension).",
		b.pos(6, 24, 0), []q{
			{ref: "A", expr: "max by (instance, family, table, set) (janus_firewall_set_elements{" + sel + "})",
				legend: "{{instance}} · {{family}} {{table}} {{set}}"},
		}, "none"))
	defaults(sets)["decimals"], defaults(sets)["min"] = 0, 0

	annotation := func(name, color, expr string) obj {
		return obj{"datasource": ds, "enable": true, "name": name, "iconColor": color, "expr": expr,
			"step": "1m", "titleFormat": name, "textFormat": "{{instance}}"}
	}
	variable := func(name, label, query string, all bool) obj {
		v := obj{"name": name, "label": label, "type": "query", "datasource": ds, "definition": query,
			"query":   obj{"qryType": 1, "query": query, "refId": "PrometheusVariableQueryEditor-VariableQuery"},
			"refresh": 2, "sort": 1, "hide": 0, "includeAll": all, "multi": all, "options": []any{}, "regex": ""}
		if all {
			v["current"] = obj{"text": []any{"All"}, "value": []any{"$__all"}}
		} else {
			v["current"] = obj{"text": "janus", "value": "janus"}
		}
		return v
	}
	return obj{
		"title":       "Janus nodes",
		"uid":         "janus-nodes",
		"description": "Janus nodes through the Janus exporter every node serves (:10056) - " + docs + "metrics.md",
		"tags":        []any{"janus", "haproxy"},
		"editable":    true,
		// A crosshair shared by every panel.
		"graphTooltip":  1,
		"time":          obj{"from": "now-24h", "to": "now"},
		"timepicker":    obj{},
		"timezone":      "browser",
		"refresh":       "1m",
		"schemaVersion": 41,
		"version":       1,
		"links": []any{obj{"title": "Janus exporter metrics", "type": "link", "icon": "doc", "targetBlank": true,
			"url": docs + "metrics.md", "tooltip": "What each metric means"}},
		"annotations": obj{"list": []any{
			obj{"builtIn": 1, "datasource": obj{"type": "grafana", "uid": "-- Grafana --"}, "enable": true, "hide": true,
				"iconColor": "rgba(0, 211, 255, 1)", "name": "Annotations & Alerts", "type": "dashboard"},
			annotation("janusd started", "blue", "changes(janus_daemon_start_time_seconds{"+sel+"}[2m]) > 0"),
			annotation("HAProxy configuration applied", "green", "changes(janus_haproxy_config_last_apply_timestamp_seconds{"+sel+"}[2m]) > 0"),
			annotation("HAProxy crashed", "red", "increase(janus_haproxy_unexpected_exits_total{"+sel+"}[2m]) > 0"),
		}},
		"templating": obj{"list": []any{
			obj{"name": "datasource", "label": "Data source", "type": "datasource", "query": "prometheus",
				"current": obj{}, "hide": 0, "refresh": 1, "regex": "", "includeAll": false, "multi": false, "options": []any{}},
			variable("job", "Job", "label_values(janus_build_info, job)", false),
			variable("instance", "Node", `label_values(up{job="$job"}, instance)`, true),
		}},
		"panels": b.panels,
	}
}

func render(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // PromQL's < and >, as written
	enc.SetIndent("", "  ")
	if err := enc.Encode(dashboard()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDashboard(t *testing.T) {
	got := render(t)
	if *update {
		if err := os.WriteFile(dashboardFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s isn't what dashboard_test.go builds: go test ./grafana -run TestDashboard -update", dashboardFile)
	}
}

// exprs is every query of the dashboard: panels, collapsed rows'
// panels, annotations.
func exprs(t *testing.T) []string {
	t.Helper()
	var d struct {
		Panels      []panelJSON `json:"panels"`
		Annotations struct {
			List []struct {
				Expr string `json:"expr"`
			} `json:"list"`
		} `json:"annotations"`
	}
	if err := json.Unmarshal(render(t), &d); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func([]panelJSON)
	walk = func(ps []panelJSON) {
		for _, p := range ps {
			for _, tg := range p.Targets {
				out = append(out, tg.Expr)
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	for _, a := range d.Annotations.List {
		if a.Expr != "" {
			out = append(out, a.Expr)
		}
	}
	return out
}

type panelJSON struct {
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
	Panels []panelJSON `json:"panels"`
}

var metricName = regexp.MustCompile(`\bjanus_[a-z_]+\b`)

// exported is every metric janusd's exporter serves (cmd/janusd/metrics.go).
func exported(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../cmd/janusd/metrics.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range regexp.MustCompile(`\b(?:gauge|counter)\("(janus_[a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		names = append(names, m[1])
	}
	if len(names) < 30 {
		t.Fatalf("found %d metrics in cmd/janusd/metrics.go: the pattern no longer matches how it declares them", len(names))
	}
	return names
}

// Every metric the dashboard queries is one janusd exports, and every
// metric janusd exports is shown or listed in notShown: a new metric gets
// a panel or a reason.
func TestDashboardMetrics(t *testing.T) {
	served := exported(t)
	used := map[string]bool{}
	for _, e := range exprs(t) {
		for _, m := range metricName.FindAllString(e, -1) {
			used[m] = true
			if !slices.Contains(served, m) {
				t.Errorf("the dashboard queries %s, which janusd doesn't export", m)
			}
		}
	}
	for _, m := range served {
		_, skipped := notShown[m]
		switch {
		case used[m] && skipped:
			t.Errorf("%s is shown: remove it from notShown", m)
		case !used[m] && !skipped:
			t.Errorf("janusd exports %s, which no panel shows: add a panel, or a reason to notShown", m)
		}
	}
	for m := range notShown {
		if !slices.Contains(served, m) {
			t.Errorf("notShown lists %s, which janusd doesn't export", m)
		}
	}
}

// Every selector of a Janus metric (or of up) keeps to the nodes the
// variables chose.
func TestDashboardSelectsItsNodes(t *testing.T) {
	selector := regexp.MustCompile(`\b(janus_[a-z_]+|up)\b(\{[^}]*\})?`)
	for _, e := range exprs(t) {
		for _, m := range selector.FindAllStringSubmatch(e, -1) {
			if !strings.Contains(m[2], `job="$job"`) || !strings.Contains(m[2], `instance=~"$instance"`) {
				t.Errorf("%s%s doesn't keep to $job and $instance: %s", m[1], m[2], e)
			}
		}
	}
}

// The extensions' columns are the official extensions (extensions/).
func TestDashboardExtensions(t *testing.T) {
	entries, err := os.ReadDir("../extensions")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if _, err := os.Stat("../extensions/" + e.Name() + "/manifest.json"); err == nil {
			names = append(names, e.Name())
		}
	}
	if !slices.Equal(names, extensionColumns) {
		t.Errorf("extensionColumns = %v, extensions/ has %v", extensionColumns, names)
	}
}
