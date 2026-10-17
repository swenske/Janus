package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// TestPrintClients: a line per connection, the Controller's users under
// it, the ones a non-admin isn't told counted.
func TestPrintClients(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return now.Add(-d).UnixNano() }
	var out bytes.Buffer
	printClients(&out, &janusv1alpha1.ClientListResponse{Connections: []*janusv1alpha1.ClientConnection{
		{Id: 1, Controller: true, Name: "controller", Roles: []string{"janus:controller"}, RemoteAddress: "10.0.30.10:51234", Interface: "eth0",
			OpenedUnixNs: at(2 * time.Hour), Calls: 1234, OpenStreams: 1, BytesReceived: 2048, BytesSent: 3 << 20, LastCallUnixNs: at(time.Second),
			Users: []*janusv1alpha1.ClientUser{{Name: "carol", Roles: []string{"os:reader"}, Calls: 12, OpenStreams: 1, LastCallUnixNs: at(time.Second), Self: true}}, UserCount: 3},
		{Id: 4, Interface: "eth0", OpenedUnixNs: at(time.Minute), Calls: 2, LastCallUnixNs: at(-time.Second)},
	}}, now)
	want := `ID  CLIENT                   ROLES             FROM              VIA   OPENED      CALLS  STREAMS  IN      OUT     LAST CALL
1   controller (Controller)  janus:controller  10.0.30.10:51234  eth0  2h0m0s ago  1234   1        2.0KiB  3.0MiB  1s ago
      for carol (you)        os:reader                                             12     1                        1s ago
      for 2 more
4   -                        -                 -                 eth0  1m0s ago    2      0        0B      0B      now
The users a Controller acts for are named to an admin.
`
	var lines []string
	for l := range strings.Lines(out.String()) {
		lines = append(lines, strings.TrimRight(l, " \n")+"\n")
	}
	if got := strings.Join(lines, ""); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
