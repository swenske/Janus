package sysctl

import (
	"cmp"
	"errors"
	"fmt"
	"log"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/events"
)

// How long values on trial wait for their confirmation.
const (
	DefaultTrial = 10 * time.Minute
	MinTrial     = time.Minute
	MaxTrial     = time.Hour
)

var (
	// ErrNotManaged: janusd doesn't run a Janus node (no -manage-host)
	// and changes no kernel parameter of the machine it runs on.
	ErrNotManaged = errors.New("kernel parameters are only changed on a Janus node (janusd -manage-host)")
	// ErrNoTrial is a confirmation or a cancel with nothing on trial.
	ErrNoTrial = errors.New("no kernel parameters are on trial")
	// ErrOldConnection is a confirmation over a connection opened before
	// the values were applied, which doesn't show new connections still
	// get through.
	ErrOldConnection = errors.New("confirm over a new connection: this one was opened before the values were applied, so it proves nothing")
)

// FieldError is one change refused, and why.
type FieldError struct{ Name, Message string }

// InvalidError is a request with refused changes - nothing applied.
type InvalidError struct{ Fields []FieldError }

func (e *InvalidError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		if f.Name == "" {
			parts[i] = f.Message
		} else {
			parts[i] = f.Name + ": " + f.Message
		}
	}
	return strings.Join(parts, "; ")
}

// Change is one parameter to change: to Value, or back to its default.
type Change struct {
	Name  string
	Value string
	Reset bool
}

// Request is a set of changes to try.
type Request struct {
	Changes  []Change
	ResetAll bool          // every Editable parameter back to its default
	Timeout  time.Duration // 0: DefaultTrial
	// ReloadHAProxy reloads HAProxy, if it runs, when a changed parameter
	// is one it reads when it opens its listeners - and again when the
	// trial is undone.
	ReloadHAProxy bool
	Actor         Actor
}

// Trial is what's on trial: each parameter's value before the trial (the
// one a revert puts back) and its value now.
type Trial struct {
	Started  time.Time // the latest apply: a confirmation must come later
	RevertAt time.Time
	Changes  []ChangeRecord
	Actor    Actor
	Reloaded bool // HAProxy was reloaded for it
}

type trial struct {
	Trial
	previous map[string]string
	target   map[string]string
	reset    map[string]bool
	timer    *time.Timer
}

// Manager changes the Editable parameters, one trial at a time. One per
// janusd.
type Manager struct {
	manage bool
	probes Probes
	reload func() error // reloads HAProxy if it runs; nil: never

	mu    sync.Mutex
	trial *trial
	now   func() time.Time
	obs   *Observer
}

// NewManager makes the manager. manage false keeps it read-only - janusd
// not running a Janus node; reload may be nil.
func NewManager(manage bool, probes Probes, reload func() error) *Manager {
	if probes == nil {
		probes = ProcProbes{Proc: "/proc"}
	}
	return &Manager{manage: manage, probes: probes, reload: reload, now: time.Now}
}

// Managed reports whether the manager may change anything.
func (m *Manager) Managed() bool { return m.manage }

// Observe makes o's signals the ground of the parameters' suggestions.
func (m *Manager) Observe(o *Observer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.obs = o
}

// Boot puts every Editable parameter back to its saved value or default,
// so that a trial a janusd restart interrupted doesn't outlive it, and
// audits the CIS benchmark - which init enforced: janusd can't write its
// keys. Nothing off a Janus node.
func (m *Manager) Boot() Audit {
	if !m.manage {
		return Audit{}
	}
	saved, defaults := Saved(), Defaults()
	for _, p := range EditableParams() {
		want, ok := saved[p.Name]
		if !ok {
			want = defaults[p.Name]
		}
		live, err := readValue(p.Path(Root))
		if err != nil || (p.Dynamic && want == "") || Normalize(p.Kind, live) == want {
			continue
		}
		if err := writeParam(p, want); err != nil {
			log.Printf("sysctl: %s: %v", p.Name, err)
			continue
		}
		log.Printf("sysctl: %s back to %s - left from an unconfirmed trial", p.Name, want)
	}
	a := AuditCIS(Root)
	if !a.OK() {
		log.Printf("sysctl: NOT COMPLIANT with %s: %s", Benchmark, strings.Join(a.Failures(), "; "))
		events.Publish("sysctl.cis_not_compliant", map[string]any{"failures": a.Failures()})
	}
	return a
}

