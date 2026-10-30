package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// The CPU model a machine gets is its hypervisor's setting
// (hypervisor.ProxmoxConfig.CPU, #372).

// cpuDrift is how a machine's CPU model differs from want, its
// hypervisor's setting, when it's one the Controller would change:
//   - none, kvm64 or qemu64 - what Proxmox VE gives a machine created
//     without a model, an older Controller's: no AES-NI, SSE4, AVX, BMI2
//     or ADX, whatever options it has;
//   - another model the setting may name (hypervisor.IsProxmoxCPUModel),
//     alone: the Controller's own, from an earlier setting.
//
// Anything else - a custom model, max, a model with options added on
// the host (flags, hidden...) - is someone's choice, left alone.
func cpuDrift(cfg map[string]any, want string) *hypervisor.Drift {
	have := option(cfg["cpu"], "cputype", "cputype")
	switch {
	case have == "" || have == "kvm64" || have == "qemu64":
		return &hypervisor.Drift{Setting: "cpu", Have: or(have, "kvm64 (Proxmox's default)"), Want: want}
	case have == want:
		return nil
	case hypervisor.IsProxmoxCPUModel(have) && cpuAlone(cfg["cpu"]):
		return &hypervisor.Drift{Setting: "cpu", Have: have, Want: want}
	}
	return nil
}

// cpuAlone reports whether a cpu option names a model and nothing else
// ("x86-64-v3", "cputype=x86-64-v3") - what the Controller writes.
func cpuAlone(raw any) bool {
	parts := strings.Split(fmt.Sprint(raw), ",")
	if len(parts) != 1 {
		return false
	}
	k, _, named := strings.Cut(strings.TrimSpace(parts[0]), "=")
	return !named || k == "cputype"
}

// levelFlags are what qemu-server's x86-64 levels ask of the host's CPU
// (CPUConfig.pm's builtin models, as /proc/cpuinfo names them): QEMU
// refuses to start a machine whose host lacks one (Proxmox adds enforce
// to every model but host).
var levelFlags = func() map[string][]string {
	v2 := []string{"aes", "popcnt", "pni", "sse4_1", "sse4_2", "ssse3"}
	v3 := append(slices.Clone(v2), "avx", "avx2", "bmi1", "bmi2", "f16c", "fma", "abm", "movbe", "xsave")
	v4 := append(slices.Clone(v3), "avx512f", "avx512bw", "avx512cd", "avx512dq", "avx512vl")
	return map[string][]string{"x86-64-v2-AES": v2, "x86-64-v3": v3, "x86-64-v4": v4}
}()

// missingCPUFlags is what a host whose /proc/cpuinfo flags are flags
// lacks to run model - nothing for host, a named model (QEMU knows its
// flags, not the Controller), or a host that didn't say.
func missingCPUFlags(model, flags string) []string {
	have := strings.Fields(flags)
	if len(have) == 0 {
		return nil
	}
	var missing []string
	for _, f := range levelFlags[model] {
		if !slices.Contains(have, f) {
			missing = append(missing, f)
		}
	}
	return missing
}

func cpuFlagsError(model, node string, missing []string) string {
	return fmt.Sprintf("CPU model %s needs %s, which %s's processor hasn't got: its machines wouldn't start - choose an older model, or host", model, strings.Join(missing, " "), node)
}

// checkCPUModel refuses a model the hypervisor's node can't run, before
// a machine is created or changed with it. A node that doesn't say
// (its status unreadable, no flags) isn't refused: QEMU has the last
// word.
func (d *Driver) checkCPUModel(ctx context.Context, model string) error {
	if len(levelFlags[model]) == 0 {
		return nil
	}
	var st struct {
		CPUInfo struct {
			Flags string `json:"flags"`
		} `json:"cpuinfo"`
	}
	err := d.api.do(ctx, http.MethodGet, d.nodePath("/status"), nil, &st)
	if missing := missingCPUFlags(model, st.CPUInfo.Flags); err == nil && len(missing) > 0 {
		return errors.New(cpuFlagsError(model, d.cfg.Node, missing))
	}
	return nil
}
