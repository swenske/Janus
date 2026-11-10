package main

import "testing"

// TestSurvive: a step that panics is skipped - init goes on - and one
// that doesn't runs as usual.
func TestSurvive(t *testing.T) {
	ran := false
	survive("test", func() { ran = true })
	if !ran {
		t.Error("the step didn't run")
	}
	survive("test", func() { panic("a malformed volume") })
}
