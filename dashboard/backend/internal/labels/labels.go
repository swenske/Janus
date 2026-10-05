// Package labels: what a node is - team=web, env=prod -, and selectors
// that pick nodes by them: a permission on the nodes that have all of a
// selector's labels (the Controller's scoped grants and tokens).
package labels

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// MaxLabels is the most a node or a selector has.
const MaxLabels = 32

var (
	keyPattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]{0,61}[a-z0-9])?$`)
	valuePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
)

// Check refuses labels a node can't carry: keys of lowercase letters,
// digits, '.', '_', '/' and '-', values of letters, digits, '.', '_' and
// '-', 63 characters each at most, starting and ending alphanumeric.
func Check(l map[string]string) error {
	if len(l) > MaxLabels {
		return fmt.Errorf("%d labels: %d at most", len(l), MaxLabels)
	}
	for k, v := range l {
		if !keyPattern.MatchString(k) {
			return fmt.Errorf("label key %q: lowercase letters, digits, '.', '_', '/', '-' - 63 at most, alphanumeric at both ends", k)
		}
		if !valuePattern.MatchString(v) {
			return fmt.Errorf("label %s=%q: letters, digits, '.', '_', '-' - 63 at most, alphanumeric at both ends", k, v)
		}
	}
	return nil
}

// Match reports whether a node with labels l has every label of
// selector - an empty selector matches every node.
func Match(selector, l map[string]string) bool {
	for k, v := range selector {
		if l[k] != v {
			return false
		}
	}
	return true
}

// String is l as "key=value,..." in key order.
func String(l map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, ",")
}

// Parse reads "key=value,..." (spaces around each allowed); empty: none.
func Parse(s string) (map[string]string, error) {
	l := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, errors.New("labels are key=value, comma-separated")
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if _, dup := l[k]; dup {
			return nil, fmt.Errorf("label %q twice", k)
		}
		l[k] = v
	}
	return l, Check(l)
}
