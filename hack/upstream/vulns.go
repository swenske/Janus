package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// vuln is one known problem in a component's version: a CVE, an advisory,
// or a bug its upstream rates (HAProxy's).
type vuln struct {
	ID       string   `json:"id"`
	Aliases  []string `json:"aliases,omitempty"`
	Title    string   `json:"title"`
	Severity string   `json:"severity"` // critical, high, medium, low or unknown
	URL      string   `json:"url,omitempty"`
	// Exploited: in CISA's Known Exploited Vulnerabilities catalog.
	Exploited bool `json:"exploited,omitempty"`
	// NotApplicable says why it can't affect Janus ("built without QUIC").
	NotApplicable string `json:"not_applicable,omitempty"`
	// Bug: a plain bug, not a security issue (HAProxy's MEDIUM and MINOR).
	Bug bool `json:"bug,omitempty"`
}

var severities = []string{"unknown", "low", "medium", "high", "critical"}

func sevRank(s string) int { return max(slices.Index(severities, s), 0) }

func maxSeverity(a, b string) string {
	if sevRank(b) > sevRank(a) {
		return b
	}
	return a
}

// A vulnSource lists the vulnerabilities affecting a version.
type vulnSource interface {
	describe() string
	affecting(e *env, c *component, v string) ([]vuln, error)
}

// env is what vulnerability sources share in one run.
type env struct {
	ctx context.Context
	f   fetcher
	// ref is the git ref whose kernel/built-files-*.txt the kernel's
	// CVEs are filtered with ("" for the working tree).
	ref string

	mu    sync.Mutex
	cna   *kernelCVEs
	kev   map[string]bool
	sevOf map[string]string
}

// osvVuln is the part of an OSV record the tool reads.
type osvVuln struct {
	ID               string   `json:"id"`
	Summary          string   `json:"summary"`
	Details          string   `json:"details"`
	Aliases          []string `json:"aliases"`
	DatabaseSpecific struct {
		Severity string `json:"severity"`
	} `json:"database_specific"`
}

// title is a one-line description of an OSV record.
func (o osvVuln) title() string {
	if o.Summary != "" {
		return o.Summary
	}
	line, _, _ := strings.Cut(strings.TrimSpace(o.Details), "\n")
	if len(line) > 160 {
		line = line[:157] + "..."
	}
	return line
}

// cveID is the record's CVE when it has one, else its own ID.
func (o osvVuln) cveID() (string, []string) {
	ids := append([]string{o.ID}, o.Aliases...)
	for _, id := range ids {
		if strings.HasPrefix(id, "CVE-") {
			return id, others(ids, id)
		}
	}
	return o.ID, o.Aliases
}

