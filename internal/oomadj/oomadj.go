// Package oomadj orders the processes of a node for the kernel's OOM
// killer (/proc/<pid>/oom_score_adj, -1000 never .. 1000 first): janusd
// last - it starts HAProxy again after it dies -, then HAProxy, then the
// routing daemons (bird, keepalived: an extension's manifest asks), then
// everything else. v2026.10.14-5's lab: a configuration HAProxy couldn't
// fit in 1 GiB, and the kernel took janusd too.
//
// init lowers its own score to Janusd before it starts janusd: lowering
// takes CAP_SYS_RESOURCE, which init has and janusd hasn't, and janusd is
// born with it - no window. What janusd starts inherits Janusd, and janusd
// raises it at once to the process's own (raising takes no privilege).
// The kernel subtracts adj/1000 of the memory from a process's score: on
// 1 GiB, HAProxy at -500 goes before an extension only once it holds
// about 500 MiB more - a HAProxy that hogs the memory still goes first.
package oomadj

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The scores.
const (
	Janusd  = -900
	HAProxy = -500
	// Routing is bird's and keepalived's (their manifests): a routing
	// daemon killed moves the node's traffic away - kept over the other
	// extensions, behind HAProxy.
	Routing = -300
	// Default is everything else janusd starts: the other extensions,
	// the configuration checks (haproxy -c, nft -c...), janus-acme.
	Default = 0
	// MinExtension is the lowest an extension's manifest may ask for:
	// never at or below HAProxy.
	MinExtension = Routing
)

// Proc is /proc - a var for tests.
var Proc = "/proc"

var enabled atomic.Bool

// Enable makes Set write: janusd only with -manage-host - on a host or a
// CI runner, the processes it starts keep the host's scores.
func Enable() { enabled.Store(true) }

// Set writes pid's oom_score_adj - nothing unless Enable was called.
// Opened without O_CREATE: on an existing file it makes SELinux check
// "dir write" and log a denial although the open goes on.
func Set(pid, adj int) error {
	if !enabled.Load() {
		return nil
	}
	return write(fmt.Sprintf("%s/%d/oom_score_adj", Proc, pid), adj)
}

// SetSelf writes the calling process's own - init's, before it starts
// janusd. Not gated by Enable.
func SetSelf(adj int) error {
	return write(Proc+"/self/oom_score_adj", adj)
}

func write(path string, adj int) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.Itoa(adj))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Started sets the score of a process just started, logging a failure:
// the process runs either way.
func Started(cmd *exec.Cmd, adj int) {
	if err := Set(cmd.Process.Pid, adj); err != nil {
		log.Printf("oom_score_adj %d for %s (pid %d): %v", adj, cmd.Path, cmd.Process.Pid, err)
	}
}

// StartedTree is Started, then - a second later - the same score for the
// children the process started meanwhile: keepalived forks its VRRP
// process at once, which may have inherited janusd's score before
// janusd set its parent's.
func StartedTree(cmd *exec.Cmd, adj int) {
	Started(cmd, adj)
	if !enabled.Load() {
		return
	}
	parent := cmd.Process.Pid
	time.AfterFunc(time.Second, func() {
		for _, pid := range children(parent) {
			if err := Set(pid, adj); err != nil {
				log.Printf("oom_score_adj %d for pid %d (of %d): %v", adj, pid, parent, err)
			}
		}
	})
}

// children lists the processes whose parent is pid (/proc/<n>/stat).
func children(pid int) []int {
	entries, err := os.ReadDir(Proc)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", Proc, n))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: comm may hold spaces and parentheses.
		s := string(stat)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) > 1 && f[1] == strconv.Itoa(pid) {
			out = append(out, n)
		}
	}
	return out
}

// Run is cmd.Run, the process's score set to adj once it started.
func Run(cmd *exec.Cmd, adj int) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	Started(cmd, adj)
	return cmd.Wait()
}

// Clamp keeps an extension's score between MinExtension and 1000.
func Clamp(adj int) int {
	return min(max(adj, MinExtension), 1000)
}
