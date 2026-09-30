package extensions

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func writeManifest(t *testing.T, dir, name, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if ms, err := Load(filepath.Join(dir, "absent")); err != nil || ms != nil {
		t.Fatalf("a missing dir: %v %v", ms, err)
	}
	writeManifest(t, dir, "b.json", `{"name":"b","version":"1","services":[{"id":"b-svc","path":"/usr/bin/b"}]}`)
	writeManifest(t, dir, "a.json", `{"name":"a","version":"1","selinux_labels":{"usr/bin/a":"a_exec_t"}}`)
	writeManifest(t, dir, "README", "not a manifest")
	ms, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Name != "a" || ms[1].Services[0].ID != "b-svc" {
		t.Errorf("loaded %+v", ms)
	}

	for name, doc := range map[string]string{
		"reserved id":    `{"name":"x","version":"1","services":[{"id":"haproxy","path":"/x"}]}`,
		"relative path":  `{"name":"x","version":"1","services":[{"id":"x","path":"x"}]}`,
		"no version":     `{"name":"x","services":[]}`,
		"bad name":       `{"name":"X","version":"1"}`,
		"escaping label": `{"name":"x","version":"1","selinux_labels":{"../etc/shadow":"x_t"}}`,
		"bad type":       `{"name":"x","version":"1","selinux_labels":{"usr/bin/x":"unconfined"}}`,
	} {
		d := t.TempDir()
		writeManifest(t, d, "x.json", doc)
		if _, err := Load(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	d := t.TempDir()
	writeManifest(t, d, "a.json", `{"name":"a","version":"1","services":[{"id":"same","path":"/a"}]}`)
	writeManifest(t, d, "b.json", `{"name":"b","version":"1","services":[{"id":"same","path":"/b"}]}`)
	if _, err := Load(d); err == nil {
		t.Error("a service id provided twice was accepted")
	}
}

func waitState(t *testing.T, m *Manager, id, want string) ServiceState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := m.State(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: state %q, want %q", id, st.State, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSupervision(t *testing.T) {
	logs := map[string]*syncBuf{"long": {}, "crashy": {}}
	m := NewManagerWithTiming([]Manifest{
		{Name: "ext-a", Version: "1", Services: []Service{{ID: "long", Path: "/bin/sh", Args: []string{"-c", "echo hello; exec sleep 60"}}}},
		{Name: "ext-b", Version: "1", Services: []Service{{ID: "crashy", Path: "/bin/sh", Args: []string{"-c", "echo boom; exit 3"}}}},
	}, func(id string) io.Writer { return logs[id] }, Timing{MinBackoff: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond, StableAfter: time.Hour, StopTimeout: 2 * time.Second})
	m.Start()

	st := waitState(t, m, "long", "running")
	if st.Extension != "ext-a" {
		t.Errorf("extension %q", st.Extension)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := m.State("crashy")
		if st.Restarts >= 3 {
			if !strings.Contains(st.LastError, "exit status 3") {
				t.Errorf("last error %q", st.LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("crashy restarted %d times", st.Restarts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Count(logs["crashy"].String(), "boom") < 3 {
		t.Errorf("crashy output: %q", logs["crashy"].String())
	}

	// Stop keeps it stopped; start brings it back.
	if err := m.StopService("long"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "long", "stopped")
	time.Sleep(300 * time.Millisecond)
	if st, _ := m.State("long"); st.State != "stopped" {
		t.Fatalf("a stopped service came back: %s", st.State)
	}
	if err := m.StartService("long"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "long", "running")
	if err := m.RestartService("long"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "long", "running")
	if strings.Count(logs["long"].String(), "hello") < 3 {
		t.Errorf("long output: %q", logs["long"].String())
	}

	// The crash loop can be stopped too.
	if err := m.StopService("crashy"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "crashy", "stopped")
	if err := m.StopService("nope"); !IsNoService(err) {
		t.Errorf("unknown service: %v", err)
	}
	_ = m.StopService("long")
}

func TestWaitFor(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "virtio-port")
	logs := &syncBuf{}
	m := NewManagerWithTiming([]Manifest{
		{Name: "ext", Version: "1", Services: []Service{{ID: "agent", Path: "/bin/sh", Args: []string{"-c", "echo up; exec sleep 60"}, WaitFor: []string{dev}}}},
	}, func(string) io.Writer { return logs }, Timing{MinBackoff: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond, StableAfter: time.Hour, StopTimeout: 2 * time.Second, WaitPoll: 30 * time.Millisecond})
	m.Start()
	defer func() { _ = m.StopService("agent") }()

	st := waitState(t, m, "agent", "waiting")
	if st.LastError != "waiting for "+dev || st.Restarts != 0 {
		t.Fatalf("waiting state %+v", st)
	}
	time.Sleep(200 * time.Millisecond)
	if st, _ := m.State("agent"); st.State != "waiting" || st.Restarts != 0 || logs.String() != "" {
		t.Fatalf("a waiting service started or counted restarts: %+v, output %q", st, logs.String())
	}
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "agent", "running")

	// Stopped while waiting: stopped, not waiting.
	if err := os.Remove(dev); err != nil {
		t.Fatal(err)
	}
	if err := m.RestartService("agent"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "agent", "waiting")
	if err := m.StopService("agent"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "agent", "stopped")
}

func TestWaitForMustBeAbsolute(t *testing.T) {
	m := Manifest{Name: "ext", Version: "1", Services: []Service{{ID: "s", Path: "/bin/true", WaitFor: []string{"dev/x"}}}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "wait_for") {
		t.Fatalf("Validate = %v", err)
	}
}
