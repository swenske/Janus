package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const repoURL = "https://github.com/swenske/Janus"

var sevIcon = map[string]string{"critical": "🔴", "high": "🟠", "medium": "🟡", "low": "⚪", "unknown": "⚫"}

// vulnLine is one vulnerability as a Markdown list item.
func vulnLine(v vuln) string {
	id := v.ID
	if v.URL != "" {
		id = "[" + v.ID + "](" + v.URL + ")"
	}
	s := fmt.Sprintf("%s %s %s - %s", sevIcon[v.Severity], v.Severity, id, v.Title)
	if v.Exploited {
		s += " - **known exploited (CISA KEV)**"
	}
	return s
}

// vulnSummary is "3 (1 high)": how many, and the worst.
func vulnSummary(vs []vuln) string {
	if len(vs) == 0 {
		return "none known"
	}
	worst := "unknown"
	n := 0
	for _, v := range vs {
		if sevRank(v.Severity) > sevRank(worst) {
			worst, n = v.Severity, 0
		}
		if v.Severity == worst {
			n++
		}
	}
	return fmt.Sprintf("%d (%s %d %s)", len(vs), sevIcon[worst], n, worst)
}

// writeStatus writes `check`'s report: the dashboard issue's body.
func writeStatus(w io.Writer, sts []status, ref string, now time.Time) {
	fmt.Fprintf(w, "Upstream components pinned in [`versions.mk`](%s/blob/main/versions.mk)", repoURL)
	if ref != "" {
		fmt.Fprintf(w, " at `%s`", ref)
	}
	fmt.Fprintf(w, ", checked %s by `make upstream-check` ([docs/upstreams.md](%s/blob/main/docs/upstreams.md)).\n\n", now.UTC().Format("2006-01-02 15:04 UTC"), repoURL)
	fmt.Fprintln(w, "| Component | In | Pinned | Update | Support | Vulnerabilities |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|")
	for _, s := range sts {
		update := "✅ newest"
		switch {
		case s.Latest != "" && s.Manual != "":
			update = "✋ " + short(s.Latest) + " (by hand)"
		case s.Latest != "":
			update = "⬆️ **" + s.Latest + "**"
		}
		if s.Newer != "" {
			update += "<br>also " + s.Newer
		}
		if s.FeedFailed {
			update = "⚠️ unknown"
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s |\n", s.Title, s.Kind, short(s.Pinned), update, supportCell(s.Support), vulnCell(s))
	}
	for _, s := range sts {
		if len(s.Vulns) == 0 && len(s.Errors) == 0 && len(s.Notices) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n### %s %s\n\n", s.Title, short(s.Pinned))
		for _, n := range s.Notices {
			fmt.Fprintln(w, "- 🆕 "+n)
		}
		for _, v := range s.Vulns {
			fmt.Fprintln(w, "- "+vulnLine(v))
		}
		for _, e := range s.Errors {
			fmt.Fprintln(w, "- ⚠️ "+e)
		}
	}
}

func short(v string) string {
	if len(v) == 40 && !strings.Contains(v, ".") {
		return v[:12] // a commit
	}
	return v
}

func supportCell(s *support) string {
	if s == nil {
		return ""
	}
	c := s.Cycle
	if s.LTS {
		c += " LTS"
	}
	switch {
	case s.Ended:
		c = "❌ " + c + " ended " + s.EOL
	case s.Soon:
		c = "⏳ " + c + " until " + s.EOL
	case s.EOL != "":
		c += " until " + s.EOL
	}
	if len(s.Others) > 0 {
		c += "<br>maintained: " + strings.Join(s.Others, ", ")
	}
	return c
}

func vulnCell(s status) string {
	c := vulnSummary(s.Vulns)
	if len(s.Sources) == 0 {
		c = "no source"
	}
	var extra []string
	if s.NotApplicable > 0 {
		extra = append(extra, fmt.Sprintf("%d not applicable", s.NotApplicable))
	}
	if s.Bugs > 0 {
		extra = append(extra, fmt.Sprintf("%d bugs", s.Bugs))
	}
	if len(extra) > 0 {
		c += "<br>" + strings.Join(extra, ", ")
	}
	return c
}

// writeBump writes a bump's pull request body.
func writeBump(w io.Writer, r *bumpResult) {
	fmt.Fprintf(w, "%s **%s → %s**, proposed by `hack/upstream` ([docs/upstreams.md](%s/blob/main/docs/upstreams.md)).\n\n", r.Title, r.From, r.To, repoURL)
	fmt.Fprintln(w, "### 🔏 Verified")
	fmt.Fprintln(w)
	for _, v := range r.Verified {
		fmt.Fprintln(w, "- ✔ "+v)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "### 🔒 Security")
	fmt.Fprintln(w)
	if len(r.Fixed) == 0 {
		fmt.Fprintln(w, "No known vulnerability fixed.")
	}
	for _, v := range r.Fixed {
		fmt.Fprintln(w, "- "+vulnLine(v))
	}
	writeSkipped(w, r.Skipped, r.NotApplicable, r.FixedBugs)
	for _, warn := range r.Warnings {
		fmt.Fprintln(w, "\n⚠️ "+warn)
	}
	fmt.Fprintln(w, "\n### Before merging")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "- [ ] `ci.yml` green")
	fmt.Fprintln(w, "- [ ] `image-build.yml` dispatched on this branch, green (boot tests)")
}

