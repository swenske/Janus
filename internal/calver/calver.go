// Package calver compares Janus versions. Releases are CalVer:
// vYYYY.MM.DD, or vYYYY.MM.DD-N for the Nth release of a day. A build
// between releases is git describe's vYYYY.MM.DD[-N]-K-g<sha>[-dirty]:
// K commits after that release.
package calver

import (
	"regexp"
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
