package main

import (
	"errors"
	"fmt"
	"strings"
)

// bumpResult is what a bump did, for its pull request.
type bumpResult struct {
	Component string `json:"component"`
	Title     string `json:"title"`
	From      string `json:"from"`
	To        string `json:"to"`
	// Verified lists, per artifact, the checks that passed.
	Verified []string `json:"verified"`
	// Fixed: what the old version had and the new one no longer has.
	Fixed []vuln `json:"fixed"`
	// FixedBugs counts the plain bugs fixed (HAProxy's MEDIUM and MINOR).
	FixedBugs int `json:"fixed_bugs"`
	// Skipped: security fixes that can't apply to Janus's build (the
	// kernel's are only counted, in NotApplicable: they are hundreds).
	Skipped       []vuln   `json:"skipped,omitempty"`
	NotApplicable int      `json:"not_applicable"`
	MaxSeverity   string   `json:"max_severity"`
	Warnings      []string `json:"warnings,omitempty"`
}

// bump moves a component to version to (its newest followed release when
// empty): every artifact is downloaded and checked, then versions.mk is
// rewritten - nothing is written unless every check passed.
func bump(e *env, data []byte, c *component, to string) ([]byte, *bumpResult, error) {
	if c.manual != "" {
		return nil, nil, fmt.Errorf("%s is bumped by hand: %s", c.name, c.manual)
	}
	vars := parseVersionsMk(data)
	from := vars[c.versionVar]
	if from == "" {
		return nil, nil, fmt.Errorf("versions.mk has no %s", c.versionVar)
	}
	if to == "" {
		all, err := c.feed.versions(e.ctx, e.f, from)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", c.feed.describe(), err)
		}
		if to, _ = newestVersions(c, from, all); to == "" {
			return nil, nil, fmt.Errorf("%s %s is already the newest release it follows", c.name, from)
		}
	}
	to = sameStyle(from, to)
	if _, ok := parseVersion(to); !ok {
		return nil, nil, fmt.Errorf("%q isn't a version", to)
	}
	res := &bumpResult{Component: c.name, Title: c.title, From: from, To: to}
	vals := map[string]string{c.versionVar: to}
	for _, arch := range c.archs() {
		a, verified, err := fetchAndVerify(e, c, to, arch)
		if err != nil {
			return nil, nil, err
		}
		vals[c.sumVars[arch]] = a.sha256
		res.Verified = append(res.Verified, verified...)
	}
	if err := fixedBetween(e, c, from, to, res); err != nil {
		res.Warnings = append(res.Warnings, "fixed vulnerabilities unknown: "+err.Error())
	}
	out, err := setVars(data, vals)
	if err != nil {
		return nil, nil, err
	}
	return out, res, nil
}

// fetchAndVerify downloads one artifact and runs its component's checks.
func fetchAndVerify(e *env, c *component, v, arch string) (*artifact, []string, error) {
	a := &artifact{version: v, arch: arch, url: c.url(v, arch)}
	var err error
	if a.path, err = e.f.download(e.ctx, a.url); err != nil {
		return nil, nil, err
	}
	if a.sha256, err = fileSHA256(a.path); err != nil {
		return nil, nil, err
	}
	name := a.url[strings.LastIndex(a.url, "/")+1:]
	var verified []string
	for _, ch := range c.checks {
		if err := ch.verify(e.ctx, e.f, a); err != nil {
			return nil, nil, fmt.Errorf("%s: %s: %w", name, ch.describe(), err)
		}
		verified = append(verified, name+": "+ch.describe())
	}
	if len(c.crossChecks) > 0 {
		var missing []string
		ok := false
		for _, ch := range c.crossChecks {
			err := ch.verify(e.ctx, e.f, a)
			switch {
			case err == nil:
				ok = true
				verified = append(verified, name+": "+ch.describe())
			case errors.Is(err, errNoCrossCheck):
				missing = append(missing, ch.describe())
			default:
				return nil, nil, fmt.Errorf("%s: %s: %w", name, ch.describe(), err)
			}
		}
		if !ok {
			return nil, nil, fmt.Errorf("%s: %w (%s)", name, errNoCrossCheck, strings.Join(missing, ", "))
		}
	}
	if len(verified) == 0 {
		return nil, nil, fmt.Errorf("%s: no way to check it", name)
	}
	return a, verified, nil
}

// fixedBetween fills res with what version from had and to no longer has.
func fixedBetween(e *env, c *component, from, to string, res *bumpResult) error {
	res.MaxSeverity = "unknown"
	for _, src := range c.vulns {
		old, err := src.affecting(e, c, from)
		if err != nil {
			return fmt.Errorf("%s: %w", src.describe(), err)
		}
		cur, err := src.affecting(e, c, to)
		if err != nil {
			return fmt.Errorf("%s: %w", src.describe(), err)
		}
		still := map[string]bool{}
		for _, v := range cur {
			still[v.ID] = true
		}
		for _, v := range old {
			switch {
			case still[v.ID]:
			case v.NotApplicable != "":
				res.NotApplicable++
				if _, kernel := src.(kernelCNA); !kernel && !v.Bug {
					res.Skipped = append(res.Skipped, v)
				}
			case v.Bug:
				res.FixedBugs++
			default:
				res.Fixed = append(res.Fixed, v)
			}
		}
	}
	enrich(e, res.Fixed)
	sortVulns(res.Fixed)
	for _, v := range res.Fixed {
		res.MaxSeverity = maxSeverity(res.MaxSeverity, v.Severity)
	}
	return nil
}
