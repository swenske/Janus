package haproxy

import "strings"

// PinnedThreads returns the global keyword of cfg that fixes HAProxy's
// threads - "nbthread" or "cpu-map" - or "" when there's none. Without
// either, HAProxy starts a thread per CPU it may run on: a reload after
// a vCPU was plugged in or taken out follows the machine.
func PinnedThreads(cfg []byte) string {
	inGlobal := false
	for _, raw := range strings.Split(string(cfg), "\n") {
		fields := strings.Fields(uncomment(raw))
		if len(fields) == 0 {
			continue
		}
		if sectionKeywords[fields[0]] {
			inGlobal = fields[0] == "global"
			continue
		}
		if inGlobal && (fields[0] == "nbthread" || fields[0] == "cpu-map") {
			return fields[0]
		}
	}
	return ""
}
