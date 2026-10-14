package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// A policy's Sysctl: the node's kernel parameters follow its size and
// load. The node suggests values (internal/sysctl's rules, its System ›
// Sysctl page's suggestions) - from its memory as it is now, what it
// observed; auto-scaling applies them the way an operator would: on
// trial, confirmed over a connection of its own once the node and
// HAProxy still answer (the node takes them back by itself otherwise).
//
//   - Never a parameter someone else confirmed: the operator keeps it.
//   - A parameter at most once every sysctlKeyEvery - the rules that
//     follow the memory aside, which follow each memory step: two rules
//     taking turns on one key would otherwise flap.
//   - Back at its size, what followed the memory goes back to what it
//     was, unless someone changed it since.
//
// Observing, each change it would make is journaled, once.

const (
	sysctlEvery    = 10 * time.Minute
	sysctlTimeout  = time.Minute
	sysctlKeyEvery = 6 * time.Hour
	// sysctlTrial is the trial's length: the node takes the values back
	// by itself unless confirmed within it.
	sysctlTrial = 120
)

// followsMemory reports whether a rule is one of those that follow the
// memory plugged in or taken out.
func followsMemory(rule string) bool { return strings.HasSuffix(rule, ".follow_memory") }

// sysctlChange is one parameter auto-scaling sets.
type sysctlChange struct {
	name, from, to, rule string
	back                 bool // back to what it was before auto-scaling
}

func (c sysctlChange) String() string {
	why := c.rule
	if c.back {
		why = "back at its size"
	}
	return fmt.Sprintf("%s %s → %s (%s)", c.name, orNone(c.from), orNone(c.to), why)
}

func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

// humanSet is the parameters someone other than auto-scaling confirmed,
// from the node's history.
func humanSet(history []*janusv1alpha1.SysctlHistoryEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range history {
		if e.GetAction() != "confirm" || e.GetActor().GetName() == nodeproxy.AutoScaling.Name {
			continue
		}
		for _, c := range e.GetChanges() {
			out[c.GetName()] = true
		}
	}
	return out
}

// planSysctl is what auto-scaling would change on a node: its
// suggestions, and what goes back once the machine is at its size.
func planSysctl(list *janusv1alpha1.SysctlListResponse, human map[string]bool, mine map[string]machines.SysctlChange, atSize bool, now time.Time) []sysctlChange {
	live := map[string]string{}
	var out []sysctlChange
	planned := map[string]bool{}
	for _, p := range list.GetParameters() {
		if p.GetClass() != janusv1alpha1.SysctlClass_SYSCTL_CLASS_EDITABLE || p.GetMissing() != "" {
			continue
		}
		live[p.GetName()] = p.GetValue()
		rec := p.GetRecommendation()
		if rec == nil || human[p.GetName()] || rec.GetValue() == p.GetValue() {
			continue
		}
		if prev, ok := mine[p.GetName()]; ok && !followsMemory(rec.GetRuleId()) && now.Sub(prev.At) < sysctlKeyEvery {
			continue
		}
		out = append(out, sysctlChange{name: p.GetName(), from: p.GetValue(), to: rec.GetValue(), rule: rec.GetRuleId()})
		planned[p.GetName()] = true
	}
	if atSize {
		for _, name := range slices.Sorted(maps.Keys(mine)) {
			c := mine[name]
			if followsMemory(c.Rule) && !planned[name] && !human[name] && live[name] == c.After && c.Before != c.After {
				out = append(out, sysctlChange{name: name, from: c.After, to: c.Before, rule: c.Rule, back: true})
			}
		}
	}
	return out
}

