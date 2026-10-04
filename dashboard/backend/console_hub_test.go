package main

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
)

// lockedBuffer is a bytes.Buffer the console's goroutine writes into
// while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestConsoleFirstReaderGetsEverything: the reader that opens a console
// gets it from its first byte - the stream used to start before that
// reader was registered, and what it printed first went to nobody.
func TestConsoleFirstReaderGetsEverything(t *testing.T) {
	for i := range 2000 {
		h := &consoleHub{}
		var out lockedBuffer
		sub := h.subscribe("m", func(_ context.Context, w io.Writer) error {
			_, err := io.WriteString(w, "boot\nok\n")
			return err
		}, &out)
		if err := <-sub.done; err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != "boot\nok\n" {
			t.Fatalf("run %d: the first reader got %q", i, got)
		}
	}
}
