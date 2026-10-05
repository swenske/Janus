package labels

import "testing"

func TestLabels(t *testing.T) {
	l, err := Parse(" team=web , env=prod")
	if err != nil || String(l) != "env=prod,team=web" {
		t.Fatalf("%v %v", l, err)
	}
	for _, s := range []string{"team", "Team=web", "team=-web", "team=web,team=api", "a=b c", "team=" + string(make([]byte, 64))} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if m, _ := Parse(""); len(m) != 0 {
		t.Error("empty")
	}
	if !Match(nil, l) || !Match(map[string]string{"team": "web"}, l) || Match(map[string]string{"team": "api"}, l) || Match(map[string]string{"team": "web", "zone": "a"}, l) {
		t.Error("Match")
	}
}
