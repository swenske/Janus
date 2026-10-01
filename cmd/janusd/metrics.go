package main

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/api"
	"github.com/swenske/Janus/internal/bootcommit"
	"github.com/swenske/Janus/internal/exporter"
	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/firewall"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/kmsgwatch"
	"github.com/swenske/Janus/internal/netmgr"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/timesync"
)

// metricsSources is what the node's exporter reads at scrape time.
type metricsSources struct {
	version    string
	started    time.Time
	ca         *pki.CA
	serverCert *pki.ServerCert
	haproxy    *haproxy.Manager
	ext        *extensions.Manager
	net        *netmgr.Manager
	time       *timesync.Service
	kmsg       *kmsgwatch.Counts
	firewall   *firewall.Manager
	statePath  string // STATE's mount point, for its filesystem's error count

	certsMu      sync.Mutex
	certsAt      time.Time
	certsCache   []haproxy.CertInfo
	certsCacheOK bool
}

// haproxyCertTTL bounds how often a scrape asks HAProxy for its
// certificates - one stats socket command per certificate.
const haproxyCertTTL = time.Minute

func (m *metricsSources) collectors() []exporter.Collector {
	return []exporter.Collector{m.node, m.certificates, m.haproxyMetrics, m.services, m.network, m.firewallMetrics, m.security, m.apiRequests}
}

func gauge(name, help string, samples ...exporter.Sample) exporter.Family {
	return exporter.Family{Name: name, Help: help, Type: exporter.Gauge, Samples: samples}
}

func counter(name, help string, samples ...exporter.Sample) exporter.Family {
	return exporter.Family{Name: name, Help: help, Type: exporter.Counter, Samples: samples}
}

func sample(v float64, labels ...string) exporter.Sample {
	return exporter.Sample{Labels: exporter.L(labels...), Value: v}
}

func (m *metricsSources) node() []exporter.Family {
	schematic := api.CurrentSchematic()
	fams := []exporter.Family{
		gauge("janus_build_info", "The Janus release the node runs, and its image schematic (docs/image-factory.md).",
			sample(1, "version", m.version, "go_version", runtime.Version(), "arch", runtime.GOARCH, "schematic", schematic)),
		gauge("janus_boot_info", "The A/B slot the node booted from, and its kernel.",
			sample(1, "slot", api.CurrentActiveSlot(), "kernel", api.KernelVersion())),
		gauge("janus_daemon_start_time_seconds", "When janusd started, as a Unix timestamp - it changes when janusd restarts.",
			sample(float64(m.started.Unix()))),
	}
	var exts []exporter.Sample
	if m.ext != nil {
		for _, man := range m.ext.Manifests() {
			exts = append(exts, sample(1, "extension", man.Name, "version", man.Version))
		}
	}
	fams = append(fams, gauge("janus_extension_info", "The optional extensions built into the node's image.", exts...))
	marker, err := bootcommit.Read()
	pending := err == nil && marker != nil
	fams = append(fams, gauge("janus_upgrade_pending_confirmation", "1 while an upgrade waits for its health confirmation - the node reverts to the previous slot if it doesn't come.",
		sample(exporter.Bool(pending))))
	return fams
}

func (m *metricsSources) certificates() []exporter.Family {
	var samples []exporter.Sample
	if m.ca != nil && m.ca.Cert != nil {
		samples = append(samples, sample(float64(m.ca.Cert.NotAfter.Unix()), "source", "api", "certificate", "ca", "cn", m.ca.Cert.Subject.CommonName))
	}
	if m.serverCert != nil {
		if t := m.serverCert.NotAfter(); !t.IsZero() {
			samples = append(samples, sample(float64(t.Unix()), "source", "api", "certificate", "server", "cn", ""))
		}
	}
	for _, c := range m.haproxyCerts() {
		t, err := time.Parse(time.RFC3339, c.NotAfter)
		if err != nil {
			continue
		}
		samples = append(samples, sample(float64(t.Unix()), "source", "haproxy", "certificate", c.Name, "cn", commonName(c.Subject)))
	}
	return []exporter.Family{gauge("janus_certificate_expiry_timestamp_seconds",
		"When a certificate expires, as a Unix timestamp: the node API's CA and server certificates, and every certificate HAProxy has loaded.", samples...)}
}

