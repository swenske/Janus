// Package support writes a node's diagnostic bundle - what to attach to
// an issue or hand to whoever helps: one tar.gz of what the node's API
// says about it (its version and image, its events, its services' and
// kernel's logs of this boot and the one before, HAProxy's state and
// configuration, the network, the kernel parameters, processes,
// mounts...), read with the caller's own rights, never a secret. Shared
// by janusctl (system support) and the Controller (a node's Tools ›
// Diagnostic bundle).
//
// What the caller may not read, or the node's version doesn't have, is
// left out, and MANIFEST.json says so. The configurations the node
// doesn't mask (BGP's and VRRP's passwords, Consul's keys) are never
// asked for; haproxy.cfg is masked here whoever reads it, and every PEM
// private key is cut out of every file.
package support

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/kmsgwatch"
)

// Format names the bundle's layout.
const Format = "janus-support 1"

// Collector writes a bundle.
type Collector struct {
	Conn *grpc.ClientConn
	// Allowed tells whether the caller may call a method ("Service/
	// Method"): what it may not is left out rather than refused by the
	// node (and kept in its audit as a refusal). Nil: every method.
	Allowed func(method string) bool
	// Node, By and Role are written into the manifest: the node's name,
	// who made the bundle with what, the caller's role.
	Node, By, Role string
	// MaxFile caps each file - a log keeps its end: 8 MiB when 0.
	MaxFile int
	// CallTimeout bounds each call: 15 s when 0.
	CallTimeout time.Duration
	// Now is the clock: time.Now when nil.
	Now func() time.Time
}

// Manifest is MANIFEST.json: what the bundle holds, and what it doesn't
// and why.
type Manifest struct {
	Format   string    `json:"format"`
	Node     string    `json:"node,omitempty"`
	Created  time.Time `json:"created"`
	By       string    `json:"by,omitempty"`
	Role     string    `json:"role,omitempty"`
	Sections []Section `json:"sections"`
}

// Section is one file of the bundle, or why it's missing.
type Section struct {
	File   string `json:"file"`
	Method string `json:"method,omitempty"`
	// Status: included, truncated (its end kept), skipped (the role, the
	// node's version, a module the image hasn't), failed.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
}

const readme = `A Janus node's diagnostic bundle, read over its API.

MANIFEST.json lists every file, and what was left out and why: what the
role that made it may not read, what the node's version doesn't have.
Never in it: the node's credentials, PEM private keys (cut out of every
file), the configurations holding passwords the node doesn't mask
(BGP, VRRP, Consul). haproxy.cfg's passwords, credential headers and
setenv values are masked.

Logs are as the node wrote them: a URL with a token in its query, a
client's address, are in them. Read them before sharing the bundle.
`

var privateKey = regexp.MustCompile(`(?s)-----BEGIN ([A-Z0-9 ]*)PRIVATE KEY-----.*?-----END ([A-Z0-9 ]*)PRIVATE KEY-----`)

// stripKeys cuts every PEM private key out of b.
func stripKeys(b []byte) []byte {
	return privateKey.ReplaceAll(b, []byte("[private key removed from the bundle]"))
}

// Write writes the bundle to w, and returns its manifest. An error is
// w's - what the node didn't give is in the manifest.
func (c *Collector) Write(ctx context.Context, w io.Writer) (*Manifest, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	b := &bundle{c: c, m: &Manifest{Format: Format, Node: c.Node, Created: now().UTC(), By: c.By, Role: c.Role}, at: now()}
	gz := gzip.NewWriter(w)
	b.tw = tar.NewWriter(gz)
	if err := b.add("README.txt", []byte(readme)); err != nil {
		return nil, err
	}
	steps := []func(context.Context) error{b.unaries, b.haproxyConfig, b.events, b.logs, b.kernel}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return nil, err
		}
	}
	manifest, _ := json.MarshalIndent(b.m, "", "  ")
	if err := b.add("MANIFEST.json", append(manifest, '\n')); err != nil {
		return nil, err
	}
	if err := b.tw.Close(); err != nil {
		return nil, err
	}
	return b.m, gz.Close()
}

type bundle struct {
	c  *Collector
	m  *Manifest
	tw *tar.Writer
	at time.Time
}

func (b *bundle) maxFile() int {
	if b.c.MaxFile > 0 {
		return b.c.MaxFile
	}
	return 8 << 20
}

func (b *bundle) timeout() time.Duration {
	if b.c.CallTimeout > 0 {
		return b.c.CallTimeout
	}
	return 15 * time.Second
}

func (b *bundle) allowed(method string) bool {
	return b.c.Allowed == nil || b.c.Allowed(method)
}

