package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// cdxComponent is a CycloneDX 1.6 component.
type cdxComponent struct {
	Type       string        `json:"type"`
	Name       string        `json:"name"`
	Version    string        `json:"version,omitempty"`
	PURL       string        `json:"purl,omitempty"`
	Hashes     []cdxHash     `json:"hashes,omitempty"`
	ExtRefs    []cdxRef      `json:"externalReferences,omitempty"`
	Properties []cdxProperty `json:"properties,omitempty"`
}

type cdxHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type cdxRef struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type cdxProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// sbom describes what a release is made of: every upstream versions.mk
// pins, the base images the build starts from, the Go modules linked into
// Janus's programs and the npm packages in the Controller's pages. It is
// reproducible: the same tree and version give the same bytes.
func sbom(version string, when time.Time) ([]byte, error) {
	mk, err := os.ReadFile("versions.mk")
	if err != nil {
		return nil, err
	}
	vars := parseVersionsMk(mk)
	var comps []cdxComponent
	for _, c := range components {
		v := vars[c.versionVar]
		if v == "" || c.kind == kindTest {
			continue
		}
		cc := cdxComponent{Type: "library", Name: c.name, Version: v, PURL: "pkg:generic/" + c.name + "@" + v,
			Properties: []cdxProperty{{"janus:kind", string(c.kind)}, {"janus:title", c.title}}}
		switch c.kind {
		case kindFirmware:
			cc.Type = "firmware"
		case kindBuild:
			cc.Type = "application"
		}
		if c.name == "linux" {
			cc.Type = "operating-system"
		}
		for _, arch := range c.archs() {
			if c.url != nil {
				cc.ExtRefs = append(cc.ExtRefs, cdxRef{"distribution", c.url(v, arch)})
			}
			if sum := vars[c.sumVars[arch]]; sum != "" {
				cc.Hashes = append(cc.Hashes, cdxHash{"SHA-256", sum})
			}
		}
		comps = append(comps, cc)
	}
	images, err := baseImages()
	if err != nil {
		return nil, err
	}
	comps = append(comps, images...)
	mods, err := linkedModules()
	if err != nil {
		return nil, err
	}
	for k, targets := range mods {
		comps = append(comps, cdxComponent{Type: "library", Name: k[0], Version: k[1],
			PURL:       "pkg:golang/" + k[0] + "@" + k[1],
			Properties: []cdxProperty{{"janus:kind", strings.Join(targets, ",")}}})
	}
	if gomod, err := os.ReadFile("go.mod"); err == nil {
		if tc := goRequires(gomod)["toolchain"]; tc != "" {
			v := strings.TrimPrefix(tc, "go")
			comps = append(comps, cdxComponent{Type: "library", Name: "stdlib", Version: v,
				PURL:       "pkg:golang/stdlib@" + v,
				Properties: []cdxProperty{{"janus:kind", "node,client,controller"}, {"janus:title", "Go standard library"}}})
		}
	}
	lock, err := os.ReadFile("dashboard/frontend/package-lock.json")
	if err != nil {
		return nil, err
	}
	pkgs, err := npmPackages(lock)
	if err != nil {
		return nil, err
	}
	for _, name := range sortedKeys(pkgs) {
		comps = append(comps, cdxComponent{Type: "library", Name: name, Version: pkgs[name],
			PURL:       "pkg:npm/" + strings.Replace(name, "@", "%40", 1) + "@" + pkgs[name],
			Properties: []cdxProperty{{"janus:kind", "controller"}}})
	}
	slices.SortStableFunc(comps, func(a, b cdxComponent) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Version, b.Version)
	})
	comps = slices.CompactFunc(comps, func(a, b cdxComponent) bool { return a.Name == b.Name && a.Version == b.Version && a.PURL == b.PURL })

	sum := sha256.Sum256([]byte("janus " + version))
	doc := map[string]any{
		"bomFormat":    "CycloneDX",
		"specVersion":  "1.6",
		"serialNumber": fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16]),
		"version":      1,
		"metadata": map[string]any{
			"timestamp": when.UTC().Format(time.RFC3339),
			"component": cdxComponent{Type: "operating-system", Name: "janus", Version: version},
		},
		"components": comps,
	}
	return json.MarshalIndent(doc, "", "  ")
}

var fromRe = regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?([a-z0-9./-]+):([A-Za-z0-9._-]+)@(sha256:[0-9a-f]{64})`)

// baseImages lists every pinned base image the Dockerfiles start from.
func baseImages() ([]cdxComponent, error) {
	out, err := exec.Command("git", "ls-files", "*Dockerfile").Output()
	if err != nil {
		return nil, err
	}
	var comps []cdxComponent
	for _, path := range strings.Fields(string(out)) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, m := range fromRe.FindAllStringSubmatch(string(data), -1) {
			name := m[1]
			repo := name
			if !strings.Contains(repo, "/") {
				repo = "library/" + repo
			}
			comps = append(comps, cdxComponent{Type: "container", Name: name, Version: m[2],
				PURL:       "pkg:docker/" + repo + "@" + strings.Replace(m[3], ":", "%3A", 1) + "?tag=" + m[2],
				Properties: []cdxProperty{{"janus:kind", "build"}, {"janus:dockerfile", filepath.ToSlash(path)}}})
		}
	}
	return comps, nil
}
