package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// securityDoc is a release's security.json: what it fixes since the
// previous release. The release workflow attaches it; the Controller
// reads it to tell which nodes miss security fixes.
type securityDoc struct {
	Version  string `json:"version"`
	Previous string `json:"previous"`
	// MaxSeverity of the fixes that apply to Janus; "none" without any.
	MaxSeverity string         `json:"max_severity"`
	Updates     []updateRecord `json:"updates"`
}

// updateRecord is one upstream component (or Go module, npm package)
// updated by the release.
type updateRecord struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	// Target is what runs it: "node", "controller" ("extension" and
	// "firmware" components are on nodes too).
	Target string `json:"target"`
	From   string `json:"from"`
	To     string `json:"to"`
	Fixes  []vuln `json:"fixes,omitempty"`
	// Skipped: security fixes that can't apply to Janus's build.
	Skipped       []vuln `json:"skipped,omitempty"`
	NotApplicable int    `json:"not_applicable,omitempty"`
	FixedBugs     int    `json:"fixed_bugs,omitempty"`
}

func targetOf(k kind) string {
	switch k {
	case kindController:
		return "controller"
	case kindBuild, kindTest:
		return ""
	}
	return "node"
}

// securityNotes compares two git refs (to "" = the working tree).
func securityNotes(e *env, from, to, version string) (*securityDoc, error) {
	read := func(ref, path string) ([]byte, error) {
		if ref == "" {
			return readFile(path)
		}
		return gitShow(ref, path)
	}
	oldMk, err := read(from, "versions.mk")
	if err != nil {
		return nil, err
	}
	newMk, err := read(to, "versions.mk")
	if err != nil {
		return nil, err
	}
	oldVars, newVars := parseVersionsMk(oldMk), parseVersionsMk(newMk)
	doc := &securityDoc{Version: version, Previous: from, MaxSeverity: "none"}
	e.ref = to

	for _, c := range components {
		a, b := oldVars[c.versionVar], newVars[c.versionVar]
		target := targetOf(c.kind)
		if a == b || a == "" || b == "" || target == "" {
			continue
		}
		u := updateRecord{Name: c.name, Title: c.title, Target: target, From: a, To: b}
		if _, ok := c.feed.(githubCommit); !ok {
			res := &bumpResult{}
			if err := fixedBetween(e, c, a, b, res); err != nil {
				return nil, fmt.Errorf("%s: %w", c.name, err)
			}
			u.Fixes, u.Skipped, u.NotApplicable, u.FixedBugs = res.Fixed, res.Skipped, res.NotApplicable, res.FixedBugs
		}
		doc.Updates = append(doc.Updates, u)
	}

	goUpdates, err := goModuleUpdates(e, read, from, to)
	if err != nil {
		return nil, err
	}
	doc.Updates = append(doc.Updates, goUpdates...)
	npmUpdates, err := npmUpdates(e, read, from, to)
	if err != nil {
		return nil, err
	}
	doc.Updates = append(doc.Updates, npmUpdates...)

	for _, u := range doc.Updates {
		for _, v := range u.Fixes {
			if doc.MaxSeverity == "none" {
				doc.MaxSeverity = "unknown"
			}
			doc.MaxSeverity = maxSeverity(doc.MaxSeverity, v.Severity)
		}
	}
	return doc, nil
}

// goPrograms are the Go programs Janus ships: what runs them, the module
// directory, the main packages.
var goPrograms = []struct {
	target, dir string
	pkgs        []string
}{
	{"node", ".", []string{"./cmd/janusd", "./cmd/janus-acme", "./rootfs/init", "./rootfs/shutdown"}},
	{"client", ".", []string{"./cmd/janusctl"}},
	{"controller", ".", []string{"./dashboard/backend", "./dashboard/updater"}},
	{"client", "terraform-provider-janus", []string{"."}},
}

