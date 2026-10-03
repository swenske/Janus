package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// assignRe matches a versions.mk assignment: "NAME := value", keeping the
// spacing around ":=" so a rewrite leaves the file's alignment alone.
var assignRe = regexp.MustCompile(`^([A-Za-z0-9_]+)(\s*):=(\s*)(\S*)\s*$`)

// parseVersionsMk returns versions.mk's variables.
func parseVersionsMk(data []byte) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := assignRe.FindStringSubmatch(line); m != nil {
			vars[m[1]] = m[4]
		}
	}
	return vars
}

// setVars rewrites the given variables' values in versions.mk, nothing
// else. Every variable must already be assigned exactly once.
func setVars(data []byte, vals map[string]string) ([]byte, error) {
	lines := strings.Split(string(data), "\n")
	seen := map[string]int{}
	for i, line := range lines {
		m := assignRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, ok := vals[m[1]]
		if !ok {
			continue
		}
		seen[m[1]]++
		lines[i] = m[1] + m[2] + ":=" + m[3] + v
	}
	for name := range vals {
		if seen[name] != 1 {
			return nil, fmt.Errorf("versions.mk assigns %s %d times, want once", name, seen[name])
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// gitShow reads a file at a git ref.
func gitShow(ref, path string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", "show", ref+":"+path)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %v: %s", ref, path, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
