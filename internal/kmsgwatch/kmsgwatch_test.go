package kmsgwatch

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		msg         string
		denial, oom bool
	}{
		// Real lines from Janus boots and the kernel's OOM killer.
		{`audit: type=1400 audit(1759272000.123:5): avc:  denied  { read } for  pid=1 comm="init" name="pnp" scontext=system_u:system_r:init_t tcontext=system_u:object_r:proc_t tclass=file permissive=0`, true, false},
		{`Out of memory: Killed process 1234 (haproxy) total-vm:123456kB, anon-rss:1000kB`, false, true},
		{`Memory cgroup out of memory: Killed process 99 (x) total-vm:1kB`, false, true},
		{`oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null)`, false, false},
		{`SELinux:  Initializing.`, false, false},
	} {
		d, o := Classify(tc.msg)
		if d != tc.denial || o != tc.oom {
			t.Errorf("Classify(%q) = %v %v, want %v %v", tc.msg, d, o, tc.denial, tc.oom)
		}
	}
}

func TestWatchMissing(t *testing.T) {
	var c Counts
	Watch(filepath.Join(t.TempDir(), "nope"), &c) // returns at once
	if c.Running.Load() {
		t.Fatal("running without a kernel log")
	}
}

func TestOOMVictim(t *testing.T) {
	for msg, want := range map[string]struct {
		pid     int
		process string
	}{
		"Out of memory: Killed process 1234 (haproxy) total-vm:123456kB, anon-rss:1000kB": {1234, "haproxy"},
		"Memory cgroup out of memory: Killed process 99 (x) total-vm:1kB":                 {99, "x"},
		"Out of memory: Killed process":                                                   {0, ""},
		"something else":                                                                  {0, ""},
	} {
		pid, process := OOMVictim(msg)
		if pid != want.pid || process != want.process {
			t.Errorf("OOMVictim(%q) = %d %q, want %d %q", msg, pid, process, want.pid, want.process)
		}
	}
}

// TestWatchOOMKills reads a kernel log of records as /dev/kmsg gives
// them - one per read - from a pipe: the OOM kill a previous janusd told
// (ToldFile) isn't told again, the next one is, and remembered.
func TestWatchOOMKills(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kmsg")
	if err := os.Symlink("/proc/self/fd/"+itoa(int(r.Fd())), path); err != nil {
		t.Fatal(err)
	}
	toldFile := filepath.Join(t.TempDir(), "told")
	if err := os.WriteFile(toldFile, []byte("100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	told := make(chan string, 4)
	c := &Counts{ToldFile: toldFile, OnOOMKill: func(pid int, process string) {
		told <- process + "/" + itoa(pid)
	}}
	go Watch(path, c)
	// Each record written once the previous one was read: /dev/kmsg
	// gives one per read, a pipe whatever is there.
	waitFor := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not read", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	write := func(rec string) {
		t.Helper()
		if _, err := w.WriteString(rec); err != nil {
			t.Fatal(err)
		}
	}
	write("3,100,5000000,-;Out of memory: Killed process 10 (old) total-vm:1kB\n")
	waitFor("the backlog's OOM kill", func() bool { return c.OOMKills.Load() == 1 })
	write("5,101,9000000,-;audit: type=1400 audit(1.2:3): avc:  denied  { read } for pid=1\n")
	waitFor("the denial", func() bool { return c.SELinuxDenials.Load() == 1 })
	write("3,102,12000000,-;Out of memory: Killed process 20 (haproxy) total-vm:1kB\n SUBSYSTEM=x\n")
	select {
	case got := <-told:
		if got != "haproxy/20" {
			t.Fatalf("told %q, want haproxy/20 (the kill told before isn't told again)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the new OOM kill wasn't told")
	}
	if n := c.OOMKills.Load(); n != 2 {
		t.Errorf("OOMKills = %d, want 2 (both counted)", n)
	}
	waitFor("the told file", func() bool {
		data, _ := os.ReadFile(toldFile)
		return string(data) == "102\n"
	})
	select {
	case got := <-told:
		t.Errorf("also told %q", got)
	default:
	}
	w.Close()
}

func itoa(i int) string { return strconv.Itoa(i) }