// sysctl makes the node's kernel parameters follow (see above), right
// after a memory step and every sysctlEvery. It returns what to say on
// the machine, "" when nothing.
func (s *autoscaler) sysctl(ctx context.Context, m *machines.Machine, node *store.Node, sm *scaledMachine, st *machines.AutoscaleState, memoryStep bool, now time.Time) string {
	if !memoryStep && now.Sub(sm.sysctlAt) < sysctlEvery {
		return ""
	}
	sm.sysctlAt = now
	ctx, cancel := context.WithTimeout(nodeproxy.WithUser(ctx, nodeproxy.AutoScaling), sysctlTimeout)
	defer cancel()
	list, err := nodes.SysctlList(ctx, node)
	if err != nil {
		log.Printf("machine %s: auto-scaling: the node's kernel parameters: %v", m.ID, err)
		return ""
	}
	if !list.GetManaged() || list.GetTrial() != nil {
		return "" // a trial under way - someone's: next time
	}
	history, err := nodes.SysctlHistory(ctx, node, 200)
	if err != nil {
		log.Printf("machine %s: auto-scaling: the node's sysctl history: %v", m.ID, err)
		return ""
	}
	atSize := m.Size != nil && m.Size.MemoryMiB == m.Spec.MemoryMiB
	changes := planSysctl(list, humanSet(history), st.Sysctl, atSize, now)
	if len(changes) == 0 {
		return ""
	}
	var words []string
	for _, c := range changes {
		words = append(words, c.String())
	}
	entry := machines.JournalEntry{Time: now.UTC(), Mode: m.Spec.Autoscale.Mode, Action: "sysctl", From: st.Size, To: st.Size, Reason: strings.Join(words, "; ")}

	if m.Spec.Autoscale.Mode != machines.AutoscaleOn {
		// Observing: what it would apply, once each.
		if sm.sysctlSeen == nil {
			sm.sysctlSeen = map[string]bool{}
		}
		var fresh []string
		for _, c := range changes {
			if k := c.name + "=" + c.to; !sm.sysctlSeen[k] {
				sm.sysctlSeen[k] = true
				fresh = append(fresh, c.String())
			}
		}
		if len(fresh) > 0 {
			entry.Reason = strings.Join(fresh, "; ")
			if err := s.a.machines.AppendJournal(m.ID, entry); err != nil {
				log.Printf("machine %s: auto-scaling journal: %v", m.ID, err)
			}
		}
		return ""
	}

	fail := func(format string, args ...any) string {
		entry.Error = fmt.Sprintf(format, args...)
		if err := s.a.machines.AppendJournal(m.ID, entry); err != nil {
			log.Printf("machine %s: auto-scaling journal: %v", m.ID, err)
		}
		return "kernel parameters not changed: " + entry.Error
	}
	req := &janusv1alpha1.SysctlApplyRequest{ReloadHaproxy: true, ConfirmTimeoutSeconds: sysctlTrial}
	for _, c := range changes {
		req.Changes = append(req.Changes, &janusv1alpha1.SysctlChangeRequest{Name: c.name, Value: c.to})
	}
	resp, err := nodes.SysctlApply(ctx, node, req)
	if err != nil {
		return fail("%v", err)
	}
	if !resp.GetAccepted() {
		var why []string
		for _, e := range resp.GetErrors() {
			why = append(why, strings.TrimPrefix(e.GetName()+": "+e.GetMessage(), ": "))
		}
		return fail("the node refused them: %s", strings.Join(why, "; "))
	}
	// Still reachable, HAProxy still answering: confirmed. Otherwise the
	// node puts them back by itself when the trial ends.
	load, err := nodes.Load(ctx, node)
	if err != nil || load.IdlePercent < 0 {
		return fail("on trial, but the node or HAProxy stopped answering - it takes them back within %d s", sysctlTrial)
	}
	if err := nodes.SysctlConfirm(ctx, node); err != nil {
		return fail("on trial, not confirmed (%v) - the node takes them back within %d s", err, sysctlTrial)
	}
	if st.Sysctl == nil {
		st.Sysctl = map[string]machines.SysctlChange{}
	}
	for _, c := range changes {
		if c.back {
			delete(st.Sysctl, c.name)
			continue
		}
		before := c.from
		if prev, ok := st.Sysctl[c.name]; ok {
			before = prev.Before // what it was before auto-scaling touched it
		}
		st.Sysctl[c.name] = machines.SysctlChange{Before: before, After: c.to, Rule: c.rule, At: now.UTC()}
	}
	if err := s.a.machines.AppendJournal(m.ID, entry); err != nil {
		log.Printf("machine %s: auto-scaling journal: %v", m.ID, err)
	}
	return "kernel parameters applied: " + entry.Reason
}
