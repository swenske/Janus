package sysctl

import (
	"bufio"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/swenske/Janus/internal/sockdiag"
)

// Probes look at the node's state for the checks that depend on it.
type Probes interface {
	// ListeningTCPPorts are the ports a TCP socket listens on.
	ListeningTCPPorts() ([]int64, error)
	// OpenFiles is how many files are open on the system.
	OpenFiles() (int64, error)
	// ConntrackEntries is how many connections are tracked.
	ConntrackEntries() (int64, error)
	// MemTotal is the memory, in bytes.
	MemTotal() (int64, error)
}

// ProcProbes reads them from /proc.
type ProcProbes struct {
	Proc string // "/proc"
}

// ListeningTCPPorts asks sock_diag for the listening sockets: the
// kernel filters them, however many connections the node holds - where
// /proc/net/tcp formats a line for each one.
func (p ProcProbes) ListeningTCPPorts() ([]int64, error) {
	var out []int64
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := sockdiag.TCP(family, sockdiag.States(sockdiag.Listen), func(s sockdiag.Socket) {
			out = append(out, int64(s.Src.Port()))
		}); err != nil {
			return nil, err
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func (p ProcProbes) OpenFiles() (int64, error) {
	data, err := os.ReadFile(p.Proc + "/sys/fs/file-nr")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0, os.ErrInvalid
	}
	return strconv.ParseInt(f[0], 10, 64)
}

func (p ProcProbes) ConntrackEntries() (int64, error) {
	data, err := os.ReadFile(p.Proc + "/sys/net/netfilter/nf_conntrack_count")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

func (p ProcProbes) MemTotal() (int64, error) {
	f, err := os.Open(p.Proc + "/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			return kb * 1024, err
		}
	}
	return 0, os.ErrNotExist
}
