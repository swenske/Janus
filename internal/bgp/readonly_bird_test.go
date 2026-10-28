package bgp

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The exporter's restricted socket with the real BIRD - the bird
// extension's own binary, JANUS_BIRD_BIN (hack/bird-ro-socket-test.sh
// extracts it from the extension's tar): the bird.conf janusd writes
// gives BIRD the socket, show commands answer over it, every command
// that changes routing is refused there - and the same command over
// BIRD's own socket does change it, so it really is a write.
func TestReadOnlySocketWithBIRD(t *testing.T) {
	bin := os.Getenv("JANUS_BIRD_BIN")
	if bin == "" {
		t.Skip("JANUS_BIRD_BIN isn't set (hack/bird-ro-socket-test.sh)")
	}
	dir, err := os.MkdirTemp("", "bird-ro") // short: a socket path has 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	prevDir, prevRO := RunDir, ReadOnlySocket
	RunDir, ReadOnlySocket = dir, true
	t.Cleanup(func() { RunDir, ReadOnlySocket = prevDir, prevRO })

	conf := filepath.Join(dir, "bird.conf")
	text := runtimeConfig("router id 192.0.2.1;\nprotocol device {}\nprotocol static s1 { ipv4; route 192.0.2.10/32 blackhole; }\n")
	if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := exec.Command(bin, "-f", "-c", conf, "-s", SocketPath(), "-P", filepath.Join(dir, "bird.pid"))
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ReadOnlySocketPath()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s after 10 s - bird.conf:\n%s\nBIRD: %s", ReadOnlySocketPath(), text, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	up := func(sock string) bool {
		lines, err := queryAt(sock, "show protocols s1")
		if err != nil {
			t.Fatalf("show protocols over %s: %v", sock, err)
		}
		for _, l := range lines {
			if f := strings.Fields(l.Text); len(f) >= 4 && f[0] == "s1" {
				return f[3] == "up"
			}
		}
		t.Fatalf("no s1 in %v", lines)
		return false
	}
	if !up(ReadOnlySocketPath()) {
		t.Fatal("s1 isn't up (read over the restricted socket)")
	}
	t.Logf("bird.conf as BIRD reads it:\n%s", text)

	for _, c := range []string{"disable s1", "restart s1", "reload s1", "debug s1 all", "configure", "configure soft", "configure check", "down"} {
		_, err := queryAt(ReadOnlySocketPath(), c)
		var be *ErrBIRD
		if !errors.As(err, &be) || !strings.Contains(err.Error(), "Access denied") {
			t.Errorf("%q over the restricted socket: %v, want BIRD's \"Access denied\"", c, err)
			continue
		}
		t.Logf("%q over the restricted socket: %v (code %d)", c, err, be.Lines[len(be.Lines)-1].Code)
	}
	if !up(SocketPath()) {
		t.Fatal("s1 went down through the restricted socket")
	}
	// The same write over BIRD's own socket (janusd's) does take effect.
	if _, err := queryAt(SocketPath(), "disable s1"); err != nil {
		t.Fatalf("disable over BIRD's own socket: %v", err)
	}
	if up(ReadOnlySocketPath()) {
		t.Error("disable over BIRD's own socket didn't take")
	}
}
