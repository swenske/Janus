package haproxy

import (
	"reflect"
	"strings"
	"testing"
)

// Fixtures below are captured verbatim from a real haproxy 3.4.0 instance
// during development (see the commit that added this file) - not
// hand-guessed formats.

func TestParseIdentifierList(t *testing.T) {
	out := []byte("# id (file) description\n" +
		"0 (/tmp/test-maps/hosts.map) pattern loaded from file '/tmp/test-maps/hosts.map' used by map at file '/tmp/test.cfg' line 12. curr_ver=0 next_ver=0 entry_cnt=1\n" +
		"1 () acl 'var' file 'httpclient' line 0. curr_ver=0 next_ver=0 entry_cnt=1\n" +
		"\n")

	got := parseIdentifierList(out)
	want := []string{"/tmp/test-maps/hosts.map"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (non-file-backed entries like \"1 ()\" must be skipped)", got, want)
	}
}

func TestParseIdentifierList_Empty(t *testing.T) {
	got := parseIdentifierList([]byte("# id (file) description\n\n"))
	if len(got) != 0 {
		t.Fatalf("expected no identifiers, got %v", got)
	}
}

// Both outputs captured from this project's own haproxy build.
func TestParseMapEntries(t *testing.T) {
	got, err := parseMapEntries("0x7c65935ff360 www.example.com app1\n0x7c65935ff3d0 api.example.com app2 with spaces\n\n")
	if err != nil || len(got) != 2 || got["www.example.com"] != "app1" || got["api.example.com"] != "app2 with spaces" {
		t.Errorf("parseMapEntries = %v, %v", got, err)
	}
	if got, err := parseMapEntries("\n"); err != nil || len(got) != 0 {
		t.Errorf("empty map = %v, %v", got, err)
	}
	if _, err := parseMapEntries("Unknown map identifier. Please use #<id> or <file>.\n\n"); err == nil || !strings.Contains(err.Error(), "Unknown map identifier") {
		t.Errorf("error output = %v, want HAProxy's message as an error", err)
	}
}