// add writes one file, private keys cut out.
func (b *bundle) add(name string, data []byte) error {
	data = stripKeys(data)
	if err := b.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: b.at, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := b.tw.Write(data)
	return err
}

// file adds a section's file and records it.
func (b *bundle) file(name, method string, data []byte, truncated bool) error {
	s := Section{File: name, Method: method, Status: "included", Bytes: len(data)}
	if truncated {
		s.Status, s.Reason = "truncated", fmt.Sprintf("its last %d MiB", b.maxFile()>>20)
	}
	b.m.Sections = append(b.m.Sections, s)
	return b.add(name, data)
}

// skip records a section left out.
func (b *bundle) skip(name, method, reason string) {
	b.m.Sections = append(b.m.Sections, Section{File: name, Method: method, Status: "skipped", Reason: reason})
}

// failed records what the node didn't give: skipped when it's the
// node's version or the image, failed otherwise.
func (b *bundle) failed(name, method string, err error) {
	st := status.Convert(err)
	switch st.Code() {
	case codes.Unimplemented:
		b.skip(name, method, "not in this node's version")
	case codes.FailedPrecondition:
		b.skip(name, method, st.Message())
	case codes.PermissionDenied:
		b.skip(name, method, "refused to this role: "+st.Message())
	default:
		b.m.Sections = append(b.m.Sections, Section{File: name, Method: method, Status: "failed", Reason: st.Message()})
	}
}

type unary struct {
	file, method string
	call         func(context.Context, *grpc.ClientConn) (proto.Message, error)
}

var empty = &emptypb.Empty{}

func sys(c *grpc.ClientConn) janusv1alpha1.SystemServiceClient {
	return janusv1alpha1.NewSystemServiceClient(c)
}
func hap(c *grpc.ClientConn) janusv1alpha1.HAProxyServiceClient {
	return janusv1alpha1.NewHAProxyServiceClient(c)
}
func netw(c *grpc.ClientConn) janusv1alpha1.NetworkServiceClient {
	return janusv1alpha1.NewNetworkServiceClient(c)
}

// unaries are the sections one call answers, written as JSON.
var unaries = []unary{
	{"version.json", "SystemService/Version", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).Version(x, empty) }},
	{"system/stats.json", "SystemService/Stats", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).Stats(x, empty) }},
	{"system/systemstat.json", "SystemService/SystemStat", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).SystemStat(x, empty) }},
	{"system/memory.json", "SystemService/Memory", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).Memory(x, empty) }},
	{"system/cpuinfo.json", "SystemService/CPUInfo", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).CPUInfo(x, empty) }},
	{"system/loadavg.json", "SystemService/LoadAvg", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).LoadAvg(x, empty) }},
	{"system/diskstats.json", "SystemService/DiskStats", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).DiskStats(x, empty) }},
	{"system/mounts.json", "SystemService/Mounts", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).Mounts(x, empty) }},
	{"system/processes.json", "SystemService/Processes", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).Processes(x, empty) }},
	{"system/services.json", "SystemService/ServiceList", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).ServiceList(x, empty) }},
	{"system/sysctl.json", "SystemService/SysctlList", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).SysctlList(x, empty) }},
	{"system/sysctl-history.json", "SystemService/SysctlHistory", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) {
		return sys(c).SysctlHistory(x, &janusv1alpha1.SysctlHistoryRequest{})
	}},
	{"system/log-boots.json", "SystemService/LogBoots", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).LogBoots(x, empty) }},
	{"system/log-forwarding.json", "SystemService/LogForwardingGet", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).LogForwardingGet(x, empty) }},
	{"system/metrics.json", "SystemService/MetricsConfigGet", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).MetricsConfigGet(x, empty) }},
	{"network/status.json", "NetworkService/NetworkStatus", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return netw(c).NetworkStatus(x, empty) }},
	{"network/config.json", "NetworkService/NetworkConfigGet", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return netw(c).NetworkConfigGet(x, empty) }},
	{"network/neighbors.json", "NetworkService/NeighborList", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) {
		return netw(c).NeighborList(x, &janusv1alpha1.NeighborListRequest{})
	}},
	{"network/netdev.json", "SystemService/NetworkDeviceStats", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).NetworkDeviceStats(x, empty) }},
	{"network/netstat.json", "SystemService/Netstat", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return sys(c).Netstat(x, empty) }},
	{"network/firewall.json", "NetworkService/FirewallGetRuleset", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return netw(c).FirewallGetRuleset(x, empty) }},
	{"network/firewall-sets.json", "NetworkService/FirewallSets", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return netw(c).FirewallSets(x, empty) }},
	{"network/vrrp.json", "NetworkService/VRRPStatus", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return netw(c).VRRPStatus(x, empty) }},
	{"network/bgp.json", "NetworkService/BGPStatus", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return netw(c).BGPStatus(x, empty) }},
	{"network/consul.json", "NetworkService/ConsulStatus", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return netw(c).ConsulStatus(x, empty) }},
	{"haproxy/info.json", "HAProxyService/ShowInfo", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return hap(c).ShowInfo(x, empty) }},
	{"haproxy/backends.json", "HAProxyService/BackendList", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return hap(c).BackendList(x, empty) }},
	{"haproxy/certificates.json", "HAProxyService/CertificateList", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return hap(c).CertificateList(x, empty) }},
	{"haproxy/files.json", "HAProxyService/FileList", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return hap(c).FileList(x, empty) }},
	{"haproxy/maps.json", "HAProxyService/MapList", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return hap(c).MapList(x, empty) }},
	{"haproxy/acme-status.json", "HAProxyService/ACMEStatus", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return hap(c).ACMEStatus(x, empty) }},
	{"haproxy/acme-config.json", "HAProxyService/ACMEGetConfig", func(x context.Context, c *grpc.ClientConn) (proto.Message, error) { return hap(c).ACMEGetConfig(x, empty) }},
}

