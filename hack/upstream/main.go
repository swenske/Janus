// Command upstream follows what Janus is built from (docs/upstreams.md):
// the upstream components versions.mk pins - new releases, their support,
// the vulnerabilities known in them - and bumps them, checking every
// download before versions.mk changes.
//
//	upstream check [-ref REF] [-only a,b] [-md FILE] [-json FILE]
//
// check reports every component: the newest release it follows, newer
// branches, its support (endoflife.date) and the vulnerabilities known in
// the pinned version that apply to Janus. Without -ref, the Go code's own
// (govulncheck) and the Controller frontend's (npm audit) too. -ref checks
// a release's pins instead of the working tree's.
//
//	upstream bump [-md FILE] [-json FILE] NAME [VERSION]
//
// bump moves a component to VERSION (default: the newest release it
// follows): downloads every artifact, checks it (signatures, upstream's
// checksums, independent ones), then rewrites versions.mk - or nothing.
// -md writes the pull request body.
//
//	upstream security-notes -from REF [-to REF] -version V [-json FILE]
//
// security-notes lists what the changes from REF to REF (default: the
// working tree) fix - upstream components, Go modules, the Go toolchain,
// npm packages - as security.json (-json) and a draft of the release
// notes' 🔒 section on stdout.
//
//	upstream sbom -version V [-out FILE]
//
// sbom writes the working tree's CycloneDX SBOM.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: upstream check|bump|security-notes|sbom ...")
	}
	e := &env{ctx: context.Background(), f: newHTTPFetcher()}
	switch os.Args[1] {
	case "check":
		checkCmd(e, os.Args[2:])
	case "bump":
		bumpCmd(e, os.Args[2:])
	case "security-notes":
		notesCmd(e, os.Args[2:])
	case "sbom":
		sbomCmd(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// output opens a report's destination: a file, or stdout for "" or "-".
func output(path string) (io.Writer, func()) {
	if path == "" || path == "-" {
		return os.Stdout, func() {}
	}
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	return f, func() {
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}
}

func writeJSON(path string, v any) {
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
}

func checkCmd(e *env, args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	ref := fs.String("ref", "", "check versions.mk at this git ref (a release tag) instead of the working tree")
	only := fs.String("only", "", "comma-separated components to check")
	md := fs.String("md", "-", "write the Markdown report here")
	js := fs.String("json", "", "write the report as JSON here")
	_ = fs.Parse(args)

	var mk []byte
	var err error
	if *ref == "" {
		mk, err = os.ReadFile("versions.mk")
	} else {
		mk, err = gitShow(*ref, "versions.mk")
	}
	if err != nil {
		log.Fatal(err)
	}
	e.ref = *ref
	var names []string
	if *only != "" {
		names = strings.Split(*only, ",")
		for _, n := range names {
			if findComponent(n) == nil && n != "janus-go" && n != "controller-npm" {
				log.Fatalf("no component %q", n)
			}
		}
	}
	now := time.Now()
	sts := checkAll(e, parseVersionsMk(mk), names, now)
	if *ref == "" {
		sts = append(sts, selfStatuses(e, names)...)
	}
	w, done := output(*md)
	writeStatus(w, sts, *ref, now)
	done()
	writeJSON(*js, sts)
}

// selfStatuses checks Janus's own dependencies: its Go modules and the
// standard library (govulncheck: only what the code reaches), and the
// Controller frontend's npm packages (npm audit, production ones).
func selfStatuses(e *env, only []string) []status {
	want := func(n string) bool {
		if len(only) == 0 {
			return true
		}
		for _, o := range only {
			if o == n {
				return true
			}
		}
		return false
	}
	var out []status
	if want("janus-go") {
		st := status{Name: "janus-go", Title: "Janus's Go code (modules, standard library)", Kind: "node", Pinned: goToolchain(), Track: "dependabot",
			Sources: []string{"govulncheck"}}
		for _, dir := range []string{".", "terraform-provider-janus"} {
			vs, err := govulncheck(e, "-C", dir, "./...")
			if err != nil {
				st.Errors = append(st.Errors, err.Error())
			}
			st.Vulns = append(st.Vulns, vs...)
		}
		enrich(e, st.Vulns)
		sortVulns(st.Vulns)
		out = append(out, st)
	}
	if want("controller-npm") {
		st := status{Name: "controller-npm", Title: "Controller frontend (npm)", Kind: kindController, Pinned: "package-lock.json", Track: "dependabot",
			Sources: []string{"npm audit"}}
		vs, err := npmAudit("dashboard/frontend")
		if err != nil {
			st.Errors = append(st.Errors, err.Error())
		}
		st.Vulns = vs
		sortVulns(st.Vulns)
		out = append(out, st)
	}
	return out
}

func goToolchain() string {
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		return ""
	}
	return goRequires(mod)["toolchain"]
}

// npmAudit runs npm audit on a lockfile, production packages only.
func npmAudit(dir string) ([]vuln, error) {
	cmd := exec.Command("npm", "audit", "--json", "--omit=dev", "--package-lock-only")
	cmd.Dir = dir
	out, err := cmd.Output()
	// npm audit exits 1 when it finds something: only its output matters.
	var doc struct {
		Vulnerabilities map[string]struct {
			Via []json.RawMessage `json:"via"`
		} `json:"vulnerabilities"`
	}
	if jerr := json.Unmarshal(out, &doc); jerr != nil {
		if err != nil {
			return nil, fmt.Errorf("npm audit in %s: %v", dir, err)
		}
		return nil, fmt.Errorf("npm audit in %s: %v", dir, jerr)
	}
	seen := map[string]bool{}
	var vs []vuln
	for name, v := range doc.Vulnerabilities {
		for _, raw := range v.Via {
			var adv struct {
				Title    string `json:"title"`
				URL      string `json:"url"`
				Severity string `json:"severity"`
			}
			if json.Unmarshal(raw, &adv) != nil || adv.URL == "" || seen[adv.URL] {
				continue // a string: the vulnerability comes through another package
			}
			seen[adv.URL] = true
			id := adv.URL[strings.LastIndex(adv.URL, "/")+1:]
			vs = append(vs, vuln{ID: id, Title: name + ": " + adv.Title, Severity: adv.Severity, URL: adv.URL})
		}
	}
	return vs, nil
}

func bumpCmd(e *env, args []string) {
	fs := flag.NewFlagSet("bump", flag.ExitOnError)
	md := fs.String("md", "", "write the pull request body (Markdown) here")
	js := fs.String("json", "", "write what was done as JSON here")
	_ = fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		log.Fatal("usage: upstream bump [-md FILE] [-json FILE] NAME [VERSION]")
	}
	c := findComponent(fs.Arg(0))
	if c == nil {
		log.Fatalf("no component %q", fs.Arg(0))
	}
	data, err := os.ReadFile("versions.mk")
	if err != nil {
		log.Fatal(err)
	}
	out, res, err := bump(e, data, c, fs.Arg(1))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("versions.mk", out, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("%s: %s -> %s", c.name, res.From, res.To)
	for _, v := range res.Verified {
		log.Printf("  verified %s", v)
	}
	if *md != "" {
		w, done := output(*md)
		writeBump(w, res)
		done()
	}
	writeJSON(*js, res)
}

func notesCmd(e *env, args []string) {
	fs := flag.NewFlagSet("security-notes", flag.ExitOnError)
	from := fs.String("from", "", "the previous release's git ref")
	to := fs.String("to", "", "the new release's git ref (default: the working tree)")
	version := fs.String("version", "", "the new release's version")
	js := fs.String("json", "", "write security.json here")
	_ = fs.Parse(args)
	if *from == "" || *version == "" {
		log.Fatal("usage: upstream security-notes -from REF [-to REF] -version V [-json FILE]")
	}
	doc, err := securityNotes(e, *from, *to, *version)
	if err != nil {
		log.Fatal(err)
	}
	writeNotesDraft(os.Stdout, doc)
	writeJSON(*js, doc)
}

func sbomCmd(args []string) {
	fs := flag.NewFlagSet("sbom", flag.ExitOnError)
	version := fs.String("version", "", "the release's version")
	out := fs.String("out", "-", "write the SBOM here")
	_ = fs.Parse(args)
	if *version == "" {
		log.Fatal("usage: upstream sbom -version V [-out FILE]")
	}
	// The commit's date, not the clock's: the same tree gives the same SBOM.
	when := time.Now()
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			when = time.Unix(n, 0)
		}
	} else if b, err := exec.Command("git", "log", "-1", "--format=%ct").Output(); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			when = time.Unix(n, 0)
		}
	}
	data, err := sbom(*version, when)
	if err != nil {
		log.Fatal(err)
	}
	w, done := output(*out)
	if _, err := w.Write(append(data, '\n')); err != nil {
		log.Fatal(err)
	}
	done()
}
