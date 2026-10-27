package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Claude Code skill (claude-code/janusctl) tells an assistant what
// each command does to a node; these tests keep it what janusctl is.

// Every command says what it does to a node, and effects names only
// commands of the tree.
func TestEveryCommandHasAnEffect(t *testing.T) {
	seen := map[string]bool{}
	commands.leaves(nil, func(path []string, _ *command) {
		key := strings.Join(path, " ")
		seen[key] = true
		if effects[key].effect == effectUnset {
			t.Errorf("janusctl %s: no entry in effects (effects.go) - what does it do to the node?", key)
		}
	})
	for key := range effects {
		if !seen[key] {
			t.Errorf("effects.go: %q is no command of the tree", key)
		}
	}
}

// The skill's index of the commands is what the tree and effects say:
// go test ./cmd/janusctl -run TestSkillCommands -update after a change.
func TestSkillCommands(t *testing.T) {
	var b bytes.Buffer
	writeSkillCommands(&b)
	if *update {
		if err := os.WriteFile(skillCommandsFile, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(skillCommandsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, b.Bytes()) {
		t.Fatalf("%s isn't what commands.go and effects.go say: go test ./cmd/janusctl -run TestSkillCommands -update", skillCommandsFile)
	}
}

// The skill's page shows the ask rules of every critical command as
// askRules writes them.
func TestDocsShowTheAskRules(t *testing.T) {
	doc, err := os.ReadFile(skillPages[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "```json\n"+askRules()+"```\n") {
		t.Errorf("docs/guide/claude-code.md doesn't show the ask rules as effects.go makes them:\n%s", askRules())
	}
}

// skillPages are the Markdown files whose janusctl command lines are
// checked against the tree: the skill's own and its page in the docs.
var skillPages = []string{
	"../../claude-code/janusctl/skills/janusctl/SKILL.md",
	"../../docs/guide/claude-code.md",
}

var (
	codeSpan  = regexp.MustCompile("`([^`\n]+)`")
	fenceLine = regexp.MustCompile("^\\s*```")
	envAssign = regexp.MustCompile(`^[A-Z_]+=`)
)

// Every janusctl command the skill and its page show is one janusctl
// has, with flags it takes: an assistant runs what the skill shows.
func TestSkillShowsRealCommands(t *testing.T) {
	for _, page := range skillPages {
		data, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		inFence := false
		for i, line := range strings.Split(string(data), "\n") {
			if fenceLine.MatchString(line) {
				inFence = !inFence
				continue
			}
			snippets := []string{}
			if inFence {
				snippets = append(snippets, line)
			} else {
				for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
					snippets = append(snippets, m[1])
				}
			}
			for _, s := range snippets {
				if err := checkCommandLine(s); err != "" {
					t.Errorf("%s:%d: %s\n\t%s", filepath.Base(page), i+1, err, s)
				}
			}
		}
	}
}

// checkCommandLine checks each janusctl invocation in a shell line - janusctl
// where a command starts: first, after a shell operator, an environment
// assignment or timeout - its global flags, the command's path down the
// tree, the command's flags. Words in capitals and anything after a shell
// operator are left alone.
func checkCommandLine(line string) string {
	words := strings.Fields(line)
	start := true
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "|" || w == "||" || w == "&&" || w == ";" || w == "!" || w == "$(":
			start = true
			continue
		case start && w == "timeout":
			i++ // its duration
			continue
		case start && envAssign.MatchString(w):
			continue
		case w != "janusctl" || !start:
			start = strings.HasSuffix(w, ";")
			continue
		}
		start = false
		var rest []string
		for _, w := range words[i+1:] {
			if w == "\\" || strings.ContainsAny(w, "|&>#") {
				break
			}
			if cut, ok := strings.CutSuffix(w, ";"); ok {
				rest = append(rest, cut)
				break
			}
			rest = append(rest, w)
		}
		// Global flags.
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			name, _, inline := strings.Cut(strings.TrimLeft(rest[0], "-"), "=")
			var fl *flagDef
			for k := range globalFlags {
				if globalFlags[k].name == name {
					fl = &globalFlags[k]
				}
			}
			if fl == nil {
				return "janusctl has no global flag -" + name
			}
			rest = rest[1:]
			if fl.value != "" && !inline && len(rest) > 0 {
				rest = rest[1:]
			}
		}
		if len(rest) == 0 {
			continue
		}
		c, left := commands.find(rest)
		if c == commands {
			if isPlaceholder(rest[0]) {
				continue
			}
			return "janusctl has no command " + rest[0]
		}
		if len(c.subs) > 0 {
			if len(left) > 0 && !isPlaceholder(left[0]) && !strings.HasPrefix(left[0], "-") {
				return "janusctl " + c.name + " has no command " + left[0]
			}
			continue
		}
		for k := 0; k < len(left); k++ {
			w := left[k]
			if !strings.HasPrefix(w, "-") || w == "-" {
				continue
			}
			name, _, inline := strings.Cut(strings.TrimLeft(w, "-"), "=")
			fl := c.flag(name)
			if fl == nil {
				return "janusctl " + c.name + " has no flag -" + name
			}
			if fl.value != "" && !inline {
				k++
			}
		}
	}
	return ""
}