// haproxyCerts returns HAProxy's certificates, cached for haproxyCertTTL.
func (m *metricsSources) haproxyCerts() []haproxy.CertInfo {
	m.certsMu.Lock()
	defer m.certsMu.Unlock()
	if m.certsCacheOK && time.Since(m.certsAt) < haproxyCertTTL {
		return m.certsCache
	}
	certs, err := m.haproxy.CertificateList()
	m.certsAt = time.Now()
	if err != nil {
		m.certsCacheOK = false
		return nil
	}
	m.certsCache, m.certsCacheOK = certs, true
	return certs
}

// commonName pulls CN out of an OpenSSL one-line subject
// ("/O=Example/CN=www.example.com").
func commonName(subject string) string {
	for _, part := range strings.Split(subject, "/") {
		if v, ok := strings.CutPrefix(part, "CN="); ok {
			return v
		}
	}
	return ""
}

func (m *metricsSources) haproxyMetrics() []exporter.Family {
	_, err := m.haproxy.ShowInfo()
	c := m.haproxy.Counters()
	fams := []exporter.Family{
		gauge("janus_haproxy_up", "1 if HAProxy answers on its stats socket. HAProxy's own metrics can't say it's down.", sample(exporter.Bool(err == nil))),
		counter("janus_haproxy_starts_total", "HAProxy processes janusd started, reloads included.", sample(float64(c.Starts.Load()))),
		counter("janus_haproxy_reloads_total", "Seamless HAProxy reloads.", sample(float64(c.Reloads.Load()))),
		counter("janus_haproxy_unexpected_exits_total", "HAProxy processes that exited without being stopped or replaced by a reload.", sample(float64(c.UnexpectedExits.Load()))),
		counter("janus_haproxy_config_applies_total", "HAProxy configurations applied through the API, by result.",
			sample(float64(c.ApplyAccepted.Load()), "result", "accepted"), sample(float64(c.ApplyRejected.Load()), "result", "rejected")),
	}
	if t := c.LastApplyUnix.Load(); t > 0 {
		fams = append(fams, gauge("janus_haproxy_config_last_apply_timestamp_seconds", "When the last HAProxy configuration was applied through the API.", sample(float64(t))))
	}
	return fams
}

var serviceStates = []string{"running", "waiting", "restarting", "stopped"}

func (m *metricsSources) services() []exporter.Family {
	if m.ext == nil {
		return nil
	}
	var states, restarts []exporter.Sample
	for _, id := range m.ext.ServiceIDs() {
		st, err := m.ext.State(id)
		if err != nil {
			continue
		}
		for _, s := range serviceStates {
			states = append(states, sample(exporter.Bool(st.State == s), "service", id, "extension", st.Extension, "state", s))
		}
		restarts = append(restarts, sample(float64(st.Restarts), "service", id, "extension", st.Extension))
	}
	return []exporter.Family{
		gauge("janus_service_state", "The state of each extension service janusd runs: 1 for its current state.", states...),
		counter("janus_service_restarts_total", "Times an extension service exited and was restarted.", restarts...),
	}
}