// ParamState is a parameter as the node has it.
type ParamState struct {
	Param    *Param
	Value    string // normalized; "" with Missing
	Missing  string // why there's no value: the kernel hasn't the file
	Default  string // Editable only
	Saved    string // the saved value; "" when the default applies
	HasSaved bool
	OnTrial  bool
	Warnings []string
	// Recommendation is a value suggested for this node - never applied
	// by itself.
	Recommendation *Recommendation
}

// Snapshot is everything SysctlList shows.
type Snapshot struct {
	Managed bool
	Params  []ParamState
	CIS     Audit
	Trial   *Trial
	// Observation is what the node observed for its suggestions; nil
	// when nothing observes.
	Observation *Observation
}

// Snapshot reads every parameter of Catalog, the CIS benchmark and the
// trial.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	saved, defaults := Saved(), Defaults()
	live := liveEditable()
	e := &env{target: live, probes: m.probes}
	s := Snapshot{Managed: m.manage, CIS: AuditCIS(Root)}
	var metrics Metrics
	if m.obs != nil {
		metrics = m.obs.Metrics()
		metrics.Live = live
		o := m.obs.Observation()
		s.Observation = &o
	}
	for _, p := range Catalog {
		ps := ParamState{Param: p}
		v, err := readValue(p.Path(Root))
		if err != nil {
			ps.Missing = missingReason(err)
		} else {
			ps.Value = Normalize(p.Kind, v)
		}
		if p.Class == Editable {
			ps.Default = defaults[p.Name]
			ps.Saved, ps.HasSaved = saved[p.Name]
			if m.trial != nil {
				_, ps.OnTrial = m.trial.target[p.Name]
			}
			if p.warn != nil && ps.Missing == "" {
				ps.Warnings = p.warn(ps.Value, e)
			}
			if m.obs != nil && ps.Missing == "" {
				ps.Recommendation = m.recommend(p, metrics, live)
			}
		}
		s.Params = append(s.Params, ps)
	}
	if m.trial != nil {
		t := m.trial.Trial
		s.Trial = &t
	}
	return s
}

// recommend is p's suggestion, if the node would take it: the same
// checks as a change's, with the other parameters as they are.
func (m *Manager) recommend(p *Param, metrics Metrics, live map[string]string) *Recommendation {
	rec, ok := p.Recommend(metrics)
	if !ok {
		return nil
	}
	if p.check != nil {
		_, nums, err := p.Parse(rec.Value)
		target := maps.Clone(live)
		target[p.Name] = rec.Value
		if err != nil || p.check(nums, &env{target: target, probes: m.probes}) != nil {
			return nil
		}
	}
	return &rec
}

// liveEditable is every Editable parameter's live value, normalized - ""
// for one the kernel hasn't.
func liveEditable() map[string]string {
	out := map[string]string{}
	for _, p := range EditableParams() {
		if v, err := readValue(p.Path(Root)); err == nil {
			out[p.Name] = Normalize(p.Kind, v)
		}
	}
	return out
}

// Validate checks req's changes without applying anything: the same
// checks ApplyTrial makes.
func (m *Manager) Validate(req Request) ([]ChangeRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plan(req, Defaults(), liveEditable())
}

