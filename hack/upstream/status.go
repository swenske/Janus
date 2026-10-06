package main

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// status is what `check` knows about a component.
type status struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Kind   kind   `json:"kind"`
	Pinned string `json:"pinned"`
	Track  string `json:"track"`
	// Latest is the newest release the component follows, when newer than
	// Pinned; Newer the newest release outside what it follows (a new
	// branch, a new major), when newer still.
	Latest string `json:"latest,omitempty"`
	Newer  string `json:"newer,omitempty"`
	// Bumpable: Latest can be bumped to by the tool.
	Bumpable bool     `json:"bumpable"`
	Manual   string   `json:"manual,omitempty"`
	Support  *support `json:"support,omitempty"`
	// Vulns affect Pinned and apply to Janus; NotApplicable counts those
	// that can't (a driver not built...), Bugs HAProxy's plain bugs.
	Vulns         []vuln   `json:"vulns"`
	NotApplicable int      `json:"not_applicable"`
	Bugs          int      `json:"bugs"`
	Sources       []string `json:"sources,omitempty"`
	Note          string   `json:"note,omitempty"`
	Errors        []string `json:"errors,omitempty"`
	// FeedFailed: its releases couldn't be listed - Latest means nothing.
	FeedFailed bool `json:"feed_failed,omitempty"`
}

// support is the pinned release cycle's support, from endoflife.date.
type support struct {
	Cycle string `json:"cycle"`
	LTS   bool   `json:"lts,omitempty"`
	EOL   string `json:"eol,omitempty"` // date, "" when unannounced
	Ended bool   `json:"ended,omitempty"`
	// Soon: it ends within six months.
	Soon bool `json:"soon,omitempty"`
	// Others are the other maintained cycles, newest first: "3.2 (LTS)".
	Others []string `json:"others,omitempty"`
}

// newestVersions picks, among a feed's versions, the newest the component
// follows and the newest overall, each only when newer than pinned.
func newestVersions(c *component, pinned string, all []string) (latest, newer string) {
	if _, ok := c.feed.(githubCommit); ok {
		if len(all) == 1 && all[0] != pinned {
			return all[0], ""
		}
		return "", ""
	}
	p := mustVersion(pinned)
	var best, bestAny version
	for _, s := range all {
		v, ok := parseVersion(s)
		if !ok || (v.prerelease() && !c.prerelease) || v.compare(p) <= 0 {
			continue
		}
		if bestAny.raw == "" || v.compare(bestAny) > 0 {
			bestAny = v
		}
		in := true
		switch c.track {
		case trackBranch:
			in = v.branch(2) == p.branch(2)
		case trackMajor:
			in = v.branch(1) == p.branch(1)
		case trackKernel:
			in = v.branch(2) == p.branch(2) || (len(v.nums) > 2 && v.nums[2] >= 2)
		}
		if in && (best.raw == "" || v.compare(best) > 0) {
			best = v
		}
	}
	latest = best.raw
	if bestAny.raw != "" && (best.raw == "" || bestAny.compare(best) > 0) {
		newer = bestAny.raw
	}
	return latest, newer
}

// endOfLife reads a release cycle's support from endoflife.date.
func endOfLife(e *env, product, cycle string, now time.Time) (*support, error) {
	var doc struct {
		Result struct {
			Releases []struct {
				Name         string `json:"name"`
				IsLTS        bool   `json:"isLts"`
				IsEOL        bool   `json:"isEol"`
				EOLFrom      string `json:"eolFrom"`
				IsMaintained bool   `json:"isMaintained"`
			} `json:"releases"`
		} `json:"result"`
	}
	if err := getJSON(e.ctx, e.f, "https://endoflife.date/api/v1/products/"+product, &doc); err != nil {
		return nil, err
	}
	s := &support{Cycle: cycle}
	found := false
	for _, r := range doc.Result.Releases {
		if r.Name == cycle {
			found = true
			s.LTS, s.EOL, s.Ended = r.IsLTS, r.EOLFrom, r.IsEOL
			if t, err := time.Parse("2006-01-02", r.EOLFrom); err == nil {
				s.Ended = s.Ended || !now.Before(t)
				s.Soon = !s.Ended && t.Sub(now) < 183*24*time.Hour
			}
			continue
		}
		if r.IsMaintained && !r.IsEOL {
			o := r.Name
			if r.IsLTS {
				o += " (LTS)"
			}
			s.Others = append(s.Others, o)
		}
	}
	if !found {
		return nil, fmt.Errorf("endoflife.date/%s has no cycle %s", product, cycle)
	}
	return s, nil
}

// componentStatus checks one component pinned at the given version.
func componentStatus(e *env, c *component, pinned string, now time.Time) status {
	st := status{Name: c.name, Title: c.title, Kind: c.kind, Pinned: pinned, Track: c.track.String(), Manual: c.manual, Note: c.note}
	if pinned == "" {
		st.Errors = append(st.Errors, "versions.mk has no "+c.versionVar)
		return st
	}
	if all, err := c.feed.versions(e.ctx, e.f, pinned); err != nil {
		st.Errors = append(st.Errors, c.feed.describe()+": "+err.Error())
		st.FeedFailed = true
	} else {
		st.Latest, st.Newer = newestVersions(c, pinned, all)
		st.Bumpable = st.Latest != "" && c.manual == ""
	}
	if c.eol != "" {
		if s, err := endOfLife(e, c.eol, mustVersion(pinned).branch(c.eolCycle), now); err != nil {
			st.Errors = append(st.Errors, "endoflife.date: "+err.Error())
		} else {
			st.Support = s
		}
	}
	for _, src := range c.vulns {
		st.Sources = append(st.Sources, src.describe())
		vs, err := src.affecting(e, c, pinned)
		if err != nil {
			st.Errors = append(st.Errors, src.describe()+": "+err.Error())
			continue
		}
		for _, v := range vs {
			switch {
			case v.NotApplicable != "":
				st.NotApplicable++
			case v.Bug:
				st.Bugs++
			default:
				st.Vulns = append(st.Vulns, v)
			}
		}
	}
	enrich(e, st.Vulns)
	sortVulns(st.Vulns)
	return st
}

// sortVulns puts the most severe first.
func sortVulns(vs []vuln) {
	slices.SortStableFunc(vs, func(a, b vuln) int {
		if d := sevRank(b.Severity) - sevRank(a.Severity); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// checkAll checks every component at the versions vars pins, in parallel.
func checkAll(e *env, vars map[string]string, only []string, now time.Time) []status {
	out := make([]status, len(components))
	var wg sync.WaitGroup
	for i, c := range components {
		if len(only) > 0 && !slices.Contains(only, c.name) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = componentStatus(e, c, vars[c.versionVar], now)
		}()
	}
	wg.Wait()
	var res []status
	for _, s := range out {
		if s.Name != "" {
			res = append(res, s)
		}
	}
	return res
}
