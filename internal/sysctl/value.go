package sysctl

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Normalize puts a value the way this package compares and stores it:
// numbers separated by single spaces ("32768 60999", where the kernel
// prints tabs), port lists sorted and merged ("8080,9100-9110").
func Normalize(kind Kind, s string) string {
	s = strings.TrimSpace(s)
	switch kind {
	case KindPorts:
		ranges, err := parsePorts(s)
		if err != nil {
			return s
		}
		return formatPorts(ranges)
	default:
		return strings.Join(strings.Fields(s), " ")
	}
}

// readValue reads a /proc/sys file, normalized as numbers or words.
func readValue(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(string(data)), " "), nil
}

// components is how many numbers a kind's value has.
func components(kind Kind) int {
	switch kind {
	case KindPair:
		return 2
	case KindTriple:
		return 3
	default:
		return 1
	}
}

// Parse checks a value's form and bounds - not its context (see check) -
// and returns it normalized with its numbers.
func (p *Param) Parse(s string) (string, []int64, error) {
	s = strings.TrimSpace(s)
	switch p.Kind {
	case KindPorts:
		ranges, err := parsePorts(s)
		if err != nil {
			return "", nil, err
		}
		if len(ranges) > p.MaxItems {
			return "", nil, fmt.Errorf("%d ports or ranges at most", p.MaxItems)
		}
		var nums []int64
		for _, r := range ranges {
			for _, port := range []int64{r.lo, r.hi} {
				if port < p.Bounds[0].Min || port > p.Bounds[0].Max {
					return "", nil, fmt.Errorf("port %d isn't between %d and %d", port, p.Bounds[0].Min, p.Bounds[0].Max)
				}
			}
			nums = append(nums, r.lo, r.hi)
		}
		return formatPorts(ranges), nums, nil
	case KindInt, KindEnum, KindPair, KindTriple:
	default:
		return "", nil, errors.New("this parameter can't be set")
	}
	fields := strings.Fields(s)
	want := components(p.Kind)
	if len(fields) != want {
		switch want {
		case 1:
			return "", nil, errors.New("one integer expected")
		default:
			return "", nil, fmt.Errorf("%d integers expected, separated by spaces", want)
		}
	}
	nums := make([]int64, want)
	for i, f := range fields {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("%q isn't an integer", f)
		}
		nums[i] = n
	}
	if p.Kind == KindEnum {
		if !slices.Contains(p.Allowed, nums[0]) {
			return "", nil, fmt.Errorf("must be one of %s", joinInts(p.Allowed, ", "))
		}
	} else {
		for i, n := range nums {
			b := p.Bounds[i]
			if n < b.Min || n > b.Max {
				return "", nil, fmt.Errorf("%s must be between %d and %d", componentName(p, i), b.Min, b.Max)
			}
		}
	}
	return joinInts(nums, " "), nums, nil
}

// componentName names a value's i-th number in an error.
func componentName(p *Param, i int) string {
	switch p.Kind {
	case KindPair:
		return []string{"the low end", "the high end"}[i]
	case KindTriple:
		return []string{"the minimum", "the default", "the maximum"}[i]
	default:
		return "the value"
	}
}

func joinInts(nums []int64, sep string) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(parts, sep)
}

// ints parses a normalized value's numbers (nil for anything else).
func ints(s string) []int64 {
	var out []int64
	for _, f := range strings.Fields(s) {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

type portRange struct{ lo, hi int64 }

// parsePorts reads ip_local_reserved_ports' form: comma-separated ports
// and lo-hi ranges, "" for none.
func parsePorts(s string) ([]portRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []portRange
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		lo, hi, isRange := strings.Cut(item, "-")
		a, err := strconv.ParseInt(strings.TrimSpace(lo), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q isn't a port or a range", item)
		}
		b := a
		if isRange {
			if b, err = strconv.ParseInt(strings.TrimSpace(hi), 10, 64); err != nil || b < a {
				return nil, fmt.Errorf("%q isn't a port or a range", item)
			}
		}
		out = append(out, portRange{a, b})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lo < out[j].lo })
	merged := out[:1]
	for _, r := range out[1:] {
		last := &merged[len(merged)-1]
		if r.lo <= last.hi+1 {
			last.hi = max(last.hi, r.hi)
			continue
		}
		merged = append(merged, r)
	}
	return merged, nil
}

func formatPorts(ranges []portRange) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		if r.lo == r.hi {
			parts[i] = strconv.FormatInt(r.lo, 10)
		} else {
			parts[i] = fmt.Sprintf("%d-%d", r.lo, r.hi)
		}
	}
	return strings.Join(parts, ",")
}

// portSet expands a normalized port list.
func portSet(s string) map[int64]bool {
	ranges, _ := parsePorts(s)
	out := map[int64]bool{}
	for _, r := range ranges {
		for p := r.lo; p <= r.hi; p++ {
			out[p] = true
		}
	}
	return out
}

// env is what check and warn see: the value each Editable parameter would
// have, and the node's state (nil probes: nothing to look at - at boot).
type env struct {
	target map[string]string
	probes Probes
}

func checkPortRange(v []int64, e *env) error {
	if v[1] < v[0]+minPorts-1 {
		return fmt.Errorf("the range must hold %d ports at least", minPorts)
	}
	if e.probes == nil {
		return nil
	}
	listening, err := e.probes.ListeningTCPPorts()
	if err != nil {
		return nil // nothing to compare with
	}
	reserved := portSet(e.target["net.ipv4.ip_local_reserved_ports"])
	var inside []string
	for _, port := range listening {
		if port >= v[0] && port <= v[1] && !reserved[port] {
			inside = append(inside, strconv.FormatInt(port, 10))
		}
	}
	if len(inside) > 0 {
		return fmt.Errorf("the node listens on %s inside this range: reserve %s (net.ipv4.ip_local_reserved_ports), or choose a range above", strings.Join(inside, ", "), pluralIt(len(inside)))
	}
	return nil
}

func pluralIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func warnPortRange(live string, e *env) []string {
	v := ints(live)
	if len(v) != 2 || e.probes == nil {
		return nil
	}
	if err := checkPortRange(v, e); err != nil && !strings.HasPrefix(err.Error(), "the range") {
		return []string{err.Error()}
	}
	return nil
}

func checkBuffers(v []int64, _ *env) error {
	if v[0] > v[1] || v[1] > v[2] {
		return errors.New("the minimum, default and maximum must be in that order")
	}
	return nil
}

func checkFileMax(v []int64, e *env) error {
	if e.probes == nil {
		return nil
	}
	open, err := e.probes.OpenFiles()
	if err != nil {
		return nil
	}
	if v[0]*4 < open*5 {
		return fmt.Errorf("%d files are open: keep at least %d (125%%)", open, (open*5+3)/4)
	}
	return nil
}

// conntrackEntryBytes is about what a conntrack entry takes (struct
// nf_conn and its extensions); the table may use an eighth of the memory.
const conntrackEntryBytes = 320

func checkConntrack(v []int64, e *env) error {
	if e.probes == nil {
		return nil
	}
	if mem, err := e.probes.MemTotal(); err == nil && mem > 0 {
		if limit := mem / 8 / conntrackEntryBytes; v[0] > limit {
			return fmt.Errorf("at most %d with this memory (an eighth of it, at %d bytes an entry)", limit, conntrackEntryBytes)
		}
	}
	if n, err := e.probes.ConntrackEntries(); err == nil && v[0]*4 < n*5 {
		return fmt.Errorf("%d connections are tracked: keep at least %d (125%%)", n, (n*5+3)/4)
	}
	return nil
}