func others(ids []string, id string) []string {
	var out []string
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func osvQuery(e *env, query map[string]any) ([]osvVuln, error) {
	var all []osvVuln
	for {
		body, _ := json.Marshal(query)
		data, err := e.f.post(e.ctx, "https://api.osv.dev/v1/query", body)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Vulns         []osvVuln `json:"vulns"`
			NextPageToken string    `json:"next_page_token"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("osv.dev: %w", err)
		}
		all = append(all, resp.Vulns...)
		if resp.NextPageToken == "" {
			return all, nil
		}
		query["page_token"] = resp.NextPageToken
	}
}

func fromOSV(o osvVuln) vuln {
	id, aliases := o.cveID()
	return vuln{
		ID: id, Aliases: aliases, Title: o.title(),
		Severity: strings.ToLower(o.DatabaseSpecific.Severity),
		URL:      "https://osv.dev/vulnerability/" + o.ID,
	}
}

// osvGit asks osv.dev about an upstream's git tag - OSV maps CVEs onto
// commit ranges of its repository. relevant, when set, keeps only the
// records it matches; excluded, the records it doesn't. Either way, the
// others are marked not applicable, for the reason irrelevant gives.
type osvGit struct {
	repo       string
	tag        string // "v%s": the tag of a plain version
	relevant   *regexp.Regexp
	excluded   *regexp.Regexp
	irrelevant string
}

func (o osvGit) describe() string { return "osv.dev (" + o.repo + ")" }

func (o osvGit) affecting(e *env, _ *component, v string) ([]vuln, error) {
	recs, err := osvQuery(e, map[string]any{
		"package": map[string]string{"name": o.repo, "ecosystem": "GIT"},
		"version": fmt.Sprintf(o.tag, plain(v)),
	})
	if err != nil {
		return nil, err
	}
	var out []vuln
	for _, r := range recs {
		x := fromOSV(r)
		text := r.Summary + " " + r.Details
		if (o.relevant != nil && !o.relevant.MatchString(text)) || (o.excluded != nil && o.excluded.MatchString(text)) {
			x.NotApplicable = o.irrelevant
		}
		out = append(out, x)
	}
	return out, nil
}

// ghsaRepo reads the security advisories an upstream publishes on its own
// GitHub repository, for its package pkg.
type ghsaRepo struct{ repo, pkg string }

func (g ghsaRepo) describe() string { return "github.com/" + g.repo + " security advisories" }

func (g ghsaRepo) affecting(e *env, _ *component, v string) ([]vuln, error) {
	var advs []struct {
		GHSAID   string `json:"ghsa_id"`
		CVEID    string `json:"cve_id"`
		HTMLURL  string `json:"html_url"`
		Summary  string `json:"summary"`
		Severity string `json:"severity"`
		Vulns    []struct {
			Package struct {
				Name string `json:"name"`
			} `json:"package"`
			Range string `json:"vulnerable_version_range"`
		} `json:"vulnerabilities"`
	}
	url := "https://api.github.com/repos/" + g.repo + "/security-advisories?state=published&per_page=100"
	if err := getJSON(e.ctx, e.f, url, &advs); err != nil {
		return nil, err
	}
	ver := mustVersion(v)
	var out []vuln
	for _, a := range advs {
		for _, x := range a.Vulns {
			if x.Package.Name != g.pkg || !inRange(ver, x.Range) {
				continue
			}
			id, aliases := a.GHSAID, []string(nil)
			if a.CVEID != "" {
				id, aliases = a.CVEID, []string{a.GHSAID}
			}
			out = append(out, vuln{ID: id, Aliases: aliases, Title: a.Summary, Severity: a.Severity, URL: a.HTMLURL})
			break
		}
	}
	return out, nil
}

// inRange reports whether v is in an advisory's range: comma-separated
// constraints (">= 1.61.0, < 1.71.0"), or a single version. A constraint
// naming no version (another product's) never matches.
func inRange(v version, expr string) bool {
	for _, c := range strings.Split(expr, ",") {
		c = strings.TrimSpace(c)
		op := ""
		for _, o := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(c, o) {
				op, c = o, strings.TrimSpace(c[len(o):])
				break
			}
		}
		w, ok := parseVersion(c)
		if !ok {
			return false
		}
		cmp := v.compare(w)
		switch op {
		case ">=":
			ok = cmp >= 0
		case "<=":
			ok = cmp <= 0
		case ">":
			ok = cmp > 0
		case "<":
			ok = cmp < 0
		default:
			ok = cmp == 0
		}
		if !ok {
			return false
		}
	}
	return expr != ""
}

// govulnBinary runs govulncheck on the upstream's own binary - its Go
// modules and the standard library it was built with. member names the
// binary inside a .zip or .tar.gz artifact (nil: the artifact is it).
type govulnBinary struct {
	member func(v, arch string) string
}

func (govulnBinary) describe() string { return "govulncheck on the release binary" }

func (g govulnBinary) affecting(e *env, c *component, v string) ([]vuln, error) {
	arch := c.archs()[0]
	path, err := e.f.download(e.ctx, c.url(v, arch))
	if err != nil {
		return nil, err
	}
	if g.member != nil {
		if path, err = extract(path, g.member(v, arch)); err != nil {
			return nil, err
		}
		defer os.Remove(path)
	}
	// Without a symbol table, govulncheck can't tell what the binary
	// reaches and reports every vulnerability of every module in it.
	if stripped, err := elfStripped(path); err != nil {
		return nil, err
	} else if stripped {
		return nil, fmt.Errorf("%s is stripped: govulncheck can't tell what it reaches", filepath.Base(c.url(v, arch)))
	}
	return govulncheck(e, "-mode=binary", path)
}

func elfStripped(path string) (bool, error) {
	f, err := elf.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return f.Section(".symtab") == nil, nil
}

// govulnSource runs govulncheck on a Go module's source at a version, with
// this tree's Go: what a component built from source (node_exporter)
// reaches, the standard library included.
type govulnSource struct{ module string }

func (g govulnSource) describe() string { return "govulncheck on its source, with this tree's Go" }

func (g govulnSource) affecting(e *env, _ *component, v string) ([]vuln, error) {
	dl, err := goModDownload(e.ctx, g.module, "v"+plain(v))
	if err != nil {
		return nil, err
	}
	return govulncheck(e, "-C", dl.Dir, "./...")
}

// goModule is what go mod download says of a module version.
type goModule struct {
	Zip, Dir, Sum, Error string
}

// goModDownload downloads a module version through the Go proxy, checked
// against Go's checksum database, into the module cache.
func goModDownload(ctx context.Context, module, version string) (*goModule, error) {
	cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", module+"@"+version)
	cmd.Dir = os.TempDir() // outside any module
	out, err := cmd.Output()
	var m goModule
	if jerr := json.Unmarshal(out, &m); jerr != nil {
		return nil, fmt.Errorf("go mod download %s@%s: %v", module, version, err)
	}
	if m.Error != "" {
		return nil, fmt.Errorf("go mod download %s@%s: %s", module, version, m.Error)
	}
	return &m, err
}

// goModuleZip is a Go module version's zip on proxy.golang.org.
func goModuleZip(module string) func(v, arch string) string {
	return func(v, _ string) string {
		return "https://proxy.golang.org/" + strings.ToLower(module) + "/@v/v" + plain(v) + ".zip"
	}
}

// goSumDB checks a module zip downloaded from the proxy is the one go mod
// download fetches and checks against Go's checksum database.
type goSumDB struct{ module string }

func (goSumDB) describe() string { return "Go's checksum database (go mod download)" }

func (g goSumDB) verify(ctx context.Context, _ fetcher, a *artifact) error {
	dl, err := goModDownload(ctx, g.module, "v"+plain(a.version))
	if err != nil {
		return err
	}
	sum, err := fileSHA256(dl.Zip)
	if err != nil {
		return err
	}
	if sum != a.sha256 {
		return fmt.Errorf("sha256 %s, the module zip go mod download checked is %s", a.sha256, sum)
	}
	return nil
}

// govulncheck runs the root module's govulncheck tool and returns what it
// finds reachable (a symbol or package used, not just a module required).
func govulncheck(e *env, args ...string) ([]vuln, error) {
	cmd := exec.CommandContext(e.ctx, "go", append([]string{"tool", "govulncheck", "-format", "json"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("govulncheck %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	recs := map[string]osvVuln{}
	found := map[string]bool{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var msg struct {
			OSV     *osvVuln `json:"osv"`
			Finding *struct {
				OSV   string `json:"osv"`
				Trace []struct {
					Package  string `json:"package"`
					Function string `json:"function"`
				} `json:"trace"`
			} `json:"finding"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("govulncheck output: %w", err)
		}
		if msg.OSV != nil {
			recs[msg.OSV.ID] = *msg.OSV
		}
		if f := msg.Finding; f != nil && len(f.Trace) > 0 && (f.Trace[0].Package != "" || f.Trace[0].Function != "") {
			found[f.OSV] = true
		}
	}
	var res []vuln
	for id := range found {
		x := fromOSV(recs[id])
		x.URL = "https://pkg.go.dev/vuln/" + id
		res = append(res, x)
	}
	slices.SortFunc(res, func(a, b vuln) int { return strings.Compare(a.ID, b.ID) })
	return res, nil
}

