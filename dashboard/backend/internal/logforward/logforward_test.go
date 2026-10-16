package logforward

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/logship"
)

// xorSealer stands in for the master key: enough to see nothing is kept
// in the clear.
type xorSealer struct{}

func (xorSealer) Seal(p []byte, purpose string) ([]byte, error) {
	out := append([]byte(purpose+":"), p...)
	for i := range out {
		out[i] ^= 0x5a
	}
	return out, nil
}

func (xorSealer) Open(s []byte, purpose string) ([]byte, error) {
	out := bytes.Clone(s)
	for i := range out {
		out[i] ^= 0x5a
	}
	rest, ok := bytes.CutPrefix(out, []byte(purpose+":"))
	if !ok {
		return nil, errors.New("wrong purpose")
	}
	return rest, nil
}

func settings() Settings {
	return Settings{
		Controller: logship.Config{Targets: []logship.Target{{Name: "es", Elasticsearch: &logship.ElasticsearchTarget{URL: "https://e:9200", APIKey: "controller-secret"}}}},
		Profiles: []Profile{
			{Name: "web", Selector: map[string]string{"team": "web"}, Config: logship.Config{Targets: []logship.Target{{Name: "syslog", Syslog: &logship.SyslogTarget{Address: "logs.example"}}}}},
			{Name: "all", Config: logship.Config{Targets: []logship.Target{{Name: "es", Elasticsearch: &logship.ElasticsearchTarget{URL: "https://e:9200", APIKey: "fleet-secret"}}}}},
		},
	}
}

func TestStoreSealsAndKeepsSecrets(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, xorSealer{})
	if err != nil {
		t.Fatal(err)
	}
	set, err := s.Set(settings())
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Controller.Targets[0].Sources; len(got) != 2 || got[0] != "controller" {
		t.Errorf("the Controller's target's default sources: %v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "logs.example") {
		t.Fatalf("state.json in the clear: %s", raw)
	}
	// Read back without secrets, sent again: kept.
	again := set.Redacted()
	if again.Profiles[1].Config.Targets[0].Elasticsearch.APIKey != "" {
		t.Fatal("Redacted kept a secret")
	}
	if _, err := s.Set(again); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(dir, xorSealer{})
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Settings()
	if got.Controller.Targets[0].Elasticsearch.APIKey != "controller-secret" || got.Profiles[1].Config.Targets[0].Elasticsearch.APIKey != "fleet-secret" {
		t.Fatalf("secrets lost: %+v", got)
	}
}

func TestProfiles(t *testing.T) {
	s := settings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if p, ok := s.ProfileFor(map[string]string{"team": "web", "env": "prod"}); !ok || p.Name != "web" {
		t.Errorf("a web node got %q", p.Name)
	}
	if p, ok := s.ProfileFor(map[string]string{"team": "db"}); !ok || p.Name != "all" {
		t.Errorf("another node got %q", p.Name)
	}
	a, b := ManagedBy("0123456789abcdef", s.Profiles[0]), ManagedBy("0123456789abcdef", s.Profiles[1])
	if !strings.HasPrefix(a, ManagedByPrefix("0123456789abcdef")+"profile web ") || a == b {
		t.Errorf("managed_by: %q, %q", a, b)
	}
	// A secret changed: another digest - the nodes get it again.
	changed := settings()
	changed.Profiles[1].Config.Targets[0].Elasticsearch.APIKey = "rotated"
	if ManagedBy("0123456789abcdef", changed.Profiles[1]) == b {
		t.Error("a rotated API key didn't change managed_by")
	}
	for _, bad := range []func(*Settings){
		func(s *Settings) { s.Profiles[0].Name = "Web" },
		func(s *Settings) { s.Profiles[1].Name = "web" },
		func(s *Settings) { s.Profiles[0].Selector = map[string]string{"Bad Key": "x"} },
		func(s *Settings) { s.Controller.Targets[0].Sources = []string{"janusd"} },
		func(s *Settings) { s.Profiles[0].Config.Targets[0].Sources = []string{"controller"} },
		func(s *Settings) { s.Controller.AccessLogsOnDisk = "always" },
	} {
		s := settings()
		bad(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("accepted: %+v", s)
		}
	}
}
