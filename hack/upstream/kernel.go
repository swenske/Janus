package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// kernelCNA reads the kernel's own CVE records (the kernel.org CNA's
// vulns.git) and keeps those whose fixes touch a file Janus's kernel
// builds - kernel/built-files-<arch>.txt, from `make kernel-built-files`.
// Every kernel CVE comes from a fix in a stable release, so a new stable
// release fixes many; most touch drivers Janus doesn't build.
type kernelCNA struct{}

func (kernelCNA) describe() string {
	return "kernel.org CNA (vulns.git), filtered by the files Janus's kernel builds"
}

// kernelCVE is one CNA record, reduced to what deciding needs.
type kernelCVE struct {
	ID      string
	Title   string
	Files   []string
	Default string // defaultStatus: "affected" or "unaffected"
	Ranges  []cveRange
}

// cveRange is a CVE JSON 5 version entry of the record's semver block.
type cveRange struct {
	Version         string `json:"version"`
	LessThan        string `json:"lessThan"`
	LessThanOrEqual string `json:"lessThanOrEqual"`
	Status          string `json:"status"`
	VersionType     string `json:"versionType"`
}

type kernelCVEs struct {
	cves  []kernelCVE
	built map[string]bool // nil: no built-files list, every CVE applies
}

func (kernelCNA) affecting(e *env, c *component, v string) ([]vuln, error) {
	k, err := e.kernelCVEs(c.variant)
	if err != nil {
		return nil, err
	}
	ver := mustVersion(v)
	var out []vuln
	for _, c := range k.cves {
		if !c.affects(ver) {
			continue
		}
		x := vuln{ID: c.ID, Title: c.Title, URL: "https://www.cve.org/CVERecord?id=" + c.ID}
		if !k.applies(c) {
			x.NotApplicable = "the code its fix changes isn't built"
		}
		out = append(out, x)
	}
	return out, nil
}

// affects decides a version's status the way the CNA writes its records:
// "unaffected" ranges are the fixes (per stable branch, then mainline),
// "affected" ones where a backport introduced the bug; anything else takes
// the record's default.
func (c kernelCVE) affects(v version) bool {
	affected := false
	for _, r := range c.Ranges {
		if !r.matches(v) {
			continue
		}
		if r.Status == "unaffected" {
			return false
		}
		affected = true
	}
	return affected || c.Default == "affected"
}

func (r cveRange) matches(v version) bool {
	lo, ok := parseVersion(r.Version)
	if !ok {
		return false
	}
	switch {
	case r.LessThan != "":
		hi, ok := parseVersion(r.LessThan)
		return ok && v.compare(lo) >= 0 && v.compare(hi) < 0
	case r.LessThanOrEqual == "*":
		return v.compare(lo) >= 0
	case strings.HasSuffix(r.LessThanOrEqual, ".*"):
		// "6.18.*": lo and later, within the 6.18 branch.
		branch := strings.TrimSuffix(r.LessThanOrEqual, ".*")
		return v.compare(lo) >= 0 && v.branch(strings.Count(branch, ".")+1) == branch
	case r.LessThanOrEqual != "":
		hi, ok := parseVersion(r.LessThanOrEqual)
		return ok && v.compare(lo) >= 0 && v.compare(hi) <= 0
	}
	// A lone version: the one where the bug was introduced, not a range.
	return false
}

// applies: the fix changes a file the kernel builds. A fix's source files
// decide when it has some - the headers it changes with them (list.h,
// a subsystem's own) are read by far more than that code; a header-only
// fix applies when a build reads the header.
func (k *kernelCVEs) applies(c kernelCVE) bool {
	if k.built == nil {
		return true
	}
	var sources, headers []string
	for _, f := range c.Files {
		if strings.HasSuffix(f, ".h") {
			headers = append(headers, f)
		} else {
			sources = append(sources, f)
		}
	}
	if len(sources) == 0 {
		sources = headers
	}
	for _, f := range sources {
		if k.built[f] {
			return true
		}
	}
	return false
}

// kernelVulnsRepo is the CNA's repository, cloned shallow into the cache.
const kernelVulnsRepo = "https://git.kernel.org/pub/scm/linux/security/vulns.git"

