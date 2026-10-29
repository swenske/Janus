// Package telemetry is the Controller's opt-in usage report
// (docs/guide/telemetry.md): once a day, what its fleet is made of - how
// many nodes and clusters, which versions, extensions, variants,
// platforms, sizes and features - for the project to know what to work
// on, what could be retired and which images to pre-build first. Never a
// name, an address, a label or a configuration; nothing is sent unless
// an admin turned it on.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Schema is the report's format: a field added or changed takes the next
// number.
const Schema = 1

// DefaultURL is where reports go (dashboardd -telemetry-url).
const DefaultURL = "https://janus-telemetry.sw-servers.net/v1/report"

// Report is what a Controller sends. Counts only: every map is how many
// nodes have a value, never which.
type Report struct {
	Schema     int        `json:"schema"`
	InstallID  string     `json:"install_id"`
	Controller Controller `json:"controller"`
	Nodes      Nodes      `json:"nodes"`
	Clusters   Clusters   `json:"clusters"`
	Platforms  Platforms  `json:"platforms"`
	Features   Features   `json:"features"`
}

// Controller is the Controller itself.
type Controller struct {
	Version    string `json:"version"`
	Arch       string `json:"arch"`
	Prerelease bool   `json:"prerelease"`
}

// Nodes are the nodes the Controller manages. Unreachable ones count in
// Total only.
type Nodes struct {
	Total       int `json:"total"`
	Unreachable int `json:"unreachable"`
	// By the Janus version they run, their architecture, the HAProxy
	// branch and kernel track their image is built with, its console,
	// each extension it has, and its schematic ID (content-addressed:
	// which choices, docs/image-factory.md).
	Versions   map[string]int `json:"versions"`
	Arches     map[string]int `json:"arches"`
	HAProxy    map[string]int `json:"haproxy"`
	Kernel     map[string]int `json:"kernel"`
	Console    map[string]int `json:"console"`
	Extensions map[string]int `json:"extensions"`
	Schematics map[string]int `json:"schematics"`
	// VCPUs and Memory: their size, by bucket (VCPUBucket, MemoryBucket).
	VCPUs  map[string]int `json:"vcpus"`
	Memory map[string]int `json:"memory"`
}

// Clusters are the Controller's clusters: how many members each has,
// smallest first.
type Clusters struct {
	Count int   `json:"count"`
	Sizes []int `json:"sizes"`
}

// Platforms: the machines the Controller created, by kind of hypervisor
// ("libvirt", "proxmox"), and the nodes added to it otherwise.
type Platforms struct {
	Machines map[string]int `json:"machines"`
	Added    int            `json:"added"`
}

// Features are what the Controller is set up to do - on or off, nothing
// of how.
type Features struct {
	SSO bool `json:"sso"`
	// MFARequired: a second factor is required of some accounts (the
	// admins, or everyone).
	MFARequired bool `json:"mfa_required"`
	// Autoscaling: a machine or a cluster scales by itself.
	Autoscaling bool `json:"autoscaling"`
	// ClusterBGP and ClusterVRRP: a cluster manages its members' BGP or
	// VRRP.
	ClusterBGP  bool `json:"cluster_bgp"`
	ClusterVRRP bool `json:"cluster_vrrp"`
	// LogForwarding: the Controller ships its own logs, or the nodes'
	// through profiles.
	LogForwarding bool `json:"log_forwarding"`
	// LetsEncrypt: nodes reach Let's Encrypt through the Controller, or
	// it orders their certificates.
	LetsEncrypt bool `json:"lets_encrypt"`
	// S3Backups: the Controller backs itself up to S3.
	S3Backups bool `json:"s3_backups"`
	// Fleet: the nodes trust the Controller's fleet PKI.
	Fleet  bool `json:"fleet"`
	Consul bool `json:"consul"`
}

// NewReport is an empty report, its maps made.
func NewReport(installID string) *Report {
	return &Report{Schema: Schema, InstallID: installID,
		Nodes: Nodes{Versions: map[string]int{}, Arches: map[string]int{}, HAProxy: map[string]int{}, Kernel: map[string]int{},
			Console: map[string]int{}, Extensions: map[string]int{}, Schematics: map[string]int{}, VCPUs: map[string]int{}, Memory: map[string]int{}},
		Clusters:  Clusters{Sizes: []int{}},
		Platforms: Platforms{Machines: map[string]int{}},
	}
}

// Node is what a report counts of a node.
type Node struct {
	Version, Arch, HAProxy, Kernel, Console, Schematic string
	Extensions                                         []string
	VCPUs                                              int
	MemoryBytes                                        uint64
}

// AddNode counts a node; nil is one that couldn't be asked.
func (r *Report) AddNode(n *Node) {
	r.Nodes.Total++
	if n == nil {
		r.Nodes.Unreachable++
		return
	}
	inc := func(m map[string]int, k string) {
		if k != "" {
			m[k]++
		}
	}
	inc(r.Nodes.Versions, n.Version)
	inc(r.Nodes.Arches, n.Arch)
	inc(r.Nodes.HAProxy, n.HAProxy)
	inc(r.Nodes.Kernel, n.Kernel)
	inc(r.Nodes.Console, n.Console)
	inc(r.Nodes.Schematics, n.Schematic)
	for _, e := range n.Extensions {
		inc(r.Nodes.Extensions, e)
	}
	if n.VCPUs > 0 {
		inc(r.Nodes.VCPUs, VCPUBucket(n.VCPUs))
	}
	if n.MemoryBytes > 0 {
		inc(r.Nodes.Memory, MemoryBucket(n.MemoryBytes))
	}
}

// AddCluster counts a cluster of members members.
func (r *Report) AddCluster(members int) {
	r.Clusters.Count++
	r.Clusters.Sizes = append(r.Clusters.Sizes, members)
	slices.Sort(r.Clusters.Sizes)
}

// VCPUBucket is a node's vCPU count, coarsely: "1", "2", "4" (4 to 7)
// or "8+".
func VCPUBucket(n int) string {
	switch {
	case n <= 1:
		return "1"
	case n < 4:
		return "2"
	case n < 8:
		return "4"
	default:
		return "8+"
	}
}

// MemoryBucket is a node's memory, coarsely: "<1GiB", "1-2GiB", "2-4GiB"
// or "4GiB+".
func MemoryBucket(b uint64) string {
	const gib = 1 << 30
	switch {
	case b < gib:
		return "<1GiB"
	case b < 2*gib:
		return "1-2GiB"
	case b < 4*gib:
		return "2-4GiB"
	default:
		return "4GiB+"
	}
}

// client sends reports: the environment's proxy, no redirect followed,
// 30 s at most.
var client = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Send posts r to url.
func Send(ctx context.Context, url string, r *Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return call(ctx, http.MethodPost, url, body)
}

// Forget asks the receiver at url (Send's) to delete what it holds of
// installID: a regenerated ID leaves nothing behind.
func Forget(ctx context.Context, url, installID string) error {
	base, ok := strings.CutSuffix(url, "/report")
	if !ok {
		return errors.New("the telemetry URL doesn't end in /report")
	}
	return call(ctx, http.MethodDelete, base+"/installs/"+installID, nil)
}

func call(ctx context.Context, method, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
