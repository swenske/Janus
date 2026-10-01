package consoledrain

import (
	"os"
	"testing"
	"time"
)

// Wait returns once a slow reader has emptied the pipe, not before.
func TestWaitForAPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := w.Write([]byte("rebooting to complete the revert\n")); err != nil {
		t.Fatal(err)
	}
	read := make(chan time.Time, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		buf := make([]byte, 64)
		_, _ = r.Read(buf)
		read <- time.Now()
	}()
	Wait(w, 5*time.Second)
	done := time.Now()
	if readAt := <-read; done.Before(readAt) {
		t.Errorf("Wait returned %v before the pipe was read", readAt.Sub(done))
	}
}

// Not a pipe: nothing to wait for.
func TestWaitForAFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	Wait(f, 5*time.Second)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("Wait on a file took %v", d)
	}
}