// plan resolves req into the changes to make - Old is the live value,
// New the one to write - checking each against the whitelist, its form
// and bounds, and the node's state with every change made.
func (m *Manager) plan(req Request, defaults, live map[string]string) ([]ChangeRecord, error) {
	changes := req.Changes
	if req.ResetAll {
		if len(changes) > 0 {
			return nil, &InvalidError{[]FieldError{{Message: "a reset of every parameter takes no other change"}}}
		}
		// Only what differs: the others are where a reset would put them.
		for _, p := range EditableParams() {
			if v, ok := live[p.Name]; ok && v != defaults[p.Name] {
				changes = append(changes, Change{Name: p.Name, Reset: true})
			}
		}
		if len(changes) == 0 {
			return nil, &InvalidError{[]FieldError{{Message: "every parameter is already at its default"}}}
		}
	}
	if len(changes) == 0 {
		return nil, &InvalidError{[]FieldError{{Message: "no change given"}}}
	}

	var errs []FieldError
	type planned struct {
		p     *Param
		value string
		nums  []int64
		reset bool
	}
	var plans []planned
	seen := map[string]bool{}
	target := map[string]string{}
	for k, v := range live {
		target[k] = v
	}
	for _, c := range changes {
		p, reason := editable(c.Name)
		switch {
		case reason != "":
			errs = append(errs, FieldError{c.Name, reason})
			continue
		case seen[c.Name]:
			errs = append(errs, FieldError{c.Name, "given twice"})
			continue
		}
		seen[c.Name] = true
		if _, ok := live[c.Name]; !ok {
			errs = append(errs, FieldError{c.Name, "absent from this node's kernel"})
			continue
		}
		var value string
		var nums []int64
		if c.Reset {
			// A default is always acceptable as a form: a Dynamic one is
			// the kernel's own, which may lie outside the bounds offered
			// to an operator (a small machine's conntrack table).
			value = defaults[c.Name]
			if p.Dynamic && value == "" {
				errs = append(errs, FieldError{c.Name, "its default - the kernel's value at boot - isn't known"})
				continue
			}
			nums = ints(value)
		} else {
			var err error
			if value, nums, err = p.Parse(c.Value); err != nil {
				errs = append(errs, FieldError{c.Name, err.Error()})
				continue
			}
		}
		target[c.Name] = value
		plans = append(plans, planned{p, value, nums, c.Reset})
	}
	e := &env{target: target, probes: m.probes}
	var records []ChangeRecord
	for _, pl := range plans {
		if pl.p.check != nil {
			if err := pl.p.check(pl.nums, e); err != nil {
				errs = append(errs, FieldError{pl.p.Name, err.Error()})
				continue
			}
		}
		records = append(records, ChangeRecord{Name: pl.p.Name, Old: live[pl.p.Name], New: pl.value, Reset: pl.reset})
	}
	if len(errs) > 0 {
		return nil, &InvalidError{errs}
	}
	// Catalog's order, for writes and display.
	order := map[string]int{}
	for i, p := range Catalog {
		order[p.Name] = i
	}
	sort.Slice(records, func(i, j int) bool { return order[records[i].Name] < order[records[j].Name] })
	return records, nil
}

// writeParam writes value and reads it back: the kernel clamps or refuses
// some values without an error.
func writeParam(p *Param, value string) error {
	path := p.Path(Root)
	if err := write(path, value); err != nil {
		return err
	}
	got, err := readValue(path)
	if err != nil {
		return err
	}
	if got = Normalize(p.Kind, got); got != value {
		return fmt.Errorf("the kernel kept %q instead of %q", got, value)
	}
	return nil
}

