package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// skillCommandsFile is the Claude Code skill's index of the commands
// (claude-code/janusctl), relative to this package.
const skillCommandsFile = "../../claude-code/janusctl/skills/janusctl/references/commands.md"

// writeSkillCommands writes the skill's index of the commands from the
// command tree and effects: one line per command - its usage, what it
// does to the node, what it does, what to know first (TestSkillCommands).
func writeSkillCommands(out io.Writer) {
	var b strings.Builder
	defer func() { _, _ = io.WriteString(out, strings.TrimRight(b.String(), "\n")+"\n") }()
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("<!-- Generated from cmd/janusctl/commands.go and effects.go: go test ./cmd/janusctl -run TestSkillCommands -update -->\n\n")
	p("# janusctl commands\n\n")
	p("Every janusctl command, with what it does to the node: **reads** - nothing\n")
	p("changes; **changes** - the node, or files on this machine; **critical** -\n")
	p("reboots, interrupts or moves traffic, wipes, or touches the node's trust or\n")
	p("credentials. SKILL.md says when each may run. What a flag means:\n")
	p("`janusctl help COMMAND` - the installed janusctl's own words, which win\n")
	p("over this list when they differ.\n\n")
	p("Global flags come before the command:\n\n")
	p("```text\njanusctl [-context NAME] [-n NODE[,NODE] | -all] COMMAND ...\n")
	p("janusctl -endpoint HOST:PORT -ca FILE -cert FILE -key FILE COMMAND ...\n```\n")
	group := ""
	for _, top := range commands.subs {
		if top.hidden {
			continue
		}
		if top.group != "" && top.group != group {
			group = top.group
			p("\n## %s\n\n", strings.TrimLeftFunc(group, func(r rune) bool { return !unicode.IsLetter(r) }))
		}
		write := func(path []string, c *command) {
			e := effects[strings.Join(path, " ")]
			usage := "janusctl " + strings.Join(path, " ")
			for _, fl := range c.flags {
				usage += " " + flagUsage(fl)
			}
			if c.args != "" {
				usage += " " + c.args
			}
			mark := e.String()
			if e.effect != effectReads {
				mark = "**" + mark + "**"
			}
			line := fmt.Sprintf("- `%s` - %s - %s", usage, mark, c.help)
			if e.note != "" {
				line += ". " + sentence(e.note)
			}
			p("%s\n", line)
		}
		if len(top.subs) == 0 {
			write([]string{top.name}, top)
			continue
		}
		top.leaves([]string{top.name}, write)
	}
}

// askRules is the Claude Code settings docs/guide/claude-code.md shows:
// an ask rule for every critical command, so Claude Code asks before
// each one whatever was allowed before - in auto mode too. The * after
// janusctl stands for the global flags; a command taking arguments ends
// with " *", so system service's rule leaves system services alone
// (TestDocsShowTheAskRules).
func askRules() string {
	var rules []string
	commands.leaves(nil, func(path []string, c *command) {
		if effects[strings.Join(path, " ")].effect != effectCritical {
			return
		}
		end := "*"
		if c.required > 0 {
			end = " *"
		}
		rules = append(rules, fmt.Sprintf("      \"Bash(janusctl *%s%s)\"", strings.Join(path, " "), end))
	})
	return "{\n  \"permissions\": {\n    \"ask\": [\n" + strings.Join(rules, ",\n") + "\n    ]\n  }\n}\n"
}
