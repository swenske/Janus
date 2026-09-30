package kmsgwatch

import (
	"path/filepath"
	"testing"
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