var protoOut = protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}

func (b *bundle) unaries(ctx context.Context) error {
	for _, u := range unaries {
		if !b.allowed(u.method) {
			b.skip(u.file, u.method, "not for this role")
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, b.timeout())
		resp, err := u.call(cctx, b.c.Conn)
		cancel()
		if err != nil {
			b.failed(u.file, u.method, err)
			continue
		}
		data, err := protoOut.Marshal(resp)
		if err != nil {
			b.failed(u.file, u.method, err)
			continue
		}
		if err := b.file(u.file, u.method, append(data, '\n'), false); err != nil {
			return err
		}
	}
	// HAProxy's "show stat", as the CSV it is.
	const method = "HAProxyService/Stats"
	if !b.allowed(method) {
		b.skip("haproxy/stat.csv", method, "not for this role")
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, b.timeout())
	defer cancel()
	stats, err := hap(b.c.Conn).Stats(cctx, empty)
	if err != nil {
		b.failed("haproxy/stat.csv", method, err)
		return nil
	}
	return b.file("haproxy/stat.csv", method, stats.GetRawCsv(), false)
}

// haproxyConfig adds haproxy.cfg, masked - the node masks it for a
// reader, the bundle for everyone.
func (b *bundle) haproxyConfig(ctx context.Context) error {
	const name, method = "haproxy/haproxy.cfg", "HAProxyService/GetConfig"
	if !b.allowed(method) {
		b.skip(name, method, "not for this role")
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, b.timeout())
	defer cancel()
	resp, err := hap(b.c.Conn).GetConfig(cctx, empty)
	if err != nil {
		b.failed(name, method, err)
		return nil
	}
	return b.file(name, method, haproxy.RedactConfig(resp.GetConfig()), false)
}

// events adds the event log and the audit log: streams that follow once
// their backlog is sent - read until they've been quiet a moment.
func (b *bundle) events(ctx context.Context) error {
	for _, s := range []struct{ file, method string }{{"events.jsonl", "SystemService/Events"}, {"audit.jsonl", "SystemService/Audit"}} {
		if !b.allowed(s.method) {
			b.skip(s.file, s.method, "not for this role")
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, b.timeout())
		var stream grpc.ServerStreamingClient[janusv1alpha1.Event]
		var err error
		if s.method == "SystemService/Events" {
			stream, err = sys(b.c.Conn).Events(cctx, &janusv1alpha1.EventsRequest{})
		} else {
			stream, err = sys(b.c.Conn).Audit(cctx, &janusv1alpha1.EventsRequest{})
		}
		var out bytes.Buffer
		if err == nil {
			err = drain(stream, func(e *janusv1alpha1.Event) {
				line, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(e)
				out.Write(append(line, '\n'))
			})
		}
		cancel()
		if err != nil {
			b.failed(s.file, s.method, err)
			continue
		}
		data, truncated := b.keepEnd(out.Bytes())
		if err := b.file(s.file, s.method, data, truncated); err != nil {
			return err
		}
	}
	return nil
}

// quiet is how long a following stream may say nothing before its
// backlog is taken to be all sent.
const quiet = 700 * time.Millisecond