func writeSkipped(w io.Writer, skipped []vuln, notApplicable, bugs int) {
	if len(skipped) > 0 {
		fmt.Fprintln(w, "\nNot applicable to Janus's build:")
		for _, v := range skipped {
			fmt.Fprintf(w, "- %s - *%s*\n", vulnLine(v), v.NotApplicable)
		}
	}
	if n := notApplicable - len(skipped); n > 0 {
		fmt.Fprintf(w, "\n%d more fixed vulnerabilities don't apply: the files they change aren't built.\n", n)
	}
	if bugs > 0 {
		fmt.Fprintf(w, "\nAnd %d bug fixes (MEDIUM/MINOR).\n", bugs)
	}
}

// writeNotesDraft writes a draft of the release notes' 🔒 section - to be
// edited into the hand-written notes, not pasted blindly.
func writeNotesDraft(w io.Writer, d *securityDoc) {
	fmt.Fprintln(w, "## 🔒 Security")
	fmt.Fprintln(w)
	any := false
	for _, u := range d.Updates {
		if len(u.Fixes) == 0 && len(u.Skipped) == 0 && u.FixedBugs == 0 {
			continue
		}
		any = true
		where := whereText(u)
		if u.FirstParty {
			fmt.Fprintf(w, "- **%s itself** (%s), its own code:\n", u.Title, where)
			for _, v := range u.Fixes {
				fmt.Fprintln(w, "  - "+vulnLine(v))
			}
			continue
		}
		fmt.Fprintf(w, "- **%s %s → %s** (%s)", u.Title, u.From, u.To, where)
		if len(u.Fixes) == 0 {
			fmt.Fprintln(w, ": no known vulnerability fixed.")
		} else {
			fmt.Fprintln(w, ":")
			for _, v := range u.Fixes {
				fmt.Fprintln(w, "  - "+vulnLine(v))
			}
		}
		var tail []string
		if len(u.Skipped) > 0 {
			tail = append(tail, fmt.Sprintf("%d more don't apply (%s)", len(u.Skipped), u.Skipped[0].NotApplicable))
		} else if u.NotApplicable > 0 {
			tail = append(tail, fmt.Sprintf("%d more don't apply to Janus's build", u.NotApplicable))
		}
		if u.FixedBugs > 0 {
			tail = append(tail, fmt.Sprintf("%d other bug fixes", u.FixedBugs))
		}
		if len(tail) > 0 {
			fmt.Fprintln(w, "  - "+strings.Join(tail, "; ")+".")
		}
	}
	if !any {
		fmt.Fprintln(w, "No known vulnerability fixed since "+d.Previous+".")
	}
}

// whereText says who an update reaches: "nodes", "nodes with the bird
// extension", "the Controller".
func whereText(u updateRecord) string {
	switch {
	case u.Extension != "":
		return "nodes with the " + u.Extension + " extension"
	case u.Target == "controller":
		return "the Controller"
	case u.Target == "client":
		return "janusctl and the Terraform provider"
	}
	return "nodes"
}
