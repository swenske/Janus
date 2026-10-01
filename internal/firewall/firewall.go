// Package firewall manages the node's nftables ruleset - the nftables
// extension (docs/firewall.md). The document is the node's whole ruleset
// in nft's own syntax: janusd validates it with nft --check, applies it
// on trial and reverts to the previous one unless it's confirmed over a
// connection opened after it was applied (conntrack keeps established
// connections, so only a new one proves the node is still reachable),
// saves it on STATE once confirmed and applies it again at every boot.
//
// Named sets are edited live (UpdateSet) without reloading the ruleset.
// Elements added without a timeout are kept on STATE and added back
// whenever the ruleset is applied; elements with a timeout expire.
package firewall

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/events"
)

// Binary is nft, from the nftables extension; Dir is where the ruleset
// and the kept set elements are saved (STATE's config/); RunDir holds the
// documents being checked or applied. Vars, so tests can move them.
var (
	Binary = "/usr/local/sbin/nft"
	Dir    = "/etc/janus/config"
	RunDir = "/run/janus/firewall"
)

const (
	rulesetFile = "firewall.nft"
	setsFile    = "firewall-sets.json"

	// DefaultTrial is how long an applied ruleset waits for its
	// confirmation; MinTrial and MaxTrial bound what can be asked for.
	DefaultTrial = 30 * time.Second
	MinTrial     = 5 * time.Second
	MaxTrial     = 300 * time.Second

	maxRulesetBytes = 1 << 20
)

// ErrNotAvailable means the image doesn't have the nftables extension.
var ErrNotAvailable = errors.New("nftables isn't in this node's image - optional modules are chosen when the image is built")

// Available reports whether nft is in the image.
func Available() bool {
	_, err := os.Stat(Binary)
	return err == nil
}

// Manager applies the ruleset and edits sets. One per janusd.
type Manager struct {
	mu    sync.Mutex
	trial *trial
	now   func() time.Time
}

type trial struct {
	started, revertAt time.Time
	ruleset           string // on trial
	previous          string // to revert to; "" means no firewall
	timer             *time.Timer
}

func New() *Manager { return &Manager{now: time.Now} }

