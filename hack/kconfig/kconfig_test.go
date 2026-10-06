package kconfig

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/schematic"
	"github.com/swenske/Janus/internal/variants"
)

// TestTracksAgree: every kernel track's configuration says what the
// default track's says, but for kernel/configs/parity-exceptions.txt.
func TestTracksAgree(t *testing.T) {
	set, err := variants.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		data, err := os.ReadFile("../../kernel/configs/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	exc, err := Exceptions(read("parity-exceptions.txt"))
	if err != nil {
		t.Fatal(err)
	}
	def := set.Default(schematic.ComponentKernel)
	defCfg := Parse(read("janus_" + def.Name + "_defconfig"))
	used := map[string]bool{}
	for _, k := range set.Kernel {
		if k.Name == def.Name {
			continue
		}
		cfg := Parse(read("janus_" + k.Name + "_defconfig"))
		older, newer, from, to := defCfg, cfg, def.Name, k.Name
		if compareVersions(k.Version, def.Version) < 0 {
			older, newer, from, to = cfg, defCfg, k.Name, def.Name
		}
		d := Compare(older, newer)
		report := func(what string, syms map[string]string) {
			var names []string
			for sym := range syms {
				if _, ok := exc[sym]; ok {
					used[sym] = true
					continue
				}
				names = append(names, sym)
			}
			sort.Strings(names)
			for _, sym := range names {
				t.Errorf("%s -> %s: %s %s (%s) - make the tracks agree, or say why in parity-exceptions.txt", from, to, sym, what, syms[sym])
			}
		}
		report("differs", d.Changed)
		report("is gone", d.Lost)
	}
	for sym := range exc {
		if !used[sym] {
			t.Errorf("parity-exceptions.txt lists %s, which no longer differs - remove it", sym)
		}
	}
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return len(pa) - len(pb)
}

func TestCompare(t *testing.T) {
	older := Parse([]byte("CONFIG_A=y\n# CONFIG_B is not set\nCONFIG_C=y\nCONFIG_GCC_VERSION=140200\nCONFIG_D=m\n"))
	newer := Parse([]byte("CONFIG_A=y\nCONFIG_B=y\nCONFIG_GCC_VERSION=150100\nCONFIG_E=y\n"))
	d := Compare(older, newer)
	if len(d.Changed) != 1 || d.Changed["CONFIG_B"] != "n -> y" {
		t.Errorf("changed: %v", d.Changed)
	}
	if len(d.Lost) != 2 || d.Lost["CONFIG_C"] != "y" || d.Lost["CONFIG_D"] != "m" {
		t.Errorf("lost: %v", d.Lost)
	}
	if _, err := Exceptions([]byte("CONFIG_X\n")); err == nil {
		t.Error("an exception without a reason was accepted")
	}
}