// kernelCVEs is the CNA's records, with what a kernel track builds: the
// records are read once, each track's built files once.
func (e *env) kernelCVEs(track string) (*kernelCVEs, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if k := e.cna[track]; k != nil {
		return k, nil
	}
	if e.cnaCVEs == nil {
		dir := filepath.Join(cacheDir(), "kernel-vulns")
		if err := syncRepo(dir, kernelVulnsRepo); err != nil {
			return nil, err
		}
		cves, err := loadKernelCVEs(filepath.Join(dir, "cve", "published"))
		if err != nil {
			return nil, err
		}
		e.cnaCVEs = cves
	}
	built, err := builtFiles(e.ref, track)
	if err != nil {
		return nil, err
	}
	if e.cna == nil {
		e.cna = map[string]*kernelCVEs{}
	}
	e.cna[track] = &kernelCVEs{cves: e.cnaCVEs, built: built}
	return e.cna[track], nil
}

func cacheDir() string {
	if d := os.Getenv("UPSTREAM_CACHE"); d != "" {
		return d
	}
	return filepath.Join("build", "upstream-cache")
}

// syncRepo clones a repository's latest commit, or brings a clone made
// earlier to it.
func syncRepo(dir, url string) error {
	run := func(args ...string) error {
		var stderr bytes.Buffer
		cmd := exec.Command("git", args...)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		return run("clone", "--quiet", "--depth", "1", url, dir)
	}
	if err := run("-C", dir, "fetch", "--quiet", "--depth", "1", "origin"); err != nil {
		return err
	}
	return run("-C", dir, "reset", "--quiet", "--hard", "FETCH_HEAD")
}

// loadKernelCVEs reads every published record (cve/published/<year>/*.json).
func loadKernelCVEs(dir string) ([]kernelCVE, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*", "CVE-*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: no CVE records", dir)
	}
	out := make([]kernelCVE, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		c, err := parseKernelCVE(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if c.Ranges != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func parseKernelCVE(data []byte) (kernelCVE, error) {
	var rec struct {
		Metadata struct {
			ID string `json:"cveId"`
		} `json:"cveMetadata"`
		Containers struct {
			CNA struct {
				Title    string `json:"title"`
				Affected []struct {
					DefaultStatus string     `json:"defaultStatus"`
					ProgramFiles  []string   `json:"programFiles"`
					Versions      []cveRange `json:"versions"`
				} `json:"affected"`
			} `json:"cna"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return kernelCVE{}, err
	}
	c := kernelCVE{ID: rec.Metadata.ID, Title: rec.Containers.CNA.Title}
	for _, a := range rec.Containers.CNA.Affected {
		// Two blocks: git commit ranges, and the version ranges read here.
		semver := false
		for _, r := range a.Versions {
			if r.VersionType != "git" {
				semver = true
			}
		}
		if !semver {
			continue
		}
		c.Files, c.Default = a.ProgramFiles, a.DefaultStatus
		for _, r := range a.Versions {
			if r.VersionType != "git" {
				c.Ranges = append(c.Ranges, r)
			}
		}
		break
	}
	if c.ID == "" {
		return kernelCVE{}, fmt.Errorf("no CVE ID")
	}
	// No version ranges (no Ranges): nothing to decide a version with.
	return c, nil
}

// builtFiles is the union of a kernel track's
// kernel/built-files-<track>-<arch>.txt at ref ("" for the working tree):
// every source file and header the track's kernel builds read, as paths
// in its tree. A ref from before kernel tracks has
// kernel/built-files-<arch>.txt, its only kernel's; a ref older than any
// list gets the working tree's; nil when there is none at all.
func builtFiles(ref, track string) (map[string]bool, error) {
	built, err := builtFilesAt(ref, track)
	if built == nil && err == nil && track != "" {
		built, err = builtFilesAt(ref, "")
	}
	if built == nil && err == nil && ref != "" {
		return builtFiles("", track)
	}
	return built, err
}

func builtFilesAt(ref, track string) (map[string]bool, error) {
	var built map[string]bool
	for _, arch := range []string{"amd64", "arm64"} {
		path := "kernel/built-files-" + arch + ".txt"
		if track != "" {
			path = "kernel/built-files-" + track + "-" + arch + ".txt"
		}
		var data []byte
		var err error
		if ref == "" {
			data, err = os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
		} else if data, err = gitShow(ref, path); err != nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		if built == nil {
			built = map[string]bool{}
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				built[line] = true
			}
		}
	}
	return built, nil
}
