package api

import (
	"os"
	"slices"
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

// testdata/show-stat-socket-stats.csv is real "show stat" output (HAProxy
// 3.4.6) of issue #64's configuration: an HTTP frontend under `option
// socket-stats` - its two binds have rows of their own (type 3, named
// after the bind) - and a TCP frontend without it. Listeners are no
// backend's servers, and no frontend is a backend.
func TestParseBackendsSocketStats(t *testing.T) {
	csv, err := os.ReadFile("testdata/show-stat-socket-stats.csv")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseBackends(string(csv))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range got {
		s := b.Name + ":"
		for _, sv := range b.Servers {
			s += " " + sv.Name + "@" + sv.Address + "=" + sv.State
		}
		names = append(names, s)
	}
	want := []string{"be_web: web1@192.0.2.10:80=no check web2@192.0.2.11:80=maint", "be_relay: relay1@192.0.2.20:22067=no check"}
	if !slices.Equal(names, want) {
		t.Errorf("backends = %q, want %q", names, want)
	}

	// A show stat without the type column: as before, by svname.
	old := "# pxname,svname,status,addr,\nfe,FRONTEND,OPEN,,\nbe,web1,UP,10.0.0.1:80,\nbe,BACKEND,UP,,\n"
	if got, err := parseBackends(old); err != nil || len(got) != 1 || len(got[0].Servers) != 1 || got[0].Servers[0].Name != "web1" {
		t.Errorf("without a type column: %v, %v", got, err)
	}
}
