package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFuzzyMatch(t *testing.T) {
	for _, tc := range []struct {
		query, text string
		ok          bool
	}{
		{"", "anything", true},
		{"slg", "system logs", true},
		{"SYS", "system logs", true},
		{"logs", "system logs", true},
		{"sgl", "system logs", false},
		{"haproxy", "hap", false},
	} {
		if _, _, ok := fuzzyMatch(tc.query, tc.text); ok != tc.ok {
			t.Errorf("fuzzyMatch(%q, %q) = %v", tc.query, tc.text, ok)
		}
	}
	// A run and a word's start beat letters scattered.
	run, _, _ := fuzzyMatch("log", "system logs")
	scattered, _, _ := fuzzyMatch("log", "slow goat")
	if run <= scattered {
		t.Errorf("a run scores %d, scattered letters %d", run, scattered)
	}
}

func TestPickerKeys(t *testing.T) {
	items := []candidate{{"system logs", ""}, {"system ls", ""}, {"haproxy backends", ""}, {"network status", ""}}
	p := newPicker("Which command?", items, false)
	for _, k := range []string{"h", "a", "p"} {
		p.key(k)
	}
	if len(p.matches) != 1 || p.matches[0].c.value != "haproxy backends" {
		t.Fatalf("typing hap: %v", p.matches)
	}
	p.key("\x7f")
	p.key("\x7f")
	p.key("\x7f")
	if len(p.matches) != 4 {
		t.Fatalf("after backspaces: %d matches", len(p.matches))
	}
	p.key("\x1b[B")
	p.key("\x1b[B")
	if done, _ := p.key("\r"); !done || p.choice()[0] != "haproxy backends" {
		t.Errorf("down, down, Enter: %v", p.choice())
	}
	p = newPicker("x", items, false)
	p.key("\x1b[A") // up from the top wraps to the bottom
	if p.choice()[0] != "network status" {
		t.Errorf("up from the top: %v", p.choice())
	}
	if _, cancelled := p.key("\x1b"); !cancelled {
		t.Error("Esc doesn't cancel")
	}
	p.key("z")
	p.key("z")
	if done, _ := p.key("\r"); done {
		t.Error("Enter with nothing matching picked something")
	}

	// Several: Tab marks, Enter takes the marked ones, in the list's order.
	p = newPicker("Which node?", []candidate{{"lb1", ""}, {"lb2", ""}, {"lb3", ""}}, true)
	p.key("\x1b[B")
	p.key("\x1b[B")
	p.key("\t") // lb3, the last: the cursor stays
	p.key("\x1b[A")
	p.key("\x1b[A")
	p.key("\t")
	if done, _ := p.key("\r"); !done || strings.Join(p.choice(), ",") != "lb1,lb3" {
		t.Errorf("marked lb3 then lb1: %v", p.choice())
	}
}

func TestPickerRender(t *testing.T) {
	var items []candidate
	for _, n := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliett", "kilo", "lima"} {
		items = append(items, candidate{n, "a node with a rather long description that won't fit in a narrow terminal"})
	}
	p := newPicker("Which node?", items, false)
	lines := p.render(style{}, 40)
	if len(lines) != 1+maxRows+1 || !strings.HasPrefix(lines[1], "❯ alpha") || lines[len(lines)-1] != "  1/12" {
		t.Errorf("render:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range lines[1:] {
		if n := utf8.RuneCountInString(l); n > 40 {
			t.Errorf("a line of %d runes in 40 columns: %q", n, l)
		}
	}
	for range 11 {
		p.key("\x1b[B")
	}
	lines = p.render(style{}, 40)
	if !strings.HasPrefix(lines[maxRows], "❯ lima") {
		t.Errorf("the list doesn't follow the cursor:\n%s", strings.Join(lines, "\n"))
	}
}

func TestFillMissingNeedsATerminalList(t *testing.T) {
	// Without a node, nothing online is asked; a group isn't a command.
	c, _, positional := leafArgs([]string{"system", "logs", "-n", "5"})
	if c.name != "logs" || len(positional) != 0 || needed(c, positional) != 1 {
		t.Errorf("system logs -n 5: %s %v", c.name, positional)
	}
	c, _, positional = leafArgs([]string{"context", "use"})
	if needed(c, positional) != 2 {
		t.Errorf("context use needs a name")
	}
}
