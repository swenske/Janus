package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
)

// fixesDir holds one record per vulnerability fixed in Janus's own code
// (security/fixes/<id>.json), committed with its fix: a release fixes
// the ones added since the previous release - in its security.json, its
// name, its 🔒 section and a security advisory each, whatever their
// severity.
const fixesDir = "security/fixes"

// fixRecord is one vulnerability of Janus's own code.
type fixRecord struct {
	ID    string `json:"id"`    // JANUS-<year>-<nnn>, the file's name
	Title string `json:"title"` // one line: what an attacker could do
	// Severity follows CVSS, and must match it.
	Severity string   `json:"severity"`
	CVSS     string   `json:"cvss"` // CVSS:3.1/... vector
	CWE      []string `json:"cwe"`
	// Target is what runs the vulnerable code: "node", "controller" or
	// "client" (janusctl, the Terraform provider).
	Target string `json:"target"`
	// Description is the advisory's body (Markdown): what was wrong, who
	// is affected, what the fix does, what to do besides updating.
	Description string `json:"description"`
}

var (
	fixIDPattern = regexp.MustCompile(`^JANUS-20[0-9]{2}-[0-9]{3}$`)
	cvssPattern  = regexp.MustCompile(`^CVSS:3\.1/AV:[NALP]/AC:[LH]/PR:[NLH]/UI:[NR]/S:[UC]/C:[NLH]/I:[NLH]/A:[NLH]$`)
	cwePattern   = regexp.MustCompile(`^CWE-[0-9]+$`)
)

// validate checks a record read from name.
func (r *fixRecord) validate(name string) error {
	var errs []string
	if !fixIDPattern.MatchString(r.ID) || name != r.ID+".json" {
		errs = append(errs, fmt.Sprintf("id %q: JANUS-<year>-<nnn>, the file's name", r.ID))
	}
	if r.Title == "" || strings.Contains(r.Title, "\n") {
		errs = append(errs, "title: one line")
	}
	if !slices.Contains([]string{"low", "medium", "high", "critical"}, r.Severity) {
		errs = append(errs, fmt.Sprintf("severity %q: low, medium, high or critical", r.Severity))
	}
	if !cvssPattern.MatchString(r.CVSS) {
		errs = append(errs, fmt.Sprintf("cvss %q: a CVSS:3.1 base vector", r.CVSS))
	} else if score := cvss31(r.CVSS); cvssSeverity(score) != r.Severity {
		errs = append(errs, fmt.Sprintf("severity %q: its CVSS scores %.1f, %s", r.Severity, score, cvssSeverity(score)))
	}
	if len(r.CWE) == 0 || slices.ContainsFunc(r.CWE, func(c string) bool { return !cwePattern.MatchString(c) }) {
		errs = append(errs, fmt.Sprintf("cwe %v: at least one CWE-<n>", r.CWE))
	}
	if !slices.Contains([]string{"node", "controller", "client"}, r.Target) {
		errs = append(errs, fmt.Sprintf("target %q: node, controller or client", r.Target))
	}
	if strings.TrimSpace(r.Description) == "" {
		errs = append(errs, "description: empty")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s/%s: %s", fixesDir, name, strings.Join(errs, "; "))
	}
	return nil
}

// fixFiles lists the records' file names at a git ref ("" = the working
// tree).
func fixFiles(ref string) ([]string, error) {
	var names []string
	if ref == "" {
		entries, err := os.ReadDir(fixesDir)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			names = append(names, e.Name())
		}
	} else {
		out, err := exec.Command("git", "ls-tree", "--name-only", ref, fixesDir+"/").Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-tree %s %s: %w", ref, fixesDir, err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" {
				names = append(names, path.Base(l))
			}
		}
	}
	return slices.DeleteFunc(names, func(n string) bool { return !strings.HasSuffix(n, ".json") }), nil
}

// readFix reads and checks one record.
func readFix(read func(path string) ([]byte, error), name string) (*fixRecord, error) {
	data, err := read(fixesDir + "/" + name)
	if err != nil {
		return nil, err
	}
	var r fixRecord
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s/%s: %w", fixesDir, name, err)
	}
	return &r, r.validate(name)
}