func (m *metricsSources) network() []exporter.Family {
	var fams []exporter.Family
	if m.time != nil {
		ts := m.time.Status()
		fams = append(fams, gauge("janus_time_synchronized", "1 if janusd's NTP client has set the clock.", sample(exporter.Bool(ts.GetSynchronized()))))
		if ts.GetLastSyncUnix() > 0 {
			fams = append(fams,
				gauge("janus_time_last_sync_timestamp_seconds", "When the clock was last synchronized over NTP.", sample(float64(ts.GetLastSyncUnix()))),
				gauge("janus_time_offset_seconds", "The clock's offset measured at the last NTP synchronization.", sample(float64(ts.GetOffsetNs())/1e9)),
				gauge("janus_time_stratum", "The NTP stratum of the server last synchronized from.", sample(float64(ts.GetStratum()))))
		}
	}
	if m.net != nil {
		pending, revertAt := m.net.Trial()
		fams = append(fams, gauge("janus_network_trial_pending", "1 while a network configuration is on trial: it reverts unless confirmed.", sample(exporter.Bool(pending))))
		if pending {
			fams = append(fams, gauge("janus_network_trial_revert_timestamp_seconds", "When the network configuration on trial reverts unless confirmed.", sample(float64(revertAt.Unix()))))
		}
	}
	return fams
}

func (m *metricsSources) firewallMetrics() []exporter.Family {
	if m.firewall == nil || !firewall.Available() {
		return nil
	}
	_, isDefault, _ := firewall.Saved()
	pending, _ := m.firewall.Trial()
	fams := []exporter.Family{
		gauge("janus_firewall_configured", "1 if the node has a saved firewall ruleset (the nftables extension).", sample(exporter.Bool(!isDefault))),
		gauge("janus_firewall_trial_pending", "1 while a firewall ruleset is on trial: it reverts unless confirmed.", sample(exporter.Bool(pending))),
	}
	if sets, err := m.firewall.Sets(); err == nil {
		var samples []exporter.Sample
		for _, s := range sets {
			samples = append(samples, sample(float64(len(s.Elements)), "family", s.Family, "table", s.Table, "set", s.Name))
		}
		fams = append(fams, gauge("janus_firewall_set_elements", "Elements in each named set of the live firewall ruleset.", samples...))
	}
	return fams
}

func (m *metricsSources) security() []exporter.Family {
	var fams []exporter.Family
	if data, err := os.ReadFile("/sys/fs/selinux/enforce"); err == nil {
		fams = append(fams, gauge("janus_selinux_enforcing", "1 if SELinux is enforcing, 0 if permissive.", sample(exporter.Bool(strings.TrimSpace(string(data)) == "1"))))
	}
	if m.kmsg != nil && m.kmsg.Running.Load() {
		fams = append(fams,
			counter("janus_selinux_denials_total", "SELinux denials in the kernel log since boot - there should be none.", sample(float64(m.kmsg.SELinuxDenials.Load()))),
			counter("janus_kernel_oom_kills_total", "Processes the kernel killed for lack of memory since boot.", sample(float64(m.kmsg.OOMKills.Load()))))
	}
	if n, ok := ext4Errors(m.statePath); ok {
		fams = append(fams, gauge("janus_state_filesystem_errors", "Errors the kernel recorded on the STATE filesystem (PKI, configuration) - it should be 0.", sample(float64(n))))
	}
	return fams
}

// ext4Errors reads the error count of the ext4 filesystem mounted at
// mountPoint, from /sys/fs/ext4/<device>/errors_count.
func ext4Errors(mountPoint string) (uint64, bool) {
	if mountPoint == "" {
		return 0, false
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// "36 25 254:6 / /etc/.state rw,relatime - ext4 /dev/vda6 rw"
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[4] != mountPoint {
			continue
		}
		sep := 6
		for sep < len(fields) && fields[sep] != "-" {
			sep++
		}
		if sep+2 >= len(fields) || fields[sep+1] != "ext4" {
			return 0, false
		}
		data, err := os.ReadFile(filepath.Join("/sys/fs/ext4", filepath.Base(fields[sep+2]), "errors_count"))
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (m *metricsSources) apiRequests() []exporter.Family {
	var samples []exporter.Sample
	for _, c := range api.APIRequests.Snapshot() {
		samples = append(samples, sample(float64(c.N), "method", c.Method, "code", c.Code))
	}
	return []exporter.Family{counter("janus_api_requests_total", "Calls to the node's API since janusd started, by method and status code - refused ones included.", samples...)}
}
