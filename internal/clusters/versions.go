package clusters

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// A version of the cluster's desired configuration: what every member
// should run, kept whole under versions/v<N>/. Its files, by path:
//
//	haproxy.cfg        the same on every member (the generated peers
//	                   section and -L make the difference)
//	files/<name>       HAProxy's files that hold no private key - those
//	                   go member to member (ClusterPull), never here
//	keepalived.conf    templates, rendered per member: .Cluster.ID,
//	bird.conf          .Cluster.Name, .Member.Name, .Member.Index,
//	firewall.nft       .Member.Address, .Member.Vars.<key>, .Peers
//
// A version is never changed: a new one is made.

// Template files and the domain each configures.
var templates = map[string]string{
	"keepalived.conf": "vrrp",
	"bird.conf":       "bgp",
	"firewall.nft":    "firewall",
}

var fileNamePattern = regexp.MustCompile(`^files/[A-Za-z0-9_][A-Za-z0-9._-]{0,99}(/[A-Za-z0-9_][A-Za-z0-9._-]{0,99})?$`)

const (
	maxVersionFile  = 1 << 20
	maxVersionFiles = 256
)

// VersionInfo describes a version.
type VersionInfo struct {
	Version int       `json:"version"`
	Author  string    `json:"author"`
	Created time.Time `json:"created"`
	Note    string    `json:"note,omitempty"`
	Files   []string  `json:"files"`
}

// Version is a version, its files with it.
type Version struct {
	VersionInfo
	Content map[string][]byte `json:"-"`
}

// CheckFiles reports what is wrong with a version's files.
func CheckFiles(files map[string][]byte) error {
	if len(files) > maxVersionFiles {
		return fmt.Errorf("%d files, %d at most", len(files), maxVersionFiles)
	}
	for p, data := range files {
		_, tmpl := templates[p]
		if p != "haproxy.cfg" && !tmpl && !fileNamePattern.MatchString(p) {
			return fmt.Errorf("%s: a version holds haproxy.cfg, files/NAME, keepalived.conf, bird.conf, firewall.nft", p)
		}
		if len(data) > maxVersionFile {
			return fmt.Errorf("%s: %d bytes, %d at most", p, len(data), maxVersionFile)
		}
		if bytes.Contains(data, []byte("PRIVATE KEY-----")) {
			return fmt.Errorf("%s holds a private key: keys go from member to member, never through the Controller - upload it to the cluster as a certificate", p)
		}
		if tmpl {
			if _, err := template.New(p).Option("missingkey=error").Parse(string(data)); err != nil {
				return fmt.Errorf("%s: %v", p, err)
			}
		}
	}
	return nil
}

func (s *Store) versionDir(id string, n int) string {
	return filepath.Join(s.clusterDir(id), "versions", "v"+strconv.Itoa(n))
}

// NewVersion records files as cluster id's next version.
func (s *Store) NewVersion(id, author, note string, files map[string][]byte) (*VersionInfo, error) {
	if err := CheckFiles(files); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clusters[id]; !ok {
		return nil, ErrNotFound
	}
	n := s.lastVersionLocked(id) + 1
	info := VersionInfo{Version: n, Author: author, Created: time.Now().UTC(), Note: note}
	for p := range files {
		info.Files = append(info.Files, p)
	}
	sort.Strings(info.Files)
	dir := s.versionDir(id, n)
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	for p, data := range files {
		if err := writeDurable(filepath.Join(tmp, filepath.FromSlash(p)), data); err != nil {
			return nil, err
		}
	}
	meta, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeDurable(filepath.Join(tmp, "manifest.json"), append(meta, '\n')); err != nil {
		return nil, err
	}
	// Whole or not at all: the directory appears complete.
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	return &info, nil
}

func (s *Store) lastVersionLocked(id string) int {
	entries, _ := os.ReadDir(filepath.Join(s.clusterDir(id), "versions"))
	last := 0
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "v")); err == nil && strings.HasPrefix(e.Name(), "v") && n > last {
			last = n
		}
	}
	return last
}

