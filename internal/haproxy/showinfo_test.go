package haproxy

import (
	"os"
	"testing"
)

// testdata/show-info.txt is real "show info" output from this project's
// own haproxy build, after 4 requests.
func TestParseShowInfo(t *testing.T) {
	data, err := os.ReadFile("testdata/show-info.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := parseShowInfo(string(data))
	want := Info{
		Version: "3.4.0-64a335366", UptimeSeconds: 1, CurrentConnections: 0, MaxConnections: 524259,
		CumulativeConnections: 4, CumulativeRequests: 4, ConnectionRate: 3, SessionRate: 3, IdlePercent: 100, Pid: 150096,
	}
	if *got != want {
		t.Errorf("parseShowInfo =\n%+v, want\n%+v", *got, want)
	}
}
