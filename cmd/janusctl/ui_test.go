package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// Off a terminal - what scripts read - nothing changes: no escapes, the
// progress lines as they always were.
func TestUIOffATerminal(t *testing.T) {
	var b bytes.Buffer
	if got := progressLine(&b, "writing-data", 0.42, true, "rootfs.squashfs"); got != "[writing-data 42%] rootfs.squashfs" {
		t.Errorf("progress: %q", got)
	}
	if got := progressLine(&b, "done", 0, false, ""); got != "[done] " {
		t.Errorf("progress without a percentage: %q", got)
	}
	tw := newTable(&b)
	fmt.Fprintln(tw, "SERVICE\tSTATE\tHEALTH")
	fmt.Fprintln(tw, "haproxy\trunning\thealthy")
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "SERVICE  STATE    HEALTH\nhaproxy  running  healthy\n" {
		t.Errorf("table: %q", got)
	}
	var help bytes.Buffer
	printHelp(&help, commands, nil)
	if strings.Contains(help.String(), "\x1b[") {
		t.Error("the help has escapes off a terminal")
	}
	for _, want := range []string{"system logs SERVICE", "haproxy map-set MAP KEY VALUE", "completion bash|zsh|fish"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("the help lacks %q", want)
		}
	}
}

func TestStyleOn(t *testing.T) {
	st := style{on: true}
	if got := st.bold("x"); got != "\x1b[1mx\x1b[0m" {
		t.Errorf("bold: %q", got)
	}
	if got := (style{}).red("x"); got != "x" {
		t.Errorf("off: %q", got)
	}
}
