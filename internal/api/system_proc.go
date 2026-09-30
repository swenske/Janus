package api

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// userHZ is the unit of /proc/<pid>/stat's CPU times - fixed at 100 by
// the kernel ABI on every architecture Janus builds for (x86_64, arm64).
const userHZ = 100

func (s *System) Hostname(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.HostnameResponse, error) {
	h, err := os.Hostname()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hostname: %v", err)
	}
	return &janusv1alpha1.HostnameResponse{Hostname: h}, nil
}

// --- SystemStat ---

func (s *System) SystemStat(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.SystemStatResponse, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/stat: %v", err)
	}
	return parseProcStat(string(data)), nil
}

func parseProcStat(content string) *janusv1alpha1.SystemStatResponse {
	resp := &janusv1alpha1.SystemStatResponse{}
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) >= 9 && f[0] == "cpu" {
			// user nice system idle iowait irq softirq steal [guest
			// guest_nice] - guest time is already counted in user.
			for i, v := range f[1:9] {
				n, _ := strconv.ParseUint(v, 10, 64)
				resp.CpuTotalTicks += n
				if i == 3 || i == 4 {
					resp.CpuIdleTicks += n
				}
			}
			continue
		}
		if len(f) != 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "btime":
			resp.BootTimeUnix = v
		case "ctxt":
			resp.ContextSwitches = v
		case "processes":
			resp.ProcessesCreated = v
		}
	}
	return resp
}

// --- NetworkDeviceStats ---

func (s *System) NetworkDeviceStats(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.NetworkDeviceStatsResponse, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/net/dev: %v", err)
	}
	return &janusv1alpha1.NetworkDeviceStatsResponse{Devices: parseNetDev(string(data))}, nil
}

// parseNetDev reads /proc/net/dev: two header lines, then
// "name: 8 receive counters 8 transmit counters".
func parseNetDev(content string) []*janusv1alpha1.NetworkDeviceStat {
	var out []*janusv1alpha1.NetworkDeviceStat
	for _, line := range strings.Split(content, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		n := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		out = append(out, &janusv1alpha1.NetworkDeviceStat{
			Name:     strings.TrimSpace(name),
			RxBytes:  n(0),
			RxErrors: n(2),
			TxBytes:  n(8),
			TxErrors: n(10),
		})
	}
	return out
}

// --- Netstat ---

func (s *System) Netstat(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.NetstatResponse, error) {
	resp := &janusv1alpha1.NetstatResponse{}
	for _, proto := range []string{"tcp", "tcp6", "udp", "udp6"} {
		data, err := os.ReadFile("/proc/net/" + proto)
		if err != nil {
			if os.IsNotExist(err) {
				continue // e.g. no IPv6 in this kernel
			}
			return nil, status.Errorf(codes.Internal, "read /proc/net/%s: %v", proto, err)
		}
		conns, err := parseProcNet(proto, string(data))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "parse /proc/net/%s: %v", proto, err)
		}
		resp.Connections = append(resp.Connections, conns...)
	}
	return resp, nil
}

var tcpStates = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1",
	"05": "FIN_WAIT2", "06": "TIME_WAIT", "07": "CLOSE", "08": "CLOSE_WAIT",
	"09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING", "0C": "NEW_SYN_RECV",
}

// parseProcNet reads /proc/net/{tcp,tcp6,udp,udp6}: a header line, then
// "sl local rem st ..." with hex addresses.
func parseProcNet(proto, content string) ([]*janusv1alpha1.Connection, error) {
	var out []*janusv1alpha1.Connection
	lines := strings.Split(content, "\n")
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		local, err := decodeProcNetAddr(f[1])
		if err != nil {
			return nil, err
		}
		remote, err := decodeProcNetAddr(f[2])
		if err != nil {
			return nil, err
		}
		state := tcpStates[f[3]]
		if strings.HasPrefix(proto, "udp") {
			// UDP only reuses two of the TCP codes: connected or not.
			state = "UNCONN"
			if f[3] == "01" {
				state = "ESTABLISHED"
			}
		}
		out = append(out, &janusv1alpha1.Connection{LocalAddress: local, RemoteAddress: remote, State: state, Protocol: proto})
	}
	return out, nil
}

// decodeProcNetAddr turns "0100007F:1F90" into "127.0.0.1:8080". The
// address is the kernel's in-memory form: each 32-bit word in host
// (little-endian on every Janus target) byte order.
func decodeProcNetAddr(s string) (string, error) {
	hexIP, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return "", fmt.Errorf("malformed address %q", s)
	}
	raw, err := hex.DecodeString(hexIP)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return "", fmt.Errorf("malformed address %q", s)
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return "", fmt.Errorf("malformed port in %q", s)
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(ip[i:], binary.LittleEndian.Uint32(raw[i:]))
	}
	if v4 := ip.To4(); v4 != nil && len(raw) == 16 && !ip.Equal(net.IPv6zero) {
		ip = v4
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), nil
}