// extract copies one file out of a .zip, .tar.gz or .tar into a temporary
// file.
func extract(archive, member string) (string, error) {
	out, err := os.CreateTemp("", "upstream-bin-")
	if err != nil {
		return "", err
	}
	defer out.Close()
	fail := func(err error) (string, error) {
		os.Remove(out.Name())
		return "", fmt.Errorf("%s: %w", filepath.Base(archive), err)
	}
	if strings.HasSuffix(archive, ".zip") {
		z, err := zip.OpenReader(archive)
		if err != nil {
			return fail(err)
		}
		defer z.Close()
		for _, f := range z.File {
			if f.Name == member {
				r, err := f.Open()
				if err != nil {
					return fail(err)
				}
				defer r.Close()
				if _, err := io.Copy(out, r); err != nil {
					return fail(err)
				}
				return out.Name(), nil
			}
		}
		return fail(fmt.Errorf("no %s", member))
	}
	file, err := os.Open(archive)
	if err != nil {
		return fail(err)
	}
	defer file.Close()
	var r io.Reader = file
	if !strings.HasSuffix(archive, ".tar") {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return fail(err)
		}
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fail(fmt.Errorf("no %s", member))
		} else if err != nil {
			return fail(err)
		}
		if h.Name == member {
			if _, err := io.Copy(out, tr); err != nil {
				return fail(err)
			}
			return out.Name(), nil
		}
	}
}

