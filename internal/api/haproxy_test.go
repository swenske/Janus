package api

import (
	"os"
	"testing"
)

// testdata/show-stat.csv is real "show stat" output from this project's
// own haproxy build: a frontend, backend "web" with a health-checked
// server that's down and an unchecked one, and an empty backend.
func TestParseBackends(t *testing.T) {
	csv, err := os.ReadFile("testdata/show-stat.csv")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseBackends(string(csv))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "web" || got[1].Name != "empty" {
		t.Fatalf("backends = %v", got)
	}
	web := got[0].Servers
	if len(web) != 2 {
		t.Fatalf("web servers = %v", web)
	}
	if web[0].Name != "web1" || web[0].Address != "127.0.0.1:1" || web[0].State != "down" {
		t.Errorf("web1 = %+v", web[0])
	}
	if web[1].Name != "web2" || web[1].Address != "10.9.9.9:80" || web[1].State != "no check" {
		t.Errorf("web2 = %+v", web[1])
	}
	if len(got[1].Servers) != 0 {
		t.Errorf("empty backend has servers: %v", got[1].Servers)
	}

	if _, err := parseBackends("no header\n"); err == nil {
		t.Error("CSV without a header parsed")
	}
}