// linkedModules maps each module (path and version) linked into the
// shipped programs to what runs it ("node", "client", "controller"),
// standard library left out.
func linkedModules() (map[[2]string][]string, error) {
	mods := map[[2]string][]string{}
	for _, p := range goPrograms {
		args := append([]string{"-C", p.dir, "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}"}, p.pkgs...)
		out, err := exec.Command("go", args...).Output()
		if err != nil {
			return nil, fmt.Errorf("go list -deps %s: %w", strings.Join(p.pkgs, " "), err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			k := [2]string{f[0], f[1]}
			if !slices.Contains(mods[k], p.target) {
				mods[k] = append(mods[k], p.target)
			}
		}
	}
	return mods, nil
}

// goModuleUpdates lists the Go modules compiled into Janus's programs whose
// version changed between the refs - and the Go toolchain, for the standard
// library - with what osv.dev knows the change fixed.
func goModuleUpdates(e *env, read func(ref, path string) ([]byte, error), from, to string) ([]updateRecord, error) {
	linked, err := linkedModules()
	if err != nil {
		return nil, err
	}
	var out []updateRecord
	seen := map[string]bool{}
	for _, dir := range []string{".", "terraform-provider-janus"} {
		path := "go.mod"
		if dir != "." {
			path = dir + "/go.mod"
		}
		oldMod, err := read(from, path)
		if err != nil {
			continue // a module the old release didn't have
		}
		newMod, err := read(to, path)
		if err != nil {
			return nil, err
		}
		oldReq, newReq := goRequires(oldMod), goRequires(newMod)
		for _, mod := range sortedKeys(newReq) {
			a, b := oldReq[mod], newReq[mod]
			targets := linked[[2]string{mod, b}]
			if a == "" || a == b || len(targets) == 0 || seen[mod+b] {
				continue
			}
			seen[mod+b] = true
			u, err := osvUpdate(e, "Go", mod, a, b)
			if err != nil {
				return nil, err
			}
			u.Title, u.Target = mod+" (Go module)", goTarget(targets)
			out = append(out, *u)
		}
		if a, b := oldReq["toolchain"], newReq["toolchain"]; a != "" && a != b && !seen["stdlib"+b] {
			seen["stdlib"+b] = true
			u, err := osvUpdate(e, "Go", "stdlib", strings.TrimPrefix(a, "go"), strings.TrimPrefix(b, "go"))
			if err != nil {
				return nil, err
			}
			u.Name, u.Title, u.Target = "go", "Go toolchain (standard library)", "node"
			out = append(out, *u)
		}
	}
	return out, nil
}

// goRequires reads a go.mod's required module versions, and its
// toolchain under "toolchain".
func goRequires(mod []byte) map[string]string {
	req := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(string(mod), "\n") {
		line, _, _ = strings.Cut(line, "//")
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case f[0] == "toolchain" && len(f) == 2:
			req["toolchain"] = f[1]
		case f[0] == "require" && len(f) == 2 && f[1] == "(":
			inBlock = true
		case f[0] == "require" && len(f) == 3:
			req[f[1]] = f[2]
		case inBlock && f[0] == ")":
			inBlock = false
		case inBlock && len(f) == 2:
			req[f[0]] = f[1]
		}
	}
	return req
}

// goTarget is where a module's fixes matter most: a node, else the
// Controller, else the clients (janusctl, the Terraform provider).
func goTarget(targets []string) string {
	for _, t := range []string{"node", "controller"} {
		if slices.Contains(targets, t) {
			return t
		}
	}
	return "client"
}

// osvUpdate is one package's update, with the osv.dev records that
// affected its old version and no longer affect its new one.
func osvUpdate(e *env, ecosystem, name, from, to string) (*updateRecord, error) {
	q := func(v string) ([]osvVuln, error) {
		return osvQuery(e, map[string]any{"package": map[string]string{"name": name, "ecosystem": ecosystem}, "version": v})
	}
	old, err := q(from)
	if err != nil {
		return nil, err
	}
	cur, err := q(to)
	if err != nil {
		return nil, err
	}
	still := map[string]bool{}
	for _, v := range cur {
		still[v.ID] = true
	}
	u := &updateRecord{Name: name, From: from, To: to}
	for _, v := range old {
		if !still[v.ID] {
			u.Fixes = append(u.Fixes, fromOSV(v))
		}
	}
	enrich(e, u.Fixes)
	sortVulns(u.Fixes)
	return u, nil
}

// npmUpdates lists the Controller frontend's npm packages (production
// ones: what its built pages contain) whose version changed.
func npmUpdates(e *env, read func(ref, path string) ([]byte, error), from, to string) ([]updateRecord, error) {
	const lock = "dashboard/frontend/package-lock.json"
	oldLock, err := read(from, lock)
	if err != nil {
		return nil, nil
	}
	newLock, err := read(to, lock)
	if err != nil {
		return nil, err
	}
	oldPkgs, err := npmPackages(oldLock)
	if err != nil {
		return nil, err
	}
	newPkgs, err := npmPackages(newLock)
	if err != nil {
		return nil, err
	}
	var out []updateRecord
	for _, name := range sortedKeys(newPkgs) {
		a, b := oldPkgs[name], newPkgs[name]
		if a == "" || a == b {
			continue
		}
		u, err := osvUpdate(e, "npm", name, a, b)
		if err != nil {
			return nil, err
		}
		u.Title, u.Target = name+" (Controller frontend)", "controller"
		out = append(out, *u)
	}
	return out, nil
}

// npmPackages reads a package-lock.json (v2/v3) for its non-dev packages.
func npmPackages(lock []byte) (map[string]string, error) {
	var doc struct {
		Packages map[string]struct {
			Version string `json:"version"`
			Dev     bool   `json:"dev"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lock, &doc); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for path, p := range doc.Packages {
		i := strings.LastIndex(path, "node_modules/")
		if i < 0 || p.Dev || p.Version == "" {
			continue
		}
		out[path[i+len("node_modules/"):]] = p.Version
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
