package haproxyflow

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// testCase is one of testdata/cases.json: a configuration, what show
// stat lists and the maps read, and what both readings of it - this one
// and the node page's (dashboard/frontend/src/node/flow.js) - find.
type testCase struct {
	Name     string                       `json:"name"`
	Config   string                       `json:"config"`
	Backends []string                     `json:"backends"`
	Maps     map[string]map[string]string `json:"maps"`
	Want     struct {
		Frontends  []wantFrontend `json:"frontends"`
		Backends   []string       `json:"backends"`
		Maps       []string       `json:"maps"`
		Links      []Link         `json:"links"`
		Unresolved []Unresolved   `json:"unresolved"`
	} `json:"want"`
}

type wantFrontend struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Mode    string   `json:"mode"`
	Binds   []string `json:"binds"`
	Answers []string `json:"answers"`
}

// orEmpty is s, never nil: the cases write [] for none.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func TestSharedCases(t *testing.T) {
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []testCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 6 {
		t.Fatalf("%d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			top := Parse(c.Config)
			var fronts []wantFrontend
			for _, f := range top.Frontends {
				fronts = append(fronts, wantFrontend{Name: f.Name, Kind: f.Kind, Mode: f.Mode, Binds: orEmpty(f.Binds), Answers: orEmpty(f.Answers)})
			}
			check(t, "frontends", orEmpty(fronts), c.Want.Frontends)
			check(t, "backends", orEmpty(top.Backends), c.Want.Backends)
			check(t, "maps", orEmpty(top.Maps), c.Want.Maps)
			links, unresolved := top.Links(c.Backends, c.Maps)
			check(t, "links", orEmpty(links), c.Want.Links)
			check(t, "unresolved", orEmpty(unresolved), c.Want.Unresolved)
		})
	}
}

func check(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("%s:\n got %s\nwant %s", what, g, w)
	}
}

func TestTokenize(t *testing.T) {
	for line, want := range map[string][]string{
		"  use_backend api if { hdr(host) -i api.example.com }  # the API": {"use_backend", "api", "if", "{", "hdr(host)", "-i", "api.example.com", "}"},
		`http-request return string "a \"b\" # c" 'd\e'`:                     {"http-request", "return", "string", `a "b" # c`, `d\e`},
		`acl a path_beg /a\ b ""`:                                             {"acl", "a", "path_beg", "/a b", ""},
		"# only a comment":                                                    nil,
		"\tbind :80\r":                                                        {"bind", ":80"},
	} {
		if got := Tokenize(line); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestMapCalls(t *testing.T) {
	got := MapCalls(`req.hdr(host),lower,map_dom(/etc/haproxy/files/hosts.map,be_default)`)
	if want := []MapCall{{"/etc/haproxy/files/hosts.map", "be_default"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("map_dom: %v", got)
	}
	got = MapCalls(`base,map_beg("/etc/haproxy/files/paths.map")`)
	if want := []MapCall{{"/etc/haproxy/files/paths.map", ""}}; !reflect.DeepEqual(got, want) {
		t.Errorf("map_beg: %v", got)
	}
	if got := MapCalls(`req.hdr(host),bitmap(1)`); got != nil {
		t.Errorf("bitmap: %v", got)
	}
}

// A dynamic rule's parts: its prefix and suffix around the expression,
// what it reads through a variable.
func TestDynamicRule(t *testing.T) {
	top := Parse(`
frontend vars
    http-request set-var(txn.be) req.hdr(host),lower,map_str(/etc/haproxy/files/vars.map) if { req.hdr(host) -m found }
    http-request set-var(txn.be) str(be_api) if { path_beg /api }
    use_backend pre_%[var(txn.be)]_post if TRUE
`)
	r := top.Frontends[0].Rules[0]
	want := Rule{Type: "dynamic", Format: "pre_%[var(txn.be)]_post", Cond: "if TRUE", Prefix: "pre_", Suffix: "_post",
		MapCalls: []MapCall{{Path: "/etc/haproxy/files/vars.map"}}, Literals: []string{"be_api"}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("rule:\n got %+v\nwant %+v", r, want)
	}
}
