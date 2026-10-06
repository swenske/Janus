// Package kconfig compares the kernel configurations of the kernel
// tracks an image can be built with (variants.mk): each is a full
// resolved .config of its own kernel version
// (kernel/configs/janus_<track>_defconfig), and nothing in olddefconfig
// keeps them saying the same thing - a symbol upstream renamed or
// dropped, a default that changed, silently give a track a kernel the
// other doesn't have. Its test (go test ./hack/kconfig) holds them
// together.
package kconfig

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

var (
	setRe   = regexp.MustCompile(`^(CONFIG_[A-Za-z0-9_]+)=(.*)$`)
	unsetRe = regexp.MustCompile(`^# (CONFIG_[A-Za-z0-9_]+) is not set$`)
)

// Parse reads a .config: every symbol it mentions, "n" for one not set.
func Parse(data []byte) map[string]string {
	syms := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := setRe.FindStringSubmatch(line); m != nil {
			syms[m[1]] = m[2]
		} else if m := unsetRe.FindStringSubmatch(line); m != nil {
			syms[m[1]] = "n"
		}
	}
	return syms
}

// toolchain lists what kbuild derives from the compiler, the assembler,
// the linker or the architecture, never chosen: no difference there says
// anything about the kernel a track gets.
var toolchain = regexp.MustCompile(`^CONFIG_(HAVE_|ARCH_HAS_|ARCH_SUPPORTS_|ARCH_WANT|ARCH_USE_|CC_HAS_|CC_CAN_|CC_IS_|CC_VERSION_TEXT|CC_MS_EXTENSIONS|GCC_|CLANG_|LD_|AS_|RUSTC_|BINDGEN_|PAHOLE_|TOOLS_SUPPORT_|OPENSSL_SUPPORTS_)`)

// Exceptions is kernel/configs/parity-exceptions.txt: a symbol per line
// ("CONFIG_X  why"), allowed to differ between the tracks.
func Exceptions(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sym, why, _ := strings.Cut(line, " ")
		if !strings.HasPrefix(sym, "CONFIG_") || strings.TrimSpace(why) == "" {
			return nil, fmt.Errorf("line %d: want \"CONFIG_X  why it differs\"", i+1)
		}
		out[sym] = strings.TrimSpace(why)
	}
	return out, nil
}

// Diff is how one track's configuration differs from another's.
type Diff struct {
	// Changed: symbols both have, with other values ("old -> new").
	Changed map[string]string
	// Lost: symbols the older track sets (y, m or a value) that the newer
	// one doesn't have at all - renamed or dropped upstream.
	Lost map[string]string
}

// Compare says how newer's configuration differs from older's, toolchain
// symbols left out.
func Compare(older, newer map[string]string) Diff {
	d := Diff{Changed: map[string]string{}, Lost: map[string]string{}}
	for sym, v := range older {
		if toolchain.MatchString(sym) {
			continue
		}
		w, ok := newer[sym]
		switch {
		case !ok && v != "n":
			d.Lost[sym] = v
		case ok && w != v:
			d.Changed[sym] = v + " -> " + w
		}
	}
	return d
}