// ApplyTrial validates req - again, just before writing - writes its
// values, checks the CIS benchmark still holds, reloads HAProxy if asked
// and needed, and leaves it all on trial: unless Confirm comes within the
// timeout, every value goes back to what it was before the trial. A trial
// already running grows by these changes, and its revert still goes back
// to the state before it began.
func (m *Manager) ApplyTrial(req Request) (*Trial, error) {
	if !m.manage {
		return nil, ErrNotManaged
	}
	timeout := cmp.Or(req.Timeout, DefaultTrial)
	if timeout < MinTrial || timeout > MaxTrial {
		return nil, &InvalidError{[]FieldError{{Message: fmt.Sprintf("the confirmation timeout must be between %s and %s", MinTrial, MaxTrial)}}}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	live := liveEditable()
	records, err := m.plan(req, Defaults(), live)
	if err != nil {
		return nil, err
	}
	if a := AuditCIS(Root); !a.OK() {
		return nil, fmt.Errorf("the node doesn't comply with %s right now, so nothing changes until it does - a reboot writes the benchmark's values again: %s", Benchmark, strings.Join(a.Failures(), "; "))
	}

	var written []ChangeRecord
	undo := func() {
		for _, r := range written {
			if err := writeParam(Lookup(r.Name), r.Old); err != nil {
				log.Printf("sysctl: put %s back to %s: %v", r.Name, r.Old, err)
			}
		}
	}
	for _, r := range records {
		if r.Old == r.New {
			continue
		}
		if err := writeParam(Lookup(r.Name), r.New); err != nil {
			undo()
			return nil, fmt.Errorf("%s: %w - nothing changed", r.Name, err)
		}
		written = append(written, r)
	}
	if a := AuditCIS(Root); !a.OK() {
		undo()
		return nil, fmt.Errorf("the CIS benchmark didn't hold with these values - nothing changed: %s", strings.Join(a.Failures(), "; "))
	}
	reloaded := false
	if req.ReloadHAProxy && m.reload != nil && needsReload(written) {
		if err := m.reload(); err != nil {
			undo()
			if rerr := m.reload(); rerr != nil {
				log.Printf("sysctl: reload HAProxy after undoing: %v", rerr)
			}
			return nil, fmt.Errorf("HAProxy didn't reload with these values - nothing changed: %w", err)
		}
		reloaded = true
	}

	now := m.now()
	t := &trial{previous: map[string]string{}, target: map[string]string{}, reset: map[string]bool{}}
	if old := m.trial; old != nil {
		old.timer.Stop()
		for k, v := range old.previous {
			t.previous[k] = v
		}
		for k, v := range old.target {
			t.target[k] = v
		}
		for k, v := range old.reset {
			t.reset[k] = v
		}
		reloaded = reloaded || old.Reloaded
	}
	for _, r := range records {
		if _, ok := t.previous[r.Name]; !ok {
			t.previous[r.Name] = r.Old
		}
		t.target[r.Name] = r.New
		t.reset[r.Name] = r.Reset
	}
	t.Started, t.RevertAt, t.Actor, t.Reloaded = now, now.Add(timeout), req.Actor, reloaded
	t.Changes = t.records()
	t.timer = time.AfterFunc(timeout, func() { m.revert(t) })
	m.trial = t

	log.Printf("sysctl: %s on trial, reverts at %s unless confirmed", describe(records), t.RevertAt.Format(time.RFC3339))
	m.record(Entry{Actor: req.Actor, Action: ActionTrial, Changes: records, Detail: "reverts at " + t.RevertAt.UTC().Format(time.RFC3339) + " unless confirmed"})
	events.Publish("sysctl.trial", map[string]any{"revert_at": t.RevertAt.Unix(), "changes": records})
	out := t.Trial
	return &out, nil
}

// records is the trial's changes, in Catalog's order.
func (t *trial) records() []ChangeRecord {
	var out []ChangeRecord
	for _, p := range EditableParams() {
		if v, ok := t.target[p.Name]; ok {
			out = append(out, ChangeRecord{Name: p.Name, Old: t.previous[p.Name], New: v, Reset: t.reset[p.Name]})
		}
	}
	return out
}

func needsReload(records []ChangeRecord) bool {
	for _, r := range records {
		if p := Lookup(r.Name); p != nil && p.Applies == HAProxyReload && r.Old != r.New {
			return true
		}
	}
	return false
}

func describe(records []ChangeRecord) string {
	parts := make([]string, len(records))
	for i, r := range records {
		parts[i] = fmt.Sprintf("%s=%s", r.Name, r.New)
	}
	return strings.Join(parts, " ")
}

func (m *Manager) record(e Entry) {
	if e.Time.IsZero() {
		e.Time = m.now().UTC()
	}
	if err := appendHistory(e); err != nil {
		log.Printf("sysctl: history: %v", err)
	}
}

// Confirm saves the values on trial. connOpened is when the connection
// carrying the confirmation was opened: after the latest apply. The
// values are checked once more first - the node may have changed during
// the trial (a new listener inside the source port range).
func (m *Manager) Confirm(connOpened time.Time, actor Actor) (*Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.trial
	if t == nil {
		return nil, ErrNoTrial
	}
	if !connOpened.After(t.Started) {
		return nil, ErrOldConnection
	}
	defaults := Defaults()
	if _, err := m.plan(Request{Changes: t.asChanges()}, defaults, liveEditable()); err != nil {
		return nil, fmt.Errorf("the values on trial no longer pass their checks - they stay on trial: %w", err)
	}
	saved := Saved()
	for name, v := range t.target {
		if v == defaults[name] {
			delete(saved, name)
		} else {
			saved[name] = v
		}
	}
	if err := saveValues(saved); err != nil {
		return nil, fmt.Errorf("the values stay on trial, they couldn't be saved: %w", err)
	}
	t.timer.Stop()
	m.trial = nil
	log.Printf("sysctl: %s confirmed and saved", describe(t.Changes))
	m.record(Entry{Actor: actor, Action: ActionConfirm, Changes: t.Changes})
	events.Publish("sysctl.confirmed", map[string]any{"changes": t.Changes})
	out := t.Trial
	return &out, nil
}

// asChanges is the trial's target as changes, the reset parameters as
// resets: a Dynamic default may lie outside an operator's bounds.
func (t *trial) asChanges() []Change {
	var out []Change
	for name, v := range t.target {
		if t.reset[name] {
			out = append(out, Change{Name: name, Reset: true})
		} else {
			out = append(out, Change{Name: name, Value: v})
		}
	}
	return out
}

// Cancel puts the values from before the trial back at once.
func (m *Manager) Cancel(actor Actor) (*Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.trial
	if t == nil {
		return nil, ErrNoTrial
	}
	t.timer.Stop()
	m.undo(t, actor, ActionCancel)
	out := t.Trial
	return &out, nil
}

func (m *Manager) revert(t *trial) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trial != t {
		return
	}
	m.undo(t, Actor{Name: "timeout"}, ActionRevert)
}