// Saved returns the confirmed ruleset; isDefault means there's none -
// the node has no firewall.
func Saved() (ruleset string, isDefault bool, err error) {
	data, err := os.ReadFile(filepath.Join(Dir, rulesetFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", true, nil
	}
	return string(data), false, nil
}

// Boot applies the saved ruleset, if any, and its kept set elements.
func (m *Manager) Boot() error {
	if !Available() {
		return nil
	}
	rs, isDefault, err := Saved()
	if err != nil {
		return err
	}
	if isDefault {
		return nil
	}
	if errs, err := apply(rs); err != nil {
		return fmt.Errorf("%w: %s", err, strings.Join(errs, "; "))
	}
	m.restoreElements()
	log.Printf("firewall: applied the saved ruleset")
	return nil
}

// Check validates ruleset without applying it; it returns nft's errors.
func Check(ruleset string) ([]string, error) {
	if len(ruleset) > maxRulesetBytes {
		return []string{fmt.Sprintf("the ruleset is over %d bytes", maxRulesetBytes)}, errors.New("invalid ruleset")
	}
	return runDocument(ruleset, true)
}

// apply replaces the node's whole ruleset with ruleset ("" removes it).
func apply(ruleset string) ([]string, error) {
	return runDocument(ruleset, false)
}

// runDocument checks or applies ruleset as the whole ruleset: a wrapper
// flushes the current one and includes the document, so nft reports
// errors with the document's own line numbers.
func runDocument(ruleset string, check bool) ([]string, error) {
	if err := os.MkdirAll(RunDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(RunDir, "ruleset-*.nft")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(ruleset); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	wrapper := fmt.Sprintf("flush ruleset\ninclude %q\n", f.Name())
	args := []string{"-f", "-"}
	if check {
		args = []string{"--check", "-f", "-"}
	}
	_, stderr, err := run(wrapper, args...)
	if err != nil {
		return errorLines(stderr, f.Name()), fmt.Errorf("invalid ruleset")
	}
	return nil, nil
}

// errorLines turns nft's stderr into one error per line, naming the
// document "ruleset" rather than its temporary file.
func errorLines(stderr, file string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
		// The wrapper that includes the document isn't the user's.
		if strings.HasPrefix(l, "In file included from ") {
			continue
		}
		if file != "" {
			l = strings.ReplaceAll(l, file, "ruleset")
		}
		l = strings.ReplaceAll(l, "/dev/stdin", "ruleset")
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		out = []string{"nft refused the ruleset"}
	}
	return out
}

func run(stdin string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(Binary, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// ApplyTrial validates ruleset and applies it on trial: unless Confirm
// is called within timeout, the ruleset that was there before the trial
// is put back. Applying again during a trial replaces what's on trial and
// restarts the countdown; the revert still goes back to the ruleset from
// before the first one.
func (m *Manager) ApplyTrial(ruleset string, timeout time.Duration) (time.Time, []string, error) {
	if !Available() {
		return time.Time{}, nil, ErrNotAvailable
	}
	timeout = cmp.Or(timeout, DefaultTrial)
	if timeout < MinTrial || timeout > MaxTrial {
		return time.Time{}, nil, fmt.Errorf("the confirmation timeout must be between %s and %s", MinTrial, MaxTrial)
	}
	if errs, err := Check(ruleset); err != nil {
		return time.Time{}, errs, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	previous := ""
	if m.trial != nil {
		m.trial.timer.Stop()
		previous = m.trial.previous
	} else {
		saved, _, err := Saved()
		if err != nil {
			return time.Time{}, nil, err
		}
		previous = saved
	}
	if errs, err := apply(ruleset); err != nil {
		return time.Time{}, errs, err
	}
	m.restoreElements()
	now := m.now()
	t := &trial{started: now, revertAt: now.Add(timeout), ruleset: ruleset, previous: previous}
	t.timer = time.AfterFunc(timeout, func() { m.revert(t) })
	m.trial = t
	log.Printf("firewall: ruleset applied on trial, reverts at %s unless confirmed", t.revertAt.Format(time.RFC3339))
	events.Publish("firewall.trial", map[string]any{"revert_at": t.revertAt.Unix()})
	return t.revertAt, nil, nil
}

func (m *Manager) revert(t *trial) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trial != t {
		return
	}
	m.trial = nil
	if errs, err := apply(t.previous); err != nil {
		log.Printf("firewall: revert failed, the ruleset on trial stays: %v %v", err, errs)
		events.Publish("firewall.revert_failed", map[string]any{"error": strings.Join(errs, "; ")})
		return
	}
	m.restoreElements()
	log.Printf("firewall: not confirmed in time - reverted to the previous ruleset")
	events.Publish("firewall.reverted", nil)
}

// ErrNoTrial is Confirm with nothing on trial.
var ErrNoTrial = errors.New("no firewall ruleset is on trial")

// ErrOldConnection is a confirmation over a connection that predates the
// trial, which doesn't prove new connections still get through.
var ErrOldConnection = errors.New("confirm over a new connection: this one was opened before the ruleset was applied, and established connections are kept whatever the ruleset - it proves nothing")

// Confirm keeps the ruleset on trial and saves it. connOpened is when the
// connection carrying the confirmation was opened; it must be after the
// trial started.
func (m *Manager) Confirm(connOpened time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.trial
	if t == nil {
		return ErrNoTrial
	}
	if !connOpened.After(t.started) {
		return ErrOldConnection
	}
	t.timer.Stop()
	m.trial = nil
	if strings.TrimSpace(t.ruleset) == "" {
		// No ruleset: no firewall, nothing to apply at boot.
		if err := os.Remove(filepath.Join(Dir, rulesetFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("the firewall is removed but the old ruleset couldn't be deleted - it comes back after a reboot: %w", err)
		}
		m.pruneElements()
		log.Printf("firewall: empty ruleset confirmed - the node has no firewall")
		events.Publish("firewall.confirmed", nil)
		return nil
	}
	if err := saveFile(rulesetFile, []byte(t.ruleset)); err != nil {
		return fmt.Errorf("the ruleset stays applied but couldn't be saved - it won't be there after a reboot: %w", err)
	}
	m.pruneElements()
	log.Printf("firewall: ruleset confirmed and saved")
	events.Publish("firewall.confirmed", nil)
	return nil
}

// Trial reports the ruleset on trial, if any.
func (m *Manager) Trial() (pending bool, revertAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trial == nil {
		return false, time.Time{}
	}
	return true, m.trial.revertAt
}

// Live is the ruleset the kernel has, as nft lists it.
func Live() (string, error) {
	if !Available() {
		return "", ErrNotAvailable
	}
	out, stderr, err := run("", "list", "ruleset")
	if err != nil {
		return "", fmt.Errorf("nft list ruleset: %v: %s", err, strings.TrimSpace(stderr))
	}
	return out, nil
}

// Set is one named set of the live ruleset.
type Set struct {
	Family, Table, Name string
	Type                string // "ipv4_addr", "ipv4_addr . inet_service"
	Flags               []string
	Elements            []Element
}

// Element is a set element, in nft syntax ("192.0.2.7",
// "198.51.100.0/24", "10.0.0.1-10.0.0.9", "192.0.2.1 . 443").
type Element struct {
	Value      string
	Timeout    time.Duration // the element's own timeout, if any
	Expires    time.Duration // time left
	Persistent bool          // kept on STATE, added back after every apply
}

// Sets lists the live ruleset's named sets and their elements.
func (m *Manager) Sets() ([]Set, error) {
	if !Available() {
		return nil, ErrNotAvailable
	}
	out, stderr, err := run("", "-j", "list", "sets")
	if err != nil {
		return nil, fmt.Errorf("nft list sets: %v: %s", err, strings.TrimSpace(stderr))
	}
	sets, err := parseSets([]byte(out))
	if err != nil {
		return nil, err
	}
	kept := loadElements()
	for i := range sets {
		k := kept[setKey(sets[i].Family, sets[i].Table, sets[i].Name)]
		for j := range sets[i].Elements {
			sets[i].Elements[j].Persistent = slices.Contains(k, sets[i].Elements[j].Value)
		}
	}
	return sets, nil
}

// parseSets reads `nft -j list sets`.
func parseSets(data []byte) ([]Set, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("nft's JSON: %w", err)
	}
	var sets []Set
	for _, obj := range doc.Nftables {
		raw, ok := obj["set"]
		if !ok {
			continue
		}
		var s struct {
			Family string            `json:"family"`
			Table  string            `json:"table"`
			Name   string            `json:"name"`
			Type   json.RawMessage   `json:"type"`
			Flags  []string          `json:"flags"`
			Elem   []json.RawMessage `json:"elem"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("nft's JSON: %w", err)
		}
		set := Set{Family: s.Family, Table: s.Table, Name: s.Name, Flags: s.Flags}
		var types []string
		if json.Unmarshal(s.Type, &types) != nil {
			var one string
			_ = json.Unmarshal(s.Type, &one)
			types = []string{one}
		}
		set.Type = strings.Join(types, " . ")
		for _, e := range s.Elem {
			el, err := parseElement(e)
			if err != nil {
				return nil, err
			}
			set.Elements = append(set.Elements, el)
		}
		sets = append(sets, set)
	}
	sort.Slice(sets, func(i, j int) bool {
		return setKey(sets[i].Family, sets[i].Table, sets[i].Name) < setKey(sets[j].Family, sets[j].Table, sets[j].Name)
	})
	return sets, nil
}

func parseElement(raw json.RawMessage) (Element, error) {
	var wrapped struct {
		Elem *struct {
			Val     json.RawMessage `json:"val"`
			Timeout int64           `json:"timeout"`
			Expires int64           `json:"expires"`
		} `json:"elem"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Elem != nil {
		v, err := formatValue(wrapped.Elem.Val)
		return Element{Value: v, Timeout: time.Duration(wrapped.Elem.Timeout) * time.Second, Expires: time.Duration(wrapped.Elem.Expires) * time.Second}, err
	}
	v, err := formatValue(raw)
	return Element{Value: v}, err
}

// formatValue renders one of nft's JSON values in nft syntax.
func formatValue(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String(), nil
	}
	var obj struct {
		Range  []json.RawMessage `json:"range"`
		Prefix *struct {
			Addr string `json:"addr"`
			Len  int    `json:"len"`
		} `json:"prefix"`
		Concat []json.RawMessage `json:"concat"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("a set element nft lists as %s", raw)
	}
	switch {
	case obj.Prefix != nil:
		return obj.Prefix.Addr + "/" + strconv.Itoa(obj.Prefix.Len), nil
	case len(obj.Range) == 2:
		a, err := formatValue(obj.Range[0])
		if err != nil {
			return "", err
		}
		b, err := formatValue(obj.Range[1])
		return a + "-" + b, err
	case len(obj.Concat) > 0:
		parts := make([]string, len(obj.Concat))
		for i, c := range obj.Concat {
			p, err := formatValue(c)
			if err != nil {
				return "", err
			}
			parts[i] = p
		}
		return strings.Join(parts, " . "), nil
	}
	return "", fmt.Errorf("a set element nft lists as %s", raw)
}

var (
	namePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_\-]{0,63}$`)
	elementPattern = regexp.MustCompile(`^[A-Za-z0-9:._/\- ]{1,200}$`)
	families       = []string{"ip", "ip6", "inet", "arp", "bridge", "netdev"}
)

// ValidSetRef checks a family, table and set name.
func ValidSetRef(family, table, set string) error {
	if !slices.Contains(families, family) {
		return fmt.Errorf("unknown family %q", family)
	}
	if !namePattern.MatchString(table) || !namePattern.MatchString(set) {
		return fmt.Errorf("invalid table or set name")
	}
	return nil
}

// ValidElement checks an element's text: what an element can be, and
// nothing that could end the element list and start another command.
func ValidElement(v string) error {
	if !elementPattern.MatchString(v) || strings.TrimSpace(v) != v {
		return fmt.Errorf("invalid set element %q", v)
	}
	return nil
}

// UpdateSet adds and deletes elements of a live set. Added elements
// without a timeout are kept: added back after every ruleset apply.
func (m *Manager) UpdateSet(family, table, set string, add []Element, del []string) error {
	if !Available() {
		return ErrNotAvailable
	}
	if err := ValidSetRef(family, table, set); err != nil {
		return err
	}
	if len(add) == 0 && len(del) == 0 {
		return nil
	}
	var script strings.Builder
	ref := family + " " + table + " " + set
	if len(add) > 0 {
		parts := make([]string, len(add))
		for i, e := range add {
			if err := ValidElement(e.Value); err != nil {
				return err
			}
			parts[i] = e.Value
			if e.Timeout > 0 {
				parts[i] += fmt.Sprintf(" timeout %ds", int64(e.Timeout/time.Second))
			}
		}
		fmt.Fprintf(&script, "add element %s { %s }\n", ref, strings.Join(parts, ", "))
	}
	if len(del) > 0 {
		for _, v := range del {
			if err := ValidElement(v); err != nil {
				return err
			}
		}
		fmt.Fprintf(&script, "delete element %s { %s }\n", ref, strings.Join(del, ", "))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, stderr, err := run(script.String(), "-f", "-"); err != nil {
		return fmt.Errorf("%s", strings.Join(errorLines(stderr, ""), "; "))
	}
	kept := loadElements()
	k := setKey(family, table, set)
	for _, e := range add {
		if e.Timeout == 0 && !slices.Contains(kept[k], e.Value) {
			kept[k] = append(kept[k], e.Value)
		}
	}
	kept[k] = slices.DeleteFunc(kept[k], func(v string) bool { return slices.Contains(del, v) })
	if len(kept[k]) == 0 {
		delete(kept, k)
	}
	return saveElements(kept)
}

func setKey(family, table, set string) string { return family + " " + table + " " + set }

// restoreElements adds the kept elements back into the live sets, set by
// set - sets the ruleset doesn't have are skipped (kept until a ruleset
// without them is confirmed), one that refuses them is logged. Called
// with mu held, after a ruleset is applied.
func (m *Manager) restoreElements() {
	kept := loadElements()
	if len(kept) == 0 {
		return
	}
	live := map[string]bool{}
	if out, _, err := run("", "-j", "list", "sets"); err == nil {
		if sets, err := parseSets([]byte(out)); err == nil {
			for _, s := range sets {
				live[setKey(s.Family, s.Table, s.Name)] = true
			}
		}
	}
	keys := make([]string, 0, len(kept))
	for k := range kept {
		if live[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		script := fmt.Sprintf("add element %s { %s }\n", k, strings.Join(kept[k], ", "))
		if _, stderr, err := run(script, "-f", "-"); err != nil {
			log.Printf("firewall: kept elements of %s not added back: %s", k, strings.TrimSpace(stderr))
		}
	}
}

// pruneElements forgets kept elements of sets the confirmed ruleset no
// longer has. Called with mu held.
func (m *Manager) pruneElements() {
	kept := loadElements()
	if len(kept) == 0 {
		return
	}
	out, _, err := run("", "-j", "list", "sets")
	if err != nil {
		return
	}
	sets, err := parseSets([]byte(out))
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, s := range sets {
		live[setKey(s.Family, s.Table, s.Name)] = true
	}
	for k := range kept {
		if !live[k] {
			delete(kept, k)
		}
	}
	_ = saveElements(kept)
}

func loadElements() map[string][]string {
	kept := map[string][]string{}
	data, err := os.ReadFile(filepath.Join(Dir, setsFile))
	if err != nil {
		return kept
	}
	if err := json.Unmarshal(data, &kept); err != nil {
		log.Printf("firewall: %s: %v", setsFile, err)
		return map[string][]string{}
	}
	for k, vs := range kept {
		vs = slices.DeleteFunc(vs, func(v string) bool { return ValidElement(v) != nil })
		if f := strings.Fields(k); len(f) != 3 || ValidSetRef(f[0], f[1], f[2]) != nil || len(vs) == 0 {
			delete(kept, k)
		} else {
			kept[k] = vs
		}
	}
	return kept
}

func saveElements(kept map[string][]string) error {
	data, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return err
	}
	return saveFile(setsFile, append(data, '\n'))
}

func saveFile(name string, data []byte) error {
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(Dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(Dir, name))
}