// --- Mounts ---

func (s *System) Mounts(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.MountsResponse, error) {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read /proc/self/mounts: %v", err)
	}
	mounts := parseMounts(string(data))
	for _, m := range mounts {
		var st syscall.Statfs_t
		if err := syscall.Statfs(m.MountedOn, &st); err == nil {
			m.SizeBytes = st.Blocks * uint64(st.Bsize)
			m.AvailableBytes = st.Bavail * uint64(st.Bsize)
		}
	}
	return &janusv1alpha1.MountsResponse{Mounts: mounts}, nil
}

// parseMounts reads /proc/self/mounts ("source target fstype options 0 0",
// with spaces and other specials octal-escaped as \040).
func parseMounts(content string) []*janusv1alpha1.MountStat {
	var out []*janusv1alpha1.MountStat
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		readOnly := false
		for _, opt := range strings.Split(f[3], ",") {
			if opt == "ro" {
				readOnly = true
			}
		}
		out = append(out, &janusv1alpha1.MountStat{
			Filesystem: unescapeMountField(f[0]) + " (" + f[2] + ")",
			MountedOn:  unescapeMountField(f[1]),
			ReadOnly:   readOnly,
		})
	}
	return out
}

func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// --- Processes / Stats ---

type procInfo struct {
	pid        int32
	comm       string
	command    string
	cpuPercent float64
	cpuSeconds float64
	rssBytes   uint64
}

func (s *System) Processes(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ProcessesResponse, error) {
	procs, err := readProcesses(true)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read processes: %v", err)
	}
	resp := &janusv1alpha1.ProcessesResponse{}
	for _, p := range procs {
		resp.Processes = append(resp.Processes, &janusv1alpha1.ProcessInfo{
			Pid: p.pid, Command: p.command, CpuPercent: p.cpuPercent, MemoryBytes: p.rssBytes,
		})
	}
	return resp, nil
}

// Stats sums Processes per managed service - a reload briefly leaves an
// old haproxy process finishing its connections next to the new one.
func (s *System) Stats(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.StatsResponse, error) {
	procs, err := readProcesses(false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read processes: %v", err)
	}
	byID := map[string]*janusv1alpha1.ProcessStat{}
	for _, p := range procs {
		if p.comm != "janusd" && p.comm != "haproxy" {
			continue
		}
		st, ok := byID[p.comm]
		if !ok {
			st = &janusv1alpha1.ProcessStat{Id: p.comm}
			byID[p.comm] = st
		}
		st.CpuPercent += p.cpuPercent
		st.CpuSeconds += p.cpuSeconds
		st.MemoryBytes += p.rssBytes
	}
	resp := &janusv1alpha1.StatsResponse{}
	for _, id := range []string{"janusd", "haproxy"} {
		if st, ok := byID[id]; ok {
			resp.Processes = append(resp.Processes, st)
		}
	}
	return resp, nil
}

// readProcesses reads every process's /proc/<pid>/stat, and with
// withCommand its cmdline too. Stats - polled by the Controller's live
// charts as often as every second - only needs the process name, which
// stat already carries, so it skips the second read per process.
func readProcesses(withCommand bool) ([]procInfo, error) {
	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return nil, err
	}
	var uptime float64
	if f := strings.Fields(string(uptimeData)); len(f) > 0 {
		uptime, _ = strconv.ParseFloat(f[0], 64)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pageSize := uint64(os.Getpagesize())
	var out []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		statData, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue // exited meanwhile
		}
		p, ok := parsePidStat(string(statData), uptime, pageSize)
		if !ok {
			continue
		}
		p.pid = int32(pid)
		if withCommand {
			p.command = "[" + p.comm + "]" // kernel threads have no cmdline
			if cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil && len(cmdline) > 0 {
				p.command = strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pid < out[j].pid })
	return out, nil
}

// parsePidStat reads /proc/<pid>/stat. comm is parenthesized and may
// itself contain spaces or ")", so fields are counted from the last ")".
// CPU is the average over the process's lifetime, like ps(1)'s %CPU.
func parsePidStat(content string, uptime float64, pageSize uint64) (procInfo, bool) {
	open, closing := strings.IndexByte(content, '('), strings.LastIndexByte(content, ')')
	if open < 0 || closing < open {
		return procInfo{}, false
	}
	f := strings.Fields(content[closing+1:])
	if len(f) < 22 {
		return procInfo{}, false
	}
	n := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
	// f[0] is field 3 (state): utime=14, stime=15, starttime=22, rss=24.
	cpuSeconds := float64(n(11)+n(12)) / userHZ
	elapsed := uptime - float64(n(19))/userHZ
	var cpu float64
	if elapsed > 0 {
		cpu = 100 * cpuSeconds / elapsed
	}
	return procInfo{comm: content[open+1 : closing], cpuPercent: cpu, cpuSeconds: cpuSeconds, rssBytes: n(21) * pageSize}, true
}