// undo writes the values from before the trial back, reloads HAProxy if
// the trial did, and records it. Called with mu held.
func (m *Manager) undo(t *trial, actor Actor, action string) {
	m.trial = nil
	var failed []string
	var back []ChangeRecord
	for _, r := range t.records() {
		back = append(back, ChangeRecord{Name: r.Name, Old: r.New, New: r.Old})
		if err := writeParam(Lookup(r.Name), r.Old); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.Name, err))
		}
	}
	if t.Reloaded && m.reload != nil {
		if err := m.reload(); err != nil {
			failed = append(failed, fmt.Sprintf("HAProxy reload: %v", err))
		}
	}
	e := Entry{Actor: actor, Action: action, Changes: back}
	if len(failed) > 0 {
		e.Detail = "not everything went back: " + strings.Join(failed, "; ")
		log.Printf("sysctl: %s: %s", action, e.Detail)
		events.Publish("sysctl.revert_failed", map[string]any{"errors": failed})
	} else {
		verb := map[string]string{ActionCancel: "cancelled", ActionRevert: "not confirmed in time"}[action]
		log.Printf("sysctl: trial %s - the previous values are back", verb)
	}
	m.record(e)
	events.Publish("sysctl."+map[string]string{ActionCancel: "cancelled", ActionRevert: "reverted"}[action], map[string]any{"changes": back})
}

// Trial is what's on trial, if anything.
func (m *Manager) Trial() *Trial {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trial == nil {
		return nil
	}
	t := m.trial.Trial
	return &t
}
