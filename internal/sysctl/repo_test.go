package sysctl

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Tests that keep the rest of the repository in step with this package.

// TestHardeningScriptMatchesBaseline: hack/qemu-hardening-test.sh checks
// a real boot's console against its own list of the baseline's writes.
func TestHardeningScriptMatchesBaseline(t *testing.T) {
	data, err := os.ReadFile("../../hack/qemu-hardening-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s+\["(/proc/sys/[^"]+)"\]="([^"]*)"$`)
	script := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		script[m[1]] = m[2]
	}
	want := map[string]string{}
	for _, s := range Baseline() {
		want[keyPath("/proc/sys", s.Key)] = s.Value
	}
	for path, v := range want {
		if got, ok := script[path]; !ok || got != v {
			t.Errorf("hack/qemu-hardening-test.sh: %s=%q, Baseline writes %q", path, got, v)
		}
	}
	for path := range script {
		if _, ok := want[path]; !ok {
			t.Errorf("hack/qemu-hardening-test.sh expects %s, which Baseline doesn't write", path)
		}
	}
}

// TestSELinuxLabelsTheWhitelist: selinux/policy.conf gives exactly the
// Editable parameters' files the type janusd may write.
func TestSELinuxLabelsTheWhitelist(t *testing.T) {
	data, err := os.ReadFile("../../selinux/policy.conf")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^genfscon proc (\S+) system_u:object_r:sysctl_tunable_t$`)
	var labelled []string
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		labelled = append(labelled, m[1])
	}
	var want []string
	for _, p := range EditableParams() {
		want = append(want, p.Path("/sys"))
	}
	slices.Sort(labelled)
	slices.Sort(want)
	if !slices.Equal(labelled, want) {
		t.Errorf("selinux/policy.conf labels sysctl_tunable_t:\n%q\nthe whitelist is:\n%q", labelled, want)
	}
}

var updateDoc = flag.Bool("update", false, "rewrite docs/guide/kernel-tuning.md's generated parts")

// TestKernelTuningDoc keeps docs/guide/kernel-tuning.md's tables what
// Catalog and CISControls say.
func TestKernelTuningDoc(t *testing.T) {
	const file = "../../docs/guide/kernel-tuning.md"
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	for _, part := range []struct{ name, body string }{
		{"editable", docEditable()},
		{"readonly", docReadOnly()},
		{"cis", docCIS()},
	} {
		begin := "<!-- generated: " + part.name + " - go test ./internal/sysctl -run TestKernelTuningDoc -update -->\n"
		end := "<!-- end: " + part.name + " -->"
		i, j := strings.Index(doc, begin), strings.Index(doc, end)
		if i < 0 || j < i {
			t.Fatalf("%s has no %q part", file, part.name)
		}
		doc = doc[:i+len(begin)] + "\n" + part.body + "\n" + doc[j:]
	}
	if *updateDoc {
		if err := os.WriteFile(file, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if doc != string(data) {
		t.Fatalf("%s isn't what internal/sysctl says: go test ./internal/sysctl -run TestKernelTuningDoc -update", file)
	}
}

var appliesText = map[Applies]string{
	Immediately:    "at once",
	NewConnections: "new connections",
	HAProxyReload:  "HAProxy's next reload",
}

// cell escapes what would end a table cell.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// docValue shows a value in a table cell.
func docValue(v string) string {
	if v == "" {
		return "(none)"
	}
	return "`" + v + "`"
}

func docAllowed(p *Param) string {
	num := func(n int64) string {
		if p.Unit == "bytes" {
			switch {
			case n%mib == 0:
				return fmt.Sprintf("%d MiB", n/mib)
			case n%kib == 0:
				return fmt.Sprintf("%d KiB", n/kib)
			}
		}
		return fmt.Sprint(n)
	}
	switch p.Kind {
	case KindEnum:
		return joinInts(p.Allowed, ", ")
	case KindPorts:
		return fmt.Sprintf("up to %d ports or ranges, %d-%d", p.MaxItems, p.Bounds[0].Min, p.Bounds[0].Max)
	}
	names := map[Kind][]string{KindPair: {"low ", "high "}, KindTriple: {"min ", "default ", "max "}}[p.Kind]
	var parts []string
	for i, b := range p.Bounds {
		prefix := ""
		if names != nil {
			prefix = names[i]
		}
		parts = append(parts, fmt.Sprintf("%s%s-%s", prefix, num(b.Min), num(b.Max)))
	}
	s := strings.Join(parts, ", ")
	if p.Unit != "" && p.Unit != "bytes" {
		s += " " + p.Unit
	}
	return s
}

func docSources(ss []Source) string {
	var parts []string
	for _, s := range ss {
		parts = append(parts, fmt.Sprintf("[%s](%s)", s.Title, s.URL))
	}
	return strings.Join(parts, ", ")
}

func docEditable() string {
	var b strings.Builder
	b.WriteString("| Parameter | Janus's default | Allowed | Takes effect | HAProxy recommends |\n|---|---|---|---|---|\n")
	for _, p := range EditableParams() {
		def := docValue(p.Default)
		if p.Dynamic {
			def = "the kernel's: " + p.KernelDefault
		}
		hap := p.HAProxyValue
		if hap == "" {
			hap = "-"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", p.Name, cell(def), cell(docAllowed(p)), appliesText[p.Applies], cell(hap))
	}
	b.WriteString("\nWhat each one does:\n\n")
	for _, p := range EditableParams() {
		fmt.Fprintf(&b, "- **`%s`** - %s HAProxy: %s Risk: %s The kernel's default: %s. Sources: %s.\n", p.Name, p.Summary, p.Effect, p.Risk, p.KernelDefault, docSources(p.Sources))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func docReadOnly() string {
	var b strings.Builder
	b.WriteString("| Parameter | Why it doesn't change | Sources |\n|---|---|---|\n")
	for _, p := range Catalog {
		if p.Class == Editable {
			continue
		}
		why := p.Why
		if p.Class == Forbidden {
			why = "Forbidden. " + why
		}
		fmt.Fprintf(&b, "| `%s` | %s %s | %s |\n", p.Name, cell(p.Summary), cell(why), docSources(p.Sources))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func docCIS() string {
	var b strings.Builder
	b.WriteString("| Control | Level | Key | Janus writes | Compliant |\n|---|---|---|---|---|\n")
	for _, c := range CISControls {
		key := "`" + c.Key + "`"
		if c.Interfaces != "" {
			key += ", and on every interface"
		}
		fmt.Fprintf(&b, "| %s | %d | %s | `%s` | %s |\n", c.ID(), c.Level, key, c.Value, strings.Join(c.Want, " or "))
	}
	return strings.TrimSuffix(b.String(), "\n")
}
