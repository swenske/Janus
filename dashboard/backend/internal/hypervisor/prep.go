package hypervisor

import (
	"fmt"
	"strings"
	"unicode"
)

// PreparationScript is every step of a host's preparation in one
// script, for the hypervisor named name.
func PreparationScript(name string, steps []PrepStep) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n# Prepares this host for the Janus Controller's hypervisor %q\n", CommentSafe(name))
	b.WriteString("# (docs/hypervisors.md). Run it as root: sh <this file>. Running it\n# again is harmless.\nset -eu\n")
	for i, s := range steps {
		fmt.Fprintf(&b, "\n# --- %d. %s ---\n", i+1, s.Title)
		for _, line := range wrap(s.About, 70) {
			b.WriteString("# " + line + "\n")
		}
		b.WriteString(s.Script)
	}
	return b.String()
}

// CommentSafe keeps a free-form name on one printable line - for the
// comments of a script, never its commands.
func CommentSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64])
	}
	return s
}

func wrap(s string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
