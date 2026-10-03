package main

import (
	"fmt"
	"strings"
)

// advisoryThreshold: a release gets a GitHub security advisory when what
// it fixes is at least this severe.
const advisoryThreshold = "high"

// needsAdvisory reports whether a release's fixes call for an advisory.
func needsAdvisory(d *securityDoc) bool {
	return sevRank(d.MaxSeverity) >= sevRank(advisoryThreshold) && d.MaxSeverity != "none"
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
		if len(u.Fixes) == 0 {
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
		"severity":    d.MaxSeverity,
		"vulnerabilities": []map[string]any{{
			"package":                  map[string]string{"ecosystem": "other", "name": "janus"},
			"vulnerable_version_range": "< " + d.Version,
			"patched_versions":         d.Version,
		}},
	}
}
