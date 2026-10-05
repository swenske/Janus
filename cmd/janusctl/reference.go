package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// writeReference writes the janusctl reference page,
// docs/guide/janusctl-reference.md, from the command tree: every command,
// its arguments and its flags - what janusctl itself dispatches,
// completes and prints its help from, so the page can't drift
// (TestReferenceDoc).
func writeReference(out io.Writer) {
	var b strings.Builder
	defer func() { _, _ = io.WriteString(out, strings.TrimRight(b.String(), "\n")+"\n") }()
	w := io.Writer(&b)
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	p("<!-- Generated from cmd/janusctl/commands.go: go test ./cmd/janusctl -run TestReferenceDoc -update -->\n\n")
	p("# janusctl reference\n\n")
	p("Every janusctl command, its arguments and its flags - generated from the\n")
	p("command tree janusctl itself runs, completes and prints its help from.\n")
	p("How to install it, sign in and reach nodes: [janusctl](../janusctl.md).\n\n")
	p("## Global flags\n\n")
	p("They come before the command:\n\n")
	p("```text\njanusctl [-context NAME] [-n NODE[,NODE] | -all] COMMAND ...\n")
	p("janusctl -endpoint HOST:PORT -ca FILE -cert FILE -key FILE COMMAND ...\n```\n\n")
	writeFlags(w, globalFlags)
	group := ""
	for _, c := range commands.subs {
		if c.hidden {
			continue
		}
		if c.group != "" && c.group != group {
			group = c.group
			p("## %s\n\n", strings.TrimLeftFunc(group, func(r rune) bool { return !unicode.IsLetter(r) }))
		}
		writeCommand(w, c, []string{c.name}, 3)
	}
}

// writeCommand writes a command - or a group, then its commands one
// heading level down (no deeper than #####).
func writeCommand(w io.Writer, c *command, path []string, level int) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	p("%s janusctl %s\n\n", strings.Repeat("#", level), strings.Join(path, " "))
	if c.help != "" {
		p("%s.\n\n", sentence(c.help))
	}
	if c.note != "" {
		p("%s\n\n", c.note)
	}
	if len(c.subs) > 0 {
		for _, s := range c.subs {
			if !s.hidden {
				writeCommand(w, s, append(append([]string{}, path...), s.name), min(level+1, 5))
			}
		}
		return
	}
	usage := "janusctl " + strings.Join(path, " ")
	for _, fl := range c.flags {
		usage += " " + flagUsage(fl)
	}
	if c.args != "" {
		usage += " " + c.args
	}
	p("```text\n%s\n```\n\n", usage)
	if c.offline {
		p("Runs on this machine: no node is contacted.\n\n")
	}
	writeFlags(w, c.flags)
}

func writeFlags(w io.Writer, flags []flagDef) {
	if len(flags) == 0 {
		return
	}
	fmt.Fprintf(w, "| Flag | |\n|---|---|\n")
	for _, fl := range flags {
		name := "-" + fl.name
		if fl.value != "" {
			name += " " + fl.value
		}
		fmt.Fprintf(w, "| `%s` | %s |\n", cell(name), cell(fl.help))
	}
	fmt.Fprintln(w)
}

// cell escapes a table cell's pipes - code spans included, in GitHub's
// tables.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// sentence capitalizes a help text's first letter.
func sentence(s string) string {
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
