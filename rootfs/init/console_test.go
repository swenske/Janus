package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// The serial console gets everything; a screen that can't keep up
// misses chunks instead of slowing the serial console down.
func TestRelayConsole(t *testing.T) {
	r, w := io.Pipe()
	var serial bytes.Buffer
	toScreen := make(chan []byte, 2) // nobody reads it: a stuck screen
	done := make(chan struct{})
	go func() {
		relayConsole(r, &serial, toScreen)
		close(done)
	}()
	for i := 0; i < 10; i++ {
		if _, err := w.Write([]byte("line\n")); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck screen blocked the relay")
	}
	if got := strings.Count(serial.String(), "line\n"); got != 10 {
		t.Errorf("serial got %d lines, want 10", got)
	}
	if len(toScreen) != 2 {
		t.Errorf("screen queue holds %d chunks, want it full (2) and the rest dropped", len(toScreen))
	}
}

// /dev/console follows the last console= of the kernel's command line:
// the mirror to the screen is for a serial one.
func TestLastConsoleIsSerial(t *testing.T) {
	for _, c := range []struct {
		cmdline string
		serial  bool
	}{
		{"console=tty0 console=ttyS0 panic=-1", true},
		{"console=tty0 console=ttyAMA0 panic=-1", true},
		{"console=ttyS0,115200n8 root=/dev/dm-0", true},
		{"console=tty0 panic=-1", false},
		{"console=ttyS0 console=tty0", false},
		{"root=/dev/dm-0 panic=-1", false},
		{"", false},
	} {
		if got := lastConsoleIsSerial(c.cmdline); got != c.serial {
			t.Errorf("%q: serial %v, want %v", c.cmdline, got, c.serial)
		}
	}
}
