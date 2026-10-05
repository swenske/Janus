// Package docscheck holds the docs' own tests that need no Node: run
// with go test ./... in ci.yml's test job.
package docscheck

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// A published link to a file of this repository with an anchor -
// github.com/swenske/Janus/blob/main/docs/x.md#heading - lives on outside
// it: in release notes, in the Controllers and nodes already deployed,
// in issues. Its file must stay, and so must the heading.
var publishedLink = regexp.MustCompile(`github\.com/swenske/Janus/blob/main/([A-Za-z0-9_./-]+\.md)#([A-Za-z0-9_-]+)`)

// The files read: every tracked text file, but the frontends' built
// copies of their sources.
func trackedFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", "../..", "ls-files").Output()
	if err != nil {
		t.Skipf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		switch {
		case strings.Contains(f, "/static/"), strings.HasSuffix(f, ".png"), strings.HasSuffix(f, ".ico"), strings.HasSuffix(f, ".webp"):
			continue
		}
		files = append(files, f)
	}
	return files
}

func TestPublishedAnchorsExist(t *testing.T) {
	anchors := map[string]map[string]bool{}
	for _, f := range trackedFiles(t) {
		data, err := os.ReadFile(filepath.Join("../..", f))
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		for _, m := range publishedLink.FindAllSubmatch(data, -1) {
			target, anchor := string(m[1]), string(m[2])
			if anchors[target] == nil {
				src, err := os.ReadFile(filepath.Join("../..", target))
				if err != nil {
					t.Errorf("%s links %s#%s: the file is gone - published links point at it, it never moves", f, target, anchor)
					anchors[target] = map[string]bool{}
					continue
				}
				anchors[target] = headingAnchors(src)
			}
			if !anchors[target][anchor] {
				t.Errorf("%s links %s#%s: no such heading any more - published links point at it: keep it (with a pointer where its content went)", f, target, anchor)
			}
		}
	}
}

// headingAnchors are the anchors GitHub gives a Markdown file's headings
// (and its explicit <a id/name>), code blocks aside.
func headingAnchors(md []byte) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	explicit := regexp.MustCompile(`<a (?:id|name)="([^"]+)"`)
	fence := ""
	sc := bufio.NewScanner(bytes.NewReader(md))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if f := fenceOf(trimmed); f != "" {
			if fence == "" {
				fence = f
			} else if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		for _, m := range explicit.FindAllStringSubmatch(line, -1) {
			out[m[1]] = true
		}
		level := len(line) - len(strings.TrimLeft(line, "#"))
		if level == 0 || level > 6 || !strings.HasPrefix(line[level:], " ") {
			continue
		}
		slug := githubSlug(headingText(strings.TrimSpace(line[level:])))
		if n := seen[slug]; n > 0 {
			out[fmt.Sprintf("%s-%d", slug, n)] = true
		} else {
			out[slug] = true
		}
		seen[slug]++
	}
	return out
}

func fenceOf(line string) string {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, f) {
			return f
		}
	}
	return ""
}

var (
	mdLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdMarks  = regexp.MustCompile("[`*]")
	htmlTags = regexp.MustCompile(`<[^>]+>`)
)

// headingText is a heading's text as GitHub renders it: links' text,
// no code or emphasis marks, no HTML tags.
func headingText(s string) string {
	s = strings.TrimRight(s, " #")
	s = mdLink.ReplaceAllString(s, "$1")
	s = htmlTags.ReplaceAllString(s, "")
	return mdMarks.ReplaceAllString(s, "")
}

// githubSlug is GitHub's anchor for a heading (github-slugger): lower
// case, letters, digits, marks, - and _ kept, spaces made -, the rest
// dropped.
func githubSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.M, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestGitHubSlug(t *testing.T) {
	for in, want := range map[string]string{
		"mTLS / PKI":                     "mtls--pki",
		"Janus's own vulnerabilities":    "januss-own-vulnerabilities",
		"TLS (AWS-LC)":                   "tls-aws-lc",
		"Method 4: the installer ISO":    "method-4-the-installer-iso",
		"`janus_haproxy_config`":         "janus_haproxy_config",
		"🔑 Sign in":                      "-sign-in",
		"The \"no shell\" API surface":   "the-no-shell-api-surface",
		"Updating the Controller":        "updating-the-controller",
		"[Links](x.md) and *emphasis*":   "links-and-emphasis",
		"Build system vs. target OS - x": "build-system-vs-target-os---x",
	} {
		if got := githubSlug(headingText(in)); got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", in, got, want)
		}
	}
	got := headingAnchors([]byte("# T\n## Prerequisites\n```sh\n## not a heading\n```\n## Prerequisites\n<a id=\"x\"></a>\n"))
	for _, want := range []string{"t", "prerequisites", "prerequisites-1", "x"} {
		if !got[want] {
			t.Errorf("headingAnchors: no %q in %v", want, got)
		}
	}
	if got["not-a-heading"] {
		t.Errorf("headingAnchors read a heading in a code block")
	}
}
