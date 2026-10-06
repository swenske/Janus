package sysctl

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
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

func (p ProcProbes) ListeningTCPPorts() ([]int64, error) {
	seen := map[int64]bool{}
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(p.Proc + "/net/" + name)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, port := range listeningPorts(string(data)) {
			seen[port] = true
		}
	}
	out := make([]int64, 0, len(seen))
	for port := range seen {
		out = append(out, port)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// listeningPorts reads /proc/net/tcp's sockets in state 0A (LISTEN): a
// header line, then "sl local_address rem_address st ...", the local
// address ending in ":<port in hex>".
func listeningPorts(content string) []int64 {
	var out []int64
	lines := strings.Split(content, "\n")
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		i := strings.LastIndexByte(f[1], ':')
		if i < 0 {
			continue
		}
		port, err := strconv.ParseInt(f[1][i+1:], 16, 64)
		if err == nil {
			out = append(out, port)
		}
	}
	return out
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
