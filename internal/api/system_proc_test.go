package api

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Fixtures below are real /proc content captured from a Linux 6.18 host.

func TestParseProcStat(t *testing.T) {
	got := parseProcStat("cpu  1 2 3\nctxt 22602863\nbtime 1790747957\nprocesses 98229\nprocs_running 2\n")
	if got.BootTimeUnix != 1790747957 || got.ContextSwitches != 22602863 || got.ProcessesCreated != 98229 {
		t.Errorf("parseProcStat = %+v", got)
	}
}

func TestParseNetDev(t *testing.T) {
	const content = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 72764052   13923    0    0    0     0          0         0 72764052   13923    0    0    0     0       0          0
  eth0: 324513820  290953    3    0    0     0          0       527 57507840  101985    7    0    0     0       0          0
`
	got := parseNetDev(content)
	if len(got) != 2 {
		t.Fatalf("parseNetDev = %d devices, want 2", len(got))
	}
	e := got[1]
	if e.Name != "eth0" || e.RxBytes != 324513820 || e.RxErrors != 3 || e.TxBytes != 57507840 || e.TxErrors != 7 {
		t.Errorf("eth0 = %+v", e)
	}
}

func TestParseProcNet(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0019 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 10667 1 000000004015f153 100 0 0 10 0
   1: 0F02000A:251C 0202000A:C350 01 00000000:00000000 00:00000000 00000000     0        0 10508 1 000000004928a395 100 0 0 10 0
`
	got, err := parseProcNet("tcp", tcp)
	if err != nil || len(got) != 2 {
		t.Fatalf("parseProcNet(tcp) = %v, %v", got, err)
	}
	if got[0].LocalAddress != "127.0.0.1:25" || got[0].RemoteAddress != "0.0.0.0:0" || got[0].State != "LISTEN" {
		t.Errorf("listener = %+v", got[0])
	}
	if got[1].LocalAddress != "10.0.2.15:9500" || got[1].RemoteAddress != "10.0.2.2:50000" || got[1].State != "ESTABLISHED" {
		t.Errorf("established = %+v", got[1])
	}

	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:0019 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 10668 1 00000000fe0d0178 100 0 0 10 0
   1: 0000000000000000FFFF00000F02000A:251C 0000000000000000FFFF00000202000A:C350 01 00000000:00000000 00:00000000 00000000     0        0 10669 1 00000000fe0d0178 100 0 0 10 0
`
	got, err = parseProcNet("tcp6", tcp6)
	if err != nil || len(got) != 2 {
		t.Fatalf("parseProcNet(tcp6) = %v, %v", got, err)
	}
	if got[0].LocalAddress != "[::1]:25" || got[0].RemoteAddress != "[::]:0" {
		t.Errorf("tcp6 listener = %+v", got[0])
	}
	if got[1].LocalAddress != "10.0.2.15:9500" {
		t.Errorf("IPv4-mapped tcp6 = %+v", got[1])
	}

	udp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
10873: 0100007F:0143 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 1094 2 00000000118e1ba0 0
`
	got, err = parseProcNet("udp", udp)
	if err != nil || len(got) != 1 || got[0].LocalAddress != "127.0.0.1:323" || got[0].State != "UNCONN" || got[0].Protocol != "udp" {
		t.Errorf("parseProcNet(udp) = %v, %v", got, err)
	}

	if _, err := parseProcNet("tcp", "header\n 0: nothex:0019 00000000:0000 0A\n"); err == nil {
		t.Error("malformed address parsed without error")
	}
}

func TestParseMounts(t *testing.T) {
	got := parseMounts("/dev/dm-0 / squashfs ro,relatime 0 0\ntmpfs /etc tmpfs rw,relatime 0 0\n/dev/vda6 /etc/.state ext4 rw,context=system_u:object_r:state_t 0 0\nnone /mnt/my\\040disk tmpfs rw 0 0\n")
	if len(got) != 4 {
		t.Fatalf("parseMounts = %d mounts", len(got))
	}
	if got[0].Filesystem != "/dev/dm-0 (squashfs)" || got[0].MountedOn != "/" || !got[0].ReadOnly {
		t.Errorf("root = %+v", got[0])
	}
	if got[2].ReadOnly || got[2].MountedOn != "/etc/.state" {
		t.Errorf("state = %+v", got[2])
	}
	if got[3].MountedOn != "/mnt/my disk" {
		t.Errorf("octal escape not decoded: %q", got[3].MountedOn)
	}
}

func TestParsePidStat(t *testing.T) {
	// A comm with a space and a ")" in it must not shift the fields.
	const stat = "97724 (my (weird) proc) S 1 97716 97716 0 -1 4194304 122 0 1 0 250 150 0 0 25 5 1 0 1000 5992448 460 18446744073709551615 0"
	p, ok := parsePidStat(stat, 30, 4096)
	if !ok {
		t.Fatal("parsePidStat failed")
	}
	if p.comm != "my (weird) proc" || p.rssBytes != 460*4096 {
		t.Errorf("parsePidStat = %+v", p)
	}
	// 4s of CPU (400 ticks) over 20s alive (started at tick 1000 = 10s, uptime 30s).
	if math.Abs(p.cpuPercent-20) > 1e-9 {
		t.Errorf("cpuPercent = %v, want 20", p.cpuPercent)
	}
	if _, ok := parsePidStat("garbage", 1, 4096); ok {
		t.Error("garbage parsed")
	}
}

func TestFormatKmsgRecord(t *testing.T) {
	got, ok := formatKmsgRecord([]byte("6,339,5140900,-;NET: Registered PF_INET protocol family\n SUBSYSTEM=net\n"))
	if !ok || got != "[    5.140900] NET: Registered PF_INET protocol family\n" {
		t.Errorf("formatKmsgRecord = %q, %v", got, ok)
	}
	if _, ok := formatKmsgRecord([]byte("no header here")); ok {
		t.Error("malformed record accepted")
	}
}

func TestWipeDirContents(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"lost+found/keep", "pki/ca.key", "pki/sub/deep", "haproxy/haproxy.cfg", "controller/registered"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "top-level-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wipeDirContents(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "lost+found" {
		t.Errorf("after wipe: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "lost+found/keep")); err != nil {
		t.Errorf("lost+found content removed: %v", err)
	}
}

func TestIsMountpoint(t *testing.T) {
	if ok, err := isMountpoint("/proc"); err != nil || !ok {
		t.Errorf("isMountpoint(/proc) = %v, %v", ok, err)
	}
	dir := t.TempDir()
	if ok, err := isMountpoint(dir); err != nil || ok {
		t.Errorf("isMountpoint(temp dir) = %v, %v", ok, err)
	}
	if ok, err := isMountpoint(filepath.Join(dir, "missing")); err != nil || ok {
		t.Errorf("isMountpoint(missing) = %v, %v", ok, err)
	}
}