// drain reads a stream that never ends by itself until it's been quiet
// for a moment, or ends, or its context does.
func drain[T any](stream grpc.ServerStreamingClient[T], each func(*T)) error {
	type item struct {
		v   *T
		err error
	}
	items := make(chan item, 64)
	go func() {
		for {
			v, err := stream.Recv()
			items <- item{v, err}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(quiet * 3) // the backlog's first message may take longer
	defer timer.Stop()
	for {
		select {
		case it := <-items:
			if errors.Is(it.err, io.EOF) {
				return nil
			}
			if it.err != nil {
				return it.err
			}
			each(it.v)
			timer.Reset(quiet)
		case <-timer.C:
			return nil
		}
	}
}

// logs adds each service's logs of this boot and the one before - those
// the LOGS partition keeps.
func (b *bundle) logs(ctx context.Context) error {
	const method = "SystemService/Logs"
	if !b.allowed(method) {
		b.skip("logs/", method, "not for this role")
		return nil
	}
	services := []string{"janusd", "haproxy"}
	if b.allowed("SystemService/ServiceList") {
		cctx, cancel := context.WithTimeout(ctx, b.timeout())
		if list, err := sys(b.c.Conn).ServiceList(cctx, empty); err == nil {
			services = services[:0]
			for _, s := range list.GetServices() {
				services = append(services, s.GetId())
			}
		}
		cancel()
	}
	for _, boot := range b.boots(ctx) {
		for _, id := range services {
			name := fmt.Sprintf("logs/%s%s.log", id, bootSuffix(boot))
			cctx, cancel := context.WithTimeout(ctx, b.timeout())
			stream, err := sys(b.c.Conn).Logs(cctx, &janusv1alpha1.LogsRequest{Id: id, Boot: boot})
			var data []byte
			truncated := false
			if err == nil {
				data, truncated, err = b.readData(stream)
			}
			cancel()
			if err != nil {
				b.failed(name, method, err)
				continue
			}
			if err := b.file(name, method, data, truncated); err != nil {
				return err
			}
		}
	}
	return nil
}

// boots are the boots whose logs to take: this one, and the one before
// when the node keeps it.
func (b *bundle) boots(ctx context.Context) []int32 {
	boots := []int32{0}
	if !b.allowed("SystemService/LogBoots") {
		return boots
	}
	cctx, cancel := context.WithTimeout(ctx, b.timeout())
	defer cancel()
	resp, err := sys(b.c.Conn).LogBoots(cctx, empty)
	if err != nil {
		return boots
	}
	for _, lb := range resp.GetBoots() {
		if lb.GetOffset() == -1 {
			return append(boots, -1)
		}
	}
	return boots
}

func bootSuffix(boot int32) string {
	if boot == 0 {
		return ""
	}
	return fmt.Sprintf(".boot%d", boot)
}

// kernel adds the kernel's messages of this boot and the one before, and
// the SELinux denials among them.
func (b *bundle) kernel(ctx context.Context) error {
	const method = "SystemService/Dmesg"
	if !b.allowed(method) {
		b.skip("kernel/dmesg.log", method, "not for this role")
		b.skip("kernel/avc-denials.log", method, "not for this role")
		return nil
	}
	var denials bytes.Buffer
	for _, boot := range b.boots(ctx) {
		name := fmt.Sprintf("kernel/dmesg%s.log", bootSuffix(boot))
		cctx, cancel := context.WithTimeout(ctx, b.timeout())
		stream, err := sys(b.c.Conn).Dmesg(cctx, &janusv1alpha1.DmesgRequest{Boot: boot})
		var data []byte
		truncated := false
		if err == nil {
			data, truncated, err = b.readData(stream)
		}
		cancel()
		if err != nil {
			b.failed(name, method, err)
			continue
		}
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if denial, _ := kmsgwatch.Classify(line); denial {
				denials.WriteString(line)
			}
		}
		if err := b.file(name, method, data, truncated); err != nil {
			return err
		}
	}
	return b.file("kernel/avc-denials.log", method, denials.Bytes(), false)
}

// readData reads a Data stream, keeping its end past MaxFile.
func (b *bundle) readData(stream grpc.ServerStreamingClient[janusv1alpha1.Data]) ([]byte, bool, error) {
	var out []byte
	cut := false
	for {
		d, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			data, c := b.keepEnd(out)
			if cut && !c {
				data = fromLine(data)
			}
			return data, cut || c, nil
		}
		if err != nil {
			return nil, false, err
		}
		out = append(out, d.GetBytes()...)
		if len(out) > 2*b.maxFile() { // memory stays bounded
			out, cut = out[len(out)-b.maxFile():], true
		}
	}
}

// keepEnd keeps data's last MaxFile bytes, from a line's start.
func (b *bundle) keepEnd(data []byte) ([]byte, bool) {
	if len(data) <= b.maxFile() {
		return data, false
	}
	return fromLine(data[len(data)-b.maxFile():]), true
}

// fromLine drops what precedes data's first whole line.
func fromLine(data []byte) []byte {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return data[i+1:]
	}
	return data
}
