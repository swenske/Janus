package main

import (
	"bytes"
	"strings"
	"testing"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// TestPrintNeighbors: a line per entry, its state in words, "-" for a
// neighbour that never answered.
func TestPrintNeighbors(t *testing.T) {
	var out bytes.Buffer
	printNeighbors(&out, []*janusv1alpha1.Neighbor{
		{Interface: "eth0", Family: janusv1alpha1.NeighborFamily_NEIGHBOR_FAMILY_IPV4, Address: "10.0.2.2", LinkAddress: "52:55:0a:00:02:02",
			State: janusv1alpha1.NeighborState_NEIGHBOR_STATE_REACHABLE, ConfirmedMsAgo: 2340, UsedMsAgo: 120},
		{Interface: "eth0", Family: janusv1alpha1.NeighborFamily_NEIGHBOR_FAMILY_IPV4, Address: "10.0.2.10",
			State: janusv1alpha1.NeighborState_NEIGHBOR_STATE_FAILED, ConfirmedMsAgo: 61000, UsedMsAgo: 3000},
		{Interface: "eth0", Family: janusv1alpha1.NeighborFamily_NEIGHBOR_FAMILY_IPV6, Address: "fe80::2", LinkAddress: "52:56:00:00:00:02",
			State: janusv1alpha1.NeighborState_NEIGHBOR_STATE_STALE, Router: true, ConfirmedMsAgo: 300000, UsedMsAgo: 300000},
	})
	want := `INTERFACE  ADDRESS    MAC                STATE           CONFIRMED  USED
eth0       10.0.2.2   52:55:0a:00:02:02  reachable       2s ago     0s ago
eth0       10.0.2.10  -                  failed          1m1s ago   3s ago
eth0       fe80::2    52:56:00:00:00:02  stale (router)  5m0s ago   5m0s ago
`
	var lines []string
	for l := range strings.Lines(out.String()) {
		lines = append(lines, strings.TrimRight(l, " \n")+"\n")
	}
	if got := strings.Join(lines, ""); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	out.Reset()
	printNeighbors(&out, nil)
	if out.String() != "no neighbours\n" {
		t.Errorf("empty table: %q", out.String())
	}
}
