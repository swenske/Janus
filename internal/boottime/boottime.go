// Package boottime records how long a node takes to come up: the
// moments, on the kernel's uptime clock, at which each stage of a boot
// completed - the kernel handed over to init, init started janusd,
// HAProxy served, the API listened. Seconds since the kernel started:
// the firmware's own time (POST, the UEFI boot manager) comes before
// that clock and isn't counted.
//
// rootfs/init writes its two moments to File (on /run, so a reboot
// clears them) before starting janusd; the boot's first janusd adds
// its own and writes it back; a janusd restarted later finds it
// complete - its own start says nothing about the boot. init prints
// its moments on the console as well, for a boot that never gets as
// far as janusd.
package boottime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// File is where init and the first janusd of a boot keep the times.
const File = "/run/janus/boot-times.json"

// Times are the seconds after the kernel started at which each stage
// completed.
type Times struct {
	// Kernel is when init started: the kernel's own boot, drivers and
	// ip=dhcp included. (Not PID 1's start time in /proc: the kernel
	// creates that task long before it runs init.)
	Kernel float64 `json:"kernel"`
	// Init is when init started janusd: its mounts, SELinux policy and
	// sysctls.
	Init float64 `json:"init"`
	// HAProxy is when HAProxy served - before the API, by design. 0
	// until the boot's first janusd writes it.
	HAProxy float64 `json:"haproxy,omitempty"`
	// API is when janusd listened. 0 until then.
	API float64 `json:"api,omitempty"`
}

// Stage is one of a boot's stages, for the exporter.
type Stage struct {
	Name    string
	Seconds float64
}

// Stages lists the stages in boot order, named after what completed.
func (t Times) Stages() []Stage {
	return []Stage{{"kernel", t.Kernel}, {"init", t.Init}, {"haproxy", t.HAProxy}, {"api", t.API}}
}

// String is the console line: the whole, then each stage.
func (t Times) String() string {
	return fmt.Sprintf("api listening %.1fs after the kernel started (kernel %.1fs, init %.1fs, haproxy %.1fs)", t.API, t.Kernel, t.Init, t.HAProxy)
}

// Uptime is the kernel's uptime, in seconds.
func Uptime() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	return parseUptime(string(data))
}

func parseUptime(s string) (float64, error) {
	fields := strings.Fields(s)
	if len(fields) < 1 {
		return 0, errors.New("/proc/uptime: empty")
	}
	up, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/uptime: %w", err)
	}
	return up, nil
}

// Load reads the times saved so far in this boot; nil when none were
// (janusd outside a node, or an init from before).
func Load(path string) (*Times, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Times
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}

// Save writes the times for what follows in this boot.
func (t Times) Save(path string) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
