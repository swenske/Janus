package logvol

import "testing"

func TestParseKmsg(t *testing.T) {
	k, ok := ParseKmsg([]byte("6,339,5140900,-;Linux version 7.2.10\n SUBSYSTEM=cpu\n"))
	if !ok || k.Priority != 6 || k.Seq != 339 || k.Usec != 5140900 || k.Message != "Linux version 7.2.10" {
		t.Fatalf("ParseKmsg = %+v, %v", k, ok)
	}
	if got := k.Line(); got != "[    5.140900] Linux version 7.2.10" {
		t.Fatalf("Line = %q", got)
	}
	for _, bad := range []string{"no separator", "6,x,1,-;m", "6;m"} {
		if _, ok := ParseKmsg([]byte(bad)); ok {
			t.Errorf("ParseKmsg(%q) parsed", bad)
		}
	}
}
