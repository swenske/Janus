package libvirt

import (
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// TestAvailableOf: what a host can give is free memory plus its buffers
// and page cache - v2026.10.14's lab host had 173 MiB free, 6.1 GiB
// available.
func TestAvailableOf(t *testing.T) {
	got, ok := availableOf([]golibvirt.NodeGetMemoryStats{
		{Field: "total", Value: 16342600},
		{Field: "free", Value: 174568},
		{Field: "buffers", Value: 80912},
		{Field: "cached", Value: 6175416},
	})
	if want := uint64(174568+80912+6175416) * 1024; !ok || got != want {
		t.Errorf("availableOf = %d, %v; want %d", got, ok, want)
	}
	if _, ok := availableOf([]golibvirt.NodeGetMemoryStats{{Field: "total", Value: 1}}); ok {
		t.Error("no free field: still an answer")
	}
}