// Versions are cluster id's versions, newest first.
func (s *Store) Versions(id string) ([]VersionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clusters[id]; !ok {
		return nil, ErrNotFound
	}
	var out []VersionInfo
	for n := s.lastVersionLocked(id); n > 0; n-- {
		data, err := os.ReadFile(filepath.Join(s.versionDir(id, n), "manifest.json"))
		if err != nil {
			continue
		}
		var info VersionInfo
		if json.Unmarshal(data, &info) == nil {
			out = append(out, info)
		}
	}
	return out, nil
}

// ErrNoVersion is a version the cluster doesn't have.
var ErrNoVersion = errors.New("no such version")

// Version is cluster id's version n, with its files.
func (s *Store) Version(id string, n int) (*Version, error) {
	dir := s.versionDir(id, n)
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoVersion
	}
	if err != nil {
		return nil, err
	}
	v := &Version{Content: map[string][]byte{}}
	if err := json.Unmarshal(data, &v.VersionInfo); err != nil {
		return nil, err
	}
	for _, p := range v.Files {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		v.Content[p] = b
	}
	return v, nil
}

// Rendered is a version as one member takes it: by domain, the bytes
// that domain's apply sends, and the files apart.
type Rendered struct {
	HAProxy []byte
	Files   map[string][]byte // by name, under /etc/haproxy/files
	Modules map[string][]byte // vrrp, bgp, firewall
}

// Hashes are the SHA-256 of what the member gets, by domain: what drift
// is measured against.
func (r *Rendered) Hashes() map[string]string {
	out := map[string]string{}
	if r.HAProxy != nil {
		out["haproxy"] = Sum(r.HAProxy)
	}
	if len(r.Files) > 0 {
		out["files"] = FilesHash(r.Files)
	}
	for d, b := range r.Modules {
		out[d] = Sum(b)
	}
	return out
}

// FilesHash is one hash of a set of files: each one's name and content.
func FilesHash(files map[string][]byte) string {
	sums := make(map[string]string, len(files))
	for n, b := range files {
		sums[n] = Sum(b)
	}
	return FilesHashOfSums(sums)
}

// FilesHashOfSums is FilesHash from each file's SHA-256 - what a node's
// FileList gives.
func FilesHashOfSums(sums map[string]string) string {
	names := make([]string, 0, len(sums))
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s %s\n", n, sums[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Sum is b's SHA-256, in hex.
func Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// templateMember is a member as a template sees it.
type templateMember struct {
	Name    string
	Index   int // from 1, in the cluster's order
	Address string
	Vars    map[string]string
}

// Render is version v as member name takes it.
func Render(v *Version, c *Cluster, name string) (*Rendered, error) {
	var self *templateMember
	var peers []templateMember
	for i, m := range c.Members {
		tm := templateMember{Name: m.Name, Index: i + 1, Address: m.Address, Vars: cloneVars(m.Vars)}
		if tm.Vars == nil {
			tm.Vars = map[string]string{}
		}
		if m.Name == name {
			self = &tm
		} else {
			peers = append(peers, tm)
		}
	}
	if self == nil {
		return nil, fmt.Errorf("%s isn't a member of cluster %s", name, c.Name)
	}
	data := map[string]any{
		"Cluster": map[string]string{"ID": c.ID, "Name": c.Name},
		"Member":  self,
		"Peers":   peers,
	}
	r := &Rendered{Files: map[string][]byte{}, Modules: map[string][]byte{}}
	for p, b := range v.Content {
		switch {
		case p == "haproxy.cfg":
			r.HAProxy = b
		case strings.HasPrefix(p, "files/"):
			r.Files[strings.TrimPrefix(p, "files/")] = b
		default:
			domain, ok := templates[p]
			if !ok {
				continue
			}
			t, err := template.New(p).Option("missingkey=error").Parse(string(b))
			if err != nil {
				return nil, fmt.Errorf("%s: %v", p, err)
			}
			var out bytes.Buffer
			if err := t.Execute(&out, data); err != nil {
				return nil, fmt.Errorf("%s for %s: %v", p, name, err)
			}
			r.Modules[domain] = out.Bytes()
		}
	}
	return r, nil
}