// The checker catches what a typo makes, and leaves alone what isn't a
// janusctl invocation.
func TestCheckCommandLine(t *testing.T) {
	for line, want := range map[string]string{
		"janusctl -n edge-1 system logs haproxy -n 200":             "",
		"timeout 5 janusctl -n edge-1 system events":                "",
		"JANUSCONFIG=x janusctl -context lab nodes":                 "",
		"janusctl -n edge-1 haproxy get-config > haproxy.cfg":       "",
		"janusctl -n a system info; echo ---; janusctl -n a nodes":  "",
		"janusctl help COMMAND":                                     "",
		"janusctl system":                                           "",
		"/plugin install janusctl --marketplace swenske/Janus":      "",
		"janusctl -n edge-1 lifecycle upgrade -wait-for-health URL": "",
		"janusctl -n edge-1 haproxy show-infos":                     "janusctl haproxy has no command show-infos",
		"janusctl -bogus version":                                   "janusctl has no global flag -bogus",
		"janusctl -n edge-1 system logs -z haproxy":                 "janusctl logs has no flag -z",
		"janusctl -n a nodes; janusctl -n a system reboto":          "janusctl system has no command reboto",
		"janusctl -n a system info; janusctl -n a haproxy apply -x": "janusctl haproxy has no command apply",
	} {
		if got := checkCommandLine(line); got != want {
			t.Errorf("checkCommandLine(%q) = %q, want %q", line, got, want)
		}
	}
}

// isPlaceholder is a word standing for a command: COMMAND, <command>,
// ...
func isPlaceholder(w string) bool {
	return w == strings.ToUpper(w) || strings.HasPrefix(w, "<") || strings.HasPrefix(w, "[")
}

// The plugin's manifests agree: the marketplace at the repository's
// root offers the plugin under its own name, with the brand's icon, and
// the skill has what Claude Code needs to list it.
func TestSkillPluginManifests(t *testing.T) {
	var market struct {
		Name    string
		Owner   struct{ Name string }
		Plugins []struct{ Name, Source, Description string }
	}
	var plugin struct{ Name, Description string }
	readJSON(t, "../../.claude-plugin/marketplace.json", &market)
	readJSON(t, "../../claude-code/janusctl/.claude-plugin/plugin.json", &plugin)
	if market.Name != "janus" || market.Owner.Name == "" || len(market.Plugins) != 1 {
		t.Fatalf("marketplace.json: want the marketplace janus, an owner, one plugin: %+v", market)
	}
	if p := market.Plugins[0]; p.Name != plugin.Name || p.Source != "./claude-code/janusctl" || p.Description == "" {
		t.Errorf("marketplace.json's plugin %+v isn't plugin.json's %q at ./claude-code/janusctl", p, plugin.Name)
	}
	// The icon Claude Code shows is the brand's tile: a copy, since a
	// plugin can't name a file outside its folder.
	icon, err := os.ReadFile("../../claude-code/janusctl/.claude-plugin/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if tile, err := os.ReadFile("../../brand/png/janus-tuile-1024.png"); err != nil || !bytes.Equal(icon, tile) {
		t.Errorf("claude-code/janusctl/.claude-plugin/icon.png isn't brand/png/janus-tuile-1024.png (%v): copy it again", err)
	}
	if plugin.Name != "janusctl" || plugin.Description == "" {
		t.Errorf("plugin.json: want the plugin janusctl, with a description: %+v", plugin)
	}

	skill, err := os.ReadFile(skillPages[0])
	if err != nil {
		t.Fatal(err)
	}
	front, body, ok := strings.Cut(strings.TrimPrefix(string(skill), "---\n"), "\n---\n")
	if !ok || !strings.HasPrefix(string(skill), "---\n") {
		t.Fatal("SKILL.md: no front matter")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(front, "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			fields[k] = v
		}
	}
	if fields["name"] != "janusctl" {
		t.Errorf("SKILL.md: name %q, want janusctl", fields["name"])
	}
	// Claude Code lists a skill by its description (1,536 characters at
	// most with when_to_use); the Agent Skills format caps it at 1,024.
	if d := fields["description"]; d == "" || len(d) > 1024 {
		t.Errorf("SKILL.md: description of %d characters, want 1 to 1,024", len(d))
	}
	if n := strings.Count(body, "\n"); n > 500 {
		t.Errorf("SKILL.md: %d lines - keep it under 500, the rest in references/", n)
	}
	for _, m := range regexp.MustCompile(`\]\(([^)#:]+)\)`).FindAllStringSubmatch(body, -1) {
		if _, err := os.Stat(filepath.Join(filepath.Dir(skillPages[0]), m[1])); err != nil {
			t.Errorf("SKILL.md links to %s: %v", m[1], err)
		}
	}
}

func readJSON(t *testing.T, file string, v any) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}
