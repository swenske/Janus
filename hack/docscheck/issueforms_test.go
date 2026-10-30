package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/variants"
)

// The site's builder links every image target variants.mk may suspend to
// its "Need tester" issue form, and its "Need something else…" card to
// the extension request form (site/frontend/src/builder.js). Each form
// exists, sets its labels itself - a labels= parameter in the link needs
// the right to label - and the three tester forms ask the same questions.
func TestIssueForms(t *testing.T) {
	const dir = "../../.github/ISSUE_TEMPLATE"
	builder, err := os.ReadFile("../../site/frontend/src/builder.js")
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(builder), "'"+name+"'") {
			t.Errorf("site/frontend/src/builder.js doesn't link %s", name)
		}
		return string(data)
	}
	idLine := regexp.MustCompile(`(?m)^    id: (\S+)$`)
	ids := func(form string) []string {
		var out []string
		for _, m := range idLine.FindAllStringSubmatch(form, -1) {
			out = append(out, m[1])
		}
		return out
	}

	var first []string
	for _, target := range variants.Targets {
		name := "need-tester-" + target + ".yml"
		form := read(name)
		if want := `labels: ["need-tester", "target:` + target + `"]`; !strings.Contains(form, "\n"+want+"\n") {
			t.Errorf("%s doesn't say %s", name, want)
		}
		got := ids(form)
		if first == nil {
			first = got
			for _, id := range []string{"what", "hardware", "consent"} {
				if !slices.Contains(got, id) {
					t.Errorf("%s asks no %s", name, id)
				}
			}
		} else if !slices.Equal(got, first) {
			t.Errorf("%s asks %v, need-tester-%s.yml %v", name, got, variants.Targets[0], first)
		}
	}
	if form := read("extension-request.yml"); !strings.Contains(form, "\nlabels: [\"extension-request\"]\n") {
		t.Error("extension-request.yml doesn't label its issues extension-request")
	}
}
