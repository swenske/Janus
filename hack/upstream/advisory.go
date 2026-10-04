package main

import (
	"fmt"
	"strings"
)

// advisoryThreshold: a release gets a GitHub security advisory for the
// components it updates when what they fix is at least this severe. Each
// fix of Janus's own code gets its own advisory, whatever its severity
// (fixAdvisory).
const advisoryThreshold = "high"

// upstreamSeverity is the worst fix among the components a release
// updates - Janus's own fixes left out.
func upstreamSeverity(d *securityDoc) string {
	worst := "none"
	for _, u := range d.Updates {
		if u.FirstParty {
			continue
		}
		for _, v := range u.Fixes {
			if worst == "none" {
				worst = "unknown"
			}
			worst = maxSeverity(worst, v.Severity)
		}
	}
	return worst
}

// needsAdvisory reports whether the components a release updates call
// for an advisory.
func needsAdvisory(d *securityDoc) bool {
	sev := upstreamSeverity(d)
	return sev != "none" && sevRank(sev) >= sevRank(advisoryThreshold)
}

// advisories are the security advisories a release calls for: one per
// fix of Janus's own code (its record read with readFix), then one for
// the components it updates when needsAdvisory.
func advisories(d *securityDoc, releaseURL string, readFix func(id string) (*fixRecord, error)) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, u := range d.Updates {
		if !u.FirstParty {
			continue
		}
		for _, v := range u.Fixes {
			r, err := readFix(v.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, fixAdvisory(r, d.Version, releaseURL))
		}
	}
	if needsAdvisory(d) {
		out = append(out, advisory(d, releaseURL))
	}
	return out, nil
}

// advisory is a repository security advisory's creation request
// (POST /repos/{owner}/{repo}/security-advisories): "Janus before <v>
// ships components with known vulnerabilities". Janus isn't a package of
// an ecosystem GitHub knows, so it's ecosystem "other" - no Dependabot
// alert anywhere, but a public, citable record on the Security tab.
func advisory(d *securityDoc, releaseURL string) map[string]any {
	var titles []string
	var body strings.Builder
	fmt.Fprintf(&body, "Janus %s updates components that had known vulnerabilities in %s. Update to %s: %s\n\n", d.Version, d.Previous, d.Version, releaseURL)
	for _, u := range d.Updates {
		if len(u.Fixes) == 0 || u.FirstParty {
			continue
		}
		titles = append(titles, strings.TrimSuffix(u.Title, " (Go module)"))
		where := whereText(u)
		fmt.Fprintf(&body, "### %s %s → %s (%s)\n\n", u.Title, u.From, u.To, where)
		for _, v := range u.Fixes {
			fmt.Fprintln(&body, "- "+vulnLine(v))
		}
		body.WriteString("\n")
	}
	body.WriteString("Every fix is listed in the release notes' 🔒 section, and in the release's security.json asset.\n")
	if len(titles) > 3 {
		titles = append(titles[:3], "more")
	}
	return map[string]any{
		"summary":     fmt.Sprintf("Janus before %s ships known vulnerabilities (%s)", d.Version, strings.Join(titles, ", ")),
		"description": body.String(),
		"severity":    upstreamSeverity(d),
		"vulnerabilities": []map[string]any{{
			"package":                  map[string]string{"ecosystem": "other", "name": "janus"},
			"vulnerable_version_range": "< " + d.Version,
			"patched_versions":         d.Version,
		}},
	}
}