// enrich fills in what sources left out: the severity of a CVE or GitHub
// advisory, and whether CISA knows it to be exploited (critical, then).
func enrich(e *env, vs []vuln) {
	kev := e.knownExploited()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range vs {
		x := &vs[i]
		for _, id := range append([]string{x.ID}, x.Aliases...) {
			if kev[id] {
				x.Exploited = true
			}
		}
		if x.Severity != "" && x.Severity != "unknown" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			x.Severity = "unknown"
			for _, id := range append([]string{x.ID}, x.Aliases...) {
				if s := e.severity(id); s != "" {
					x.Severity = s
					break
				}
			}
		}()
	}
	wg.Wait()
	for i := range vs {
		if vs[i].Exploited {
			vs[i].Severity = "critical"
		}
	}
}

// knownExploited is CISA's Known Exploited Vulnerabilities catalog (empty
// when it can't be read: it only ever raises a severity).
func (e *env) knownExploited() map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kev != nil {
		return e.kev
	}
	e.kev = map[string]bool{}
	var doc struct {
		Vulnerabilities []struct {
			CVEID string `json:"cveID"`
		} `json:"vulnerabilities"`
	}
	if err := getJSON(e.ctx, e.f, "https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json", &doc); err == nil {
		for _, v := range doc.Vulnerabilities {
			e.kev[v.CVEID] = true
		}
	}
	return e.kev
}

// severity rates an ID: a GitHub advisory by GitHub's rating, a CVE by its
// CVE record's CVSS (its CNA's, else an ADP's such as CISA's).
func (e *env) severity(id string) string {
	e.mu.Lock()
	s, ok := e.sevOf[id]
	e.mu.Unlock()
	if ok {
		return s
	}
	switch {
	case strings.HasPrefix(id, "GHSA-"):
		var adv struct {
			Severity string `json:"severity"`
		}
		if getJSON(e.ctx, e.f, "https://api.github.com/advisories/"+id, &adv) == nil && adv.Severity != "unknown" {
			s = adv.Severity
		}
	case strings.HasPrefix(id, "CVE-"):
		s = cveSeverity(e, id)
	}
	e.mu.Lock()
	if e.sevOf == nil {
		e.sevOf = map[string]string{}
	}
	e.sevOf[id] = s
	e.mu.Unlock()
	return s
}

func cveSeverity(e *env, id string) string {
	var year, num int
	if n, _ := fmt.Sscanf(id, "CVE-%d-%d", &year, &num); n != 2 {
		return ""
	}
	url := fmt.Sprintf("https://raw.githubusercontent.com/CVEProject/cvelistV5/main/cves/%d/%dxxx/%s.json", year, num/1000, id)
	var rec struct {
		Containers struct {
			CNA cveContainer   `json:"cna"`
			ADP []cveContainer `json:"adp"`
		} `json:"containers"`
	}
	if getJSON(e.ctx, e.f, url, &rec) != nil {
		return ""
	}
	if s := rec.Containers.CNA.severity(); s != "" {
		return s
	}
	for _, a := range rec.Containers.ADP {
		if s := a.severity(); s != "" {
			return s
		}
	}
	return ""
}

type cveContainer struct {
	Metrics []map[string]json.RawMessage `json:"metrics"`
}

// severity is the container's CVSS rating, the newest CVSS version first.
func (c cveContainer) severity() string {
	for _, key := range []string{"cvssV4_0", "cvssV3_1", "cvssV3_0"} {
		for _, m := range c.Metrics {
			raw, ok := m[key]
			if !ok {
				continue
			}
			var cvss struct {
				BaseSeverity string `json:"baseSeverity"`
			}
			if json.Unmarshal(raw, &cvss) == nil && cvss.BaseSeverity != "" {
				s := strings.ToLower(cvss.BaseSeverity)
				if s == "none" {
					s = "low"
				}
				return s
			}
		}
	}
	return ""
}
