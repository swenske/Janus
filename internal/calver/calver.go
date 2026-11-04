// Package calver compares Janus versions. Releases are CalVer:
// vYYYY.MM.DD, or vYYYY.MM.DD-N for the Nth release of a day. A build
// between releases is git describe's vYYYY.MM.DD[-N]-K-g<sha>[-dirty]:
// K commits after that release.
package calver

import (
	"regexp"
	"slices"
	"strconv"
)

var (
	releaseRE  = regexp.MustCompile(`^v(\d{4})\.(\d{2})\.(\d{2})(?:-(\d+))?$`)
	describeRE = regexp.MustCompile(`^v(\d{4})\.(\d{2})\.(\d{2})(?:-(\d+))?(?:-\d+-g[0-9a-f]+)?(?:-dirty)?$`)
)

// IsRelease reports whether v is a release version.
func IsRelease(v string) bool { return releaseRE.MatchString(v) }

// Compare places release against running: 1 newer, 0 the same, -1
// older. known is false when either can't be compared - release isn't a
// release, running neither a release nor a build after one ("dev", a
// bare SHA).
func Compare(release, running string) (cmp int, known bool) {
	r, ok := parse(releaseRE, release)
	if !ok {
		return 0, false
	}
	c, ok := parse(describeRE, running)
	if !ok {
		return 0, false
	}
	for i := range r {
		if r[i] != c[i] {
			if r[i] > c[i] {
				return 1, true
			}
			return -1, true
		}
	}
	return 0, true
}

// SameDay reports whether two releases are of the same day: a
// maintenance release of vYYYY.MM.DD[-N] takes one of that day's
// versions, so it sorts between the release it fixes and the next day's.
func SameDay(a, b string) bool {
	va, oka := parse(releaseRE, a)
	vb, okb := parse(releaseRE, b)
	return oka && okb && [3]int(va[:3]) == [3]int(vb[:3])
}

// SortNewestFirst sorts s by the release version each element carries,
// newest first; those whose version isn't a release keep their order,
// after the releases. GitHub lists releases by the date of the commit
// each tag names, not by version: a maintenance release - a fix of an
// older release, tagged later on its own branch (docs/contributing/
// releasing.md) - comes first there, though it is older than the newest.
func SortNewestFirst[T any](s []T, version func(T) string) {
	slices.SortStableFunc(s, func(a, b T) int {
		va, oka := parse(releaseRE, version(a))
		vb, okb := parse(releaseRE, version(b))
		switch {
		case oka && okb:
			for i := range va {
				if va[i] != vb[i] {
					return vb[i] - va[i]
				}
			}
			return 0
		case oka:
			return -1
		case okb:
			return 1
		}
		return 0
	})
}

func parse(re *regexp.Regexp, v string) ([4]int, bool) {
	m := re.FindStringSubmatch(v)
	if m == nil {
		return [4]int{}, false
	}
	var out [4]int
	for i := range 4 {
		if m[i+1] != "" {
			out[i], _ = strconv.Atoi(m[i+1])
		}
	}
	return out, true
}
