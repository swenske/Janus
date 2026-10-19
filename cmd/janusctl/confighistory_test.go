package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// TestPrintConfigHistory: a line per version, newest first, the running
// one and a revert named; the version found before the history has no
// time and no author.
func TestPrintConfigHistory(t *testing.T) {
	at := time.Date(2026, 10, 19, 14, 5, 0, 0, time.UTC).Unix()
	var out bytes.Buffer
	printConfigHistory(&out, &janusv1alpha1.ConfigHistoryResponse{Versions: []*janusv1alpha1.ConfigVersionInfo{
		{Id: 3, AppliedUnix: at, Author: &janusv1alpha1.ConfigAuthor{Name: "carol", Roles: []string{"os:operator"}, Via: "controller"}, Sha256: "aaaaaaaaaaaaaaaa", LinesAdded: 1, LinesRemoved: 2, RevertedFrom: 1, Running: true},
		{Id: 2, AppliedUnix: at - 60, Author: &janusv1alpha1.ConfigAuthor{Name: "admin", Roles: []string{"os:admin"}}, Sha256: "bbbbbbbbbbbbbbbb", LinesAdded: 2, LinesRemoved: 1},
		{Id: 1, Author: &janusv1alpha1.ConfigAuthor{}, Sha256: "cccccccccccccccc", LinesAdded: 40, BeforeHistory: true},
	}, Redacted: true}, time.UTC)
	want := `ID  APPLIED              BY                                  CHANGES  SHA256
3   2026-10-19 14:05:00  carol (os:operator) via controller  +1 -2    aaaaaaaaaaaa  running, back to 1
2   2026-10-19 14:04:00  admin (os:admin)                    +2 -1    bbbbbbbbbbbb
1   -                    -                                   +40 -0   cccccccccccc  found when the history began
Figures from the configurations with their secrets masked: this certificate may read them, not apply one.
`
	var lines []string
	for l := range strings.Lines(out.String()) {
		lines = append(lines, strings.TrimRight(l, " \n")+"\n")
	}
	if got := strings.Join(lines, ""); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
