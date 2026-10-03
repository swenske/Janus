package main

import (
	"strconv"
	"strings"
)

// version is a dotted numeric version as upstreams write them - "6.18.55",
// "v1.53", "2.0.4+ent" - with an optional pre-release suffix ("3.5-dev8",
// "1.13.0-rc1"). Build metadata after "+" is ignored.
type version struct {
	raw  string
	nums []int
	pre  string
}

// parseVersion parses s, or reports false when it isn't a version at all
// (a tag such as "test1", a commit hash).
func parseVersion(s string) (version, bool) {
	t := strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(t, '+'); i >= 0 {
		t = t[:i]
	}
	pre := ""
	if i := strings.IndexByte(t, '-'); i >= 0 {
		t, pre = t[:i], t[i+1:]
		if pre == "" {
			return version{}, false
		}
	}
	if t == "" {
		return version{}, false
	}
	parts := strings.Split(t, ".")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return version{}, false
		}
		nums[i] = n
	}
	return version{raw: s, nums: nums, pre: pre}, true
}

func mustVersion(s string) version {
	v, ok := parseVersion(s)
	if !ok {
		panic("not a version: " + s)
	}
	return v
}

// prerelease reports whether v is a pre-release ("-rc1", "-dev8").
func (v version) prerelease() bool { return v.pre != "" }

// compare orders versions numerically, a missing component counting as
// 0 ("6.18" == "6.18.0"); with equal numbers a pre-release sorts before
// its release.
func (v version) compare(w version) int {
	for i := 0; i < max(len(v.nums), len(w.nums)); i++ {
		a, b := at(v.nums, i), at(w.nums, i)
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	switch {
	case v.pre == w.pre:
		return 0
	case v.pre == "":
		return 1
	case w.pre == "":
		return -1
	}
	return comparePre(v.pre, w.pre)
}

func at(nums []int, i int) int {
	if i < len(nums) {
		return nums[i]
	}
	return 0
}

// comparePre orders pre-release suffixes, numbers inside them numerically
// ("rc2" < "rc10").
func comparePre(a, b string) int {
	for a != "" && b != "" {
		da, db := digitsPrefix(a), digitsPrefix(b)
		if da != "" && db != "" {
			na, _ := strconv.Atoi(da)
			nb, _ := strconv.Atoi(db)
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			if a[0] < b[0] {
				return -1
			}
			return 1
		}
		a, b = a[1:], b[1:]
	}
	switch a {
	case b:
		return 0
	case "":
		return -1
	}
	return 1
}

func digitsPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// branch is v's first n components ("6.18" for n=2), or "" when v has
// fewer.
func (v version) branch(n int) string {
	if len(v.nums) < n {
		return ""
	}
	parts := make([]string, n)
	for i := range n {
		parts[i] = strconv.Itoa(v.nums[i])
	}
	return strings.Join(parts, ".")
}

// sameStyle writes plain version s ("1.54") the way pinned is written:
// with its "v" prefix when pinned has one.
func sameStyle(pinned, s string) string {
	s = strings.TrimPrefix(s, "v")
	if strings.HasPrefix(pinned, "v") {
		return "v" + s
	}
	return s
}
