package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCVSS31: scores of well-known vectors, as the specification's
// calculator gives them.
func TestCVSS31(t *testing.T) {
	for vector, want := range map[string]float64{
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H": 9.8,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H": 10,
		"CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H": 8.8,
		"CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:C/C:H/I:N/A:N": 6.8,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N": 6.1,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:L/I:N/A:N": 5.3,
		"CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:N/A:N": 5.9,
		"CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:L/I:N/A:N": 3.3,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N": 0,
	} {
		if got := cvss31(vector); got != want {
			t.Errorf("cvss31(%s) = %.1f, want %.1f", vector, got, want)
		}
	}
}

// TestFixRecords: every committed record of a fix of Janus's own code is
// complete, named after its id, rated as its CVSS says.
func TestFixRecords(t *testing.T) {
	t.Chdir("../..")
	names, err := fixFiles("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := readFix(readFile, name); err != nil {
			t.Error(err)
		}
	}
}

func writeFix(t *testing.T, dir string, r fixRecord) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fixesDir, r.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// TestFirstPartyUpdates: a release fixes the records added since the
// previous release, grouped by what runs the code - one already released
// isn't fixed again.
func TestFirstPartyUpdates(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(fixesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fix := func(id, severity, cvss, target string) fixRecord {
		return fixRecord{ID: id, Title: "title of " + id, Severity: severity, CVSS: cvss, CWE: []string{"CWE-863"}, Target: target, Description: "what " + id + " was"}
	}
	writeFix(t, dir, fix("JANUS-2026-001", "medium", "CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:C/C:H/I:N/A:N", "node"))
	git(t, "init", "-q")
	git(t, "add", ".")
	git(t, "commit", "-q", "-m", "previous release")
	git(t, "tag", "v1")

	writeFix(t, dir, fix("JANUS-2026-002", "high", "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", "controller"))
	writeFix(t, dir, fix("JANUS-2026-003", "low", "CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:L/I:N/A:N", "node"))
	if err := os.WriteFile(filepath.Join(fixesDir, "README.md"), []byte("not a record"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := firstPartyUpdates(readFile, "v1", "", "v2")
	if err != nil {
		t.Fatal(err)
	}
	summary := func(us []updateRecord) string {
		var parts []string
		for _, u := range us {
			s := u.Name + "(" + u.Target + "," + u.From + "→" + u.To + "):"
			for _, v := range u.Fixes {
				s += " " + v.ID + "/" + v.Severity
			}
			if !u.FirstParty {
				s += " not first-party"
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "; ")
	}
	if want := "janus-controller(controller,v1→v2): JANUS-2026-002/high; janus(node,v1→v2): JANUS-2026-003/low"; summary(got) != want {
		t.Errorf("working tree since v1: %s, want %s", summary(got), want)
	}
	if u := got[0].Fixes[0].URL; u != repoURL+"/blob/v2/security/fixes/JANUS-2026-002.json" {
		t.Errorf("URL %s", u)
	}

	// Between two refs: what the later one added.
	git(t, "add", ".")
	git(t, "commit", "-q", "-m", "fixes")
	if got, err := firstPartyUpdates(func(p string) ([]byte, error) { return gitShow("HEAD", p) }, "v1", "HEAD", "v2"); err != nil || len(got) != 2 {
		t.Errorf("v1..HEAD: %s, %v", summary(got), err)
	}
	if got, err := firstPartyUpdates(readFile, "HEAD", "", "v3"); err != nil || len(got) != 0 {
		t.Errorf("nothing new since HEAD: %s, %v", summary(got), err)
	}

	// A record rated otherwise than its CVSS is refused.
	writeFix(t, dir, fix("JANUS-2026-004", "critical", "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", "node"))
	if _, err := firstPartyUpdates(readFile, "HEAD", "", "v3"); err == nil || !strings.Contains(err.Error(), "scores 8.8, high") {
		t.Errorf("a critical record scoring 8.8: %v", err)
	}
}

// TestFixAdvisories: each fix of Janus's own code gets its own advisory,
// whatever its severity; the components' advisory leaves them out, and
// only comes when what they fix is high or worse.
func TestFixAdvisories(t *testing.T) {
	d := &securityDoc{Version: "v2026.10.10", Previous: "v2026.10.04", MaxSeverity: "high", Updates: []updateRecord{
		{Name: "janus", Title: "Janus", Target: "node", FirstParty: true, Fixes: []vuln{{ID: "JANUS-2026-001", Severity: "medium"}}},
		{Name: "janus-controller", Title: "Janus Controller", Target: "controller", FirstParty: true, Fixes: []vuln{{ID: "JANUS-2026-002", Severity: "high"}}},
		{Name: "zlib", Title: "zlib", Target: "node", From: "1.3.1", To: "1.3.2", Fixes: []vuln{{ID: "CVE-2026-27171", Severity: "low"}}},
	}}
	records := map[string]*fixRecord{
		"JANUS-2026-001": {ID: "JANUS-2026-001", Title: "secrets", Target: "node", CVSS: "CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:C/C:H/I:N/A:N", CWE: []string{"CWE-522"}, Description: "what was wrong"},
		"JANUS-2026-002": {ID: "JANUS-2026-002", Title: "reader", Target: "controller", CVSS: "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", CWE: []string{"CWE-863"}, Description: "what was wrong"},
	}
	read := func(id string) (*fixRecord, error) { return records[id], nil }
	if needsAdvisory(d) {
		t.Error("a low component fix calls for an advisory because Janus's own fix is high")
	}
	list, err := advisories(d, "https://example.test/r", read)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0]["summary"] != "secrets" || list[1]["summary"] != "reader" {
		t.Fatalf("advisories: %v", list)
	}
	a := list[1]
	if a["cvss_vector_string"] != records["JANUS-2026-002"].CVSS || a["severity"] != nil || !strings.Contains(a["description"].(string), "Fixed in Janus v2026.10.10: https://example.test/r") {
		t.Errorf("advisory: %v", a)
	}
	v := a["vulnerabilities"].([]map[string]any)[0]
	if v["package"].(map[string]string)["name"] != "janus-controller" || v["vulnerable_version_range"] != "< v2026.10.10" {
		t.Errorf("vulnerabilities: %v", v)
	}

	d.Updates[2].Fixes[0].Severity = "critical"
	if list, _ := advisories(d, "https://example.test/r", read); len(list) != 3 || list[2]["severity"] != "critical" || strings.Contains(list[2]["description"].(string), "JANUS-2026") {
		t.Errorf("with a critical component fix: %v", list)
	}
}