// fixTitles are the update names and titles of Janus's own fixes, by
// target - the package an advisory names, too.
var fixTitles = map[string][2]string{
	"node":       {"janus", "Janus"},
	"controller": {"janus-controller", "Janus Controller"},
	"client":     {"janusctl", "janusctl and the Terraform provider"},
}

// firstPartyUpdates are the fixes of Janus's own code the release brings:
// the records at to that weren't at from, one update per target.
func firstPartyUpdates(read func(path string) ([]byte, error), from, to, version string) ([]updateRecord, error) {
	before, err := fixFiles(from)
	if err != nil {
		return nil, err
	}
	now, err := fixFiles(to)
	if err != nil {
		return nil, err
	}
	byTarget := map[string]int{} // index in out
	var out []updateRecord
	for _, name := range now {
		if slices.Contains(before, name) {
			continue
		}
		r, err := readFix(read, name)
		if err != nil {
			return nil, err
		}
		i, ok := byTarget[r.Target]
		if !ok {
			t := fixTitles[r.Target]
			out = append(out, updateRecord{Name: t[0], Title: t[1], Target: r.Target, From: from, To: version, FirstParty: true})
			i = len(out) - 1
			byTarget[r.Target] = i
		}
		out[i].Fixes = append(out[i].Fixes, vuln{ID: r.ID, Title: r.Title, Severity: r.Severity, URL: repoURL + "/blob/" + version + "/" + fixesDir + "/" + name})
	}
	return out, nil
}

// fixAdvisory is the security advisory of one of Janus's own fixes.
func fixAdvisory(r *fixRecord, version, releaseURL string) map[string]any {
	return map[string]any{
		"summary":            r.Title,
		"description":        strings.TrimSpace(r.Description) + "\n\nFixed in Janus " + version + ": " + releaseURL + "\n",
		"cvss_vector_string": r.CVSS,
		"cwe_ids":            r.CWE,
		"vulnerabilities": []map[string]any{{
			"package":                  map[string]string{"ecosystem": "other", "name": fixTitles[r.Target][0]},
			"vulnerable_version_range": "< " + version,
			"patched_versions":         version,
		}},
	}
}

// cvss31 is a CVSS 3.1 base vector's score (the specification's
// formulas), the vector already checked by cvssPattern.
func cvss31(vector string) float64 {
	m := map[string]string{}
	for _, part := range strings.Split(vector, "/")[1:] {
		k, v, _ := strings.Cut(part, ":")
		m[k] = v
	}
	changed := m["S"] == "C"
	av := map[string]float64{"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2}[m["AV"]]
	ac := map[string]float64{"L": 0.77, "H": 0.44}[m["AC"]]
	pr := map[string]float64{"N": 0.85, "L": 0.62, "H": 0.27}[m["PR"]]
	if changed {
		pr = map[string]float64{"N": 0.85, "L": 0.68, "H": 0.5}[m["PR"]]
	}
	ui := map[string]float64{"N": 0.85, "R": 0.62}[m["UI"]]
	cia := map[string]float64{"H": 0.56, "L": 0.22, "N": 0}
	iss := 1 - (1-cia[m["C"]])*(1-cia[m["I"]])*(1-cia[m["A"]])
	impact := 6.42 * iss
	if changed {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	}
	if impact <= 0 {
		return 0
	}
	exploitability := 8.22 * av * ac * pr * ui
	if changed {
		return roundUp(min(1.08*(impact+exploitability), 10))
	}
	return roundUp(min(impact+exploitability, 10))
}

// roundUp is CVSS 3.1's Roundup: the smallest one-decimal number not
// below x.
func roundUp(x float64) float64 {
	n := int(math.Round(x * 100000))
	if n%10000 == 0 {
		return float64(n) / 100000
	}
	return float64(n/10000+1) / 10
}

// cvssSeverity is a score's qualitative rating.
func cvssSeverity(score float64) string {
	switch {
	case score >= 9:
		return "critical"
	case score >= 7:
		return "high"
	case score >= 4:
		return "medium"
	case score > 0:
		return "low"
	}
	return "none"
}
