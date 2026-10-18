package haproxy

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Maps and ACLs are only visible/manageable through the runtime API when
// they're file-backed (e.g. `... map(/etc/haproxy/maps/x.map)` or
// `acl foo src -f /etc/haproxy/acls/x.acl` in the running config) -
// inline ACLs and maps with no backing file don't show up here at all.
// Verified against a real haproxy instance during development (see the
// commit that added this file): `show map`/`show acl` list entries as
// "<id> (<file>) <description>"; a bare "()" means no backing file, and
// those are skipped.
var identifierListLineRE = regexp.MustCompile(`^\d+\s+\(([^)]*)\)`)

func parseIdentifierList(out []byte) []string {
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := identifierListLineRE.FindStringSubmatch(line)
		if m == nil || m[1] == "" {
			continue
		}
		names = append(names, m[1])
	}
	return names
}

// MapList runs "show map" and returns every file-backed map's identifier
// (the same string used to address it in MapGet/MapUpdate).
func (m *Manager) MapList() ([]string, error) {
	out, err := m.statsCommand("show map")
	if err != nil {
		return nil, err
	}
	return parseIdentifierList(out), nil
}

// MapGet runs "show map <mapName>" and returns its key/value entries.
// Each line of output is "<internal-id> <key> <value...>" - the value is
// everything after the key, rejoined, since it may itself contain spaces.
func (m *Manager) MapGet(mapName string) (map[string]string, error) {
	name, err := cliToken("map", mapName)
	if err != nil {
		return nil, err
	}
	out, err := m.statsCommand("show map " + name)
	if err != nil {
		return nil, err
	}
	return parseMapEntries(string(out))
}

// parseMapEntries reads "show map <name>" output. Every entry line starts
// with HAProxy's internal id ("0x..."); anything else is HAProxy's error
// message (e.g. "Unknown map identifier...") - returned as an error
// rather than mistaken for an entry.
func parseMapEntries(out string) (map[string]string, error) {
	entries := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !strings.HasPrefix(fields[0], "0x") {
			return nil, fmt.Errorf("%s", strings.TrimSpace(out))
		}
		if len(fields) < 3 {
			continue
		}
		entries[fields[1]] = strings.Join(fields[2:], " ")
	}
	return entries, nil
}

// MapUpdate deletes or upserts a single map entry. HAProxy's "set map"
// only modifies an *existing* key (confirmed empirically: it returns
// "entry not found." on a missing key, it does not create one), so an
// upsert is implemented as delete-then-add instead - the delete step is
// allowed to fail (the key not existing yet is the normal case for a
// fresh entry, not an error).
func (m *Manager) MapUpdate(mapName, key, value string, del bool) error {
	name, err := cliToken("map", mapName)
	if err != nil {
		return err
	}
	k, err := cliToken("key", key)
	if err != nil {
		return err
	}
	v, err := cliText("value", value)
	if err != nil {
		return err
	}
	if del {
		return mustEmpty(m.statsCommand(fmt.Sprintf("del map %s %s", name, k)))
	}
	_, _ = m.statsCommand(fmt.Sprintf("del map %s %s", name, k))
	return mustEmpty(m.statsCommand(fmt.Sprintf("add map %s %s %s", name, k, v)))
}

// ACLUpdate deletes or upserts a single ACL pattern value. Same
// delete-then-add upsert reasoning as MapUpdate - ACL entries are a flat
// value list (no key), and "add acl" would otherwise create a duplicate
// entry if the value is already present.
func (m *Manager) ACLUpdate(aclName, value string, del bool) error {
	name, err := cliToken("acl", aclName)
	if err != nil {
		return err
	}
	v, err := cliText("value", value)
	if err != nil {
		return err
	}
	if del {
		return mustEmpty(m.statsCommand(fmt.Sprintf("del acl %s %s", name, v)))
	}
	_, _ = m.statsCommand(fmt.Sprintf("del acl %s %s", name, v))
	return mustEmpty(m.statsCommand(fmt.Sprintf("add acl %s %s", name, v)))
}

// mustEmpty treats a non-empty (after trimming) response from a runtime
// API mutation command as an error - empirically, HAProxy's stats socket
// returns an empty line on success and a human-readable message
// ("Key not found.", "entry not found.", ...) on failure for
// add/set/del map and add/del acl.
func mustEmpty(out []byte, err error) error {
	if err != nil {
		return err
	}
	if trimmed := bytes.TrimSpace(out); len(trimmed) > 0 {
		return fmt.Errorf("%s", trimmed)
	}
	return nil
}

// PatternMatch is what "get map" or "get acl" answers: whether a value
// matches one of a map's keys or an ACL's patterns, with HAProxy's own
// matching - the method the configuration reads it with (str, beg, dom,
// reg, ip...).
type PatternMatch struct {
	Found           bool   `json:"found"`
	Method          string `json:"method"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	Pattern         string `json:"pattern,omitempty"` // the key or pattern that matched
	Value           string `json:"value,omitempty"`   // a map's value for it
}

// MapLookup runs "get map <map> <value>": which entry value matches -
// as a request would, unlike MapGet's exact keys.
func (m *Manager) MapLookup(mapName, value string) (PatternMatch, error) {
	return m.patternLookup("get map", "map", mapName, value)
}

// ACLLookup runs "get acl <acl> <value>": which pattern value matches.
func (m *Manager) ACLLookup(aclName, value string) (PatternMatch, error) {
	return m.patternLookup("get acl", "acl", aclName, value)
}

func (m *Manager) patternLookup(verb, what, name, value string) (PatternMatch, error) {
	n, err := cliToken(what, name)
	if err != nil {
		return PatternMatch{}, err
	}
	if value == "" {
		return PatternMatch{}, fmt.Errorf("%w: no value to look up", ErrInvalidArgument)
	}
	v, err := cliText("value", value)
	if err != nil {
		return PatternMatch{}, err
	}
	out, err := m.statsCommand(verb + " " + n + " " + v)
	if err != nil {
		return PatternMatch{}, err
	}
	return parsePatternMatch(strings.TrimSpace(string(out)))
}

// parsePatternMatch reads `type=dom, case=sensitive, found=yes,
// idx=list, key="www.example.com", value="be_default", type="str"` (an
// ACL says match= and pattern=; testdata/get-*.txt).
func parsePatternMatch(out string) (PatternMatch, error) {
	var pm PatternMatch
	seen := false
	for rest := out; rest != ""; {
		name, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		var value string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `", `)
			if end < 0 {
				value, rest = strings.TrimSuffix(after[1:], `"`), ""
			} else {
				value, rest = after[1:1+end], after[1+end+3:]
			}
		} else {
			value, rest, _ = strings.Cut(after, ", ")
		}
		switch name {
		case "type":
			if pm.Method == "" { // the second type= is the value's
				pm.Method = value
			}
		case "case":
			pm.CaseInsensitive = value == "insensitive"
		case "found", "match":
			pm.Found, seen = value == "yes", true
		case "key", "pattern":
			pm.Pattern = value
		case "value":
			pm.Value = value
		}
	}
	if !seen {
		if strings.HasPrefix(out, "Unknown") {
			return PatternMatch{}, fmt.Errorf("%w: %s", ErrNotFound, out)
		}
		return PatternMatch{}, fmt.Errorf("%s", out)
	}
	return pm, nil
}
