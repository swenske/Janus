package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// runSysctl is `system sysctl`: the node's kernel parameters
// (docs/guide/kernel-tuning.md) - the whitelist HAProxy depends on,
// changed on trial, and the CIS benchmark's, read-only.
func runSysctl(conn *grpc.ClientConn, endpoint string, redial redialer, args []string) {
	client := janusv1alpha1.NewSystemServiceClient(conn)
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("system sysctl list", flag.ExitOnError)
		cis := fs.Bool("cis", false, "every CIS control too")
		_ = fs.Parse(args[1:])
		c, cancel := ctx()
		defer cancel()
		resp, err := client.SysctlList(c, &emptypb.Empty{})
		check("SysctlList", err)
		printSysctlList(resp, *cis)

	case "get":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system sysctl get NAME")
			os.Exit(2)
		}
		c, cancel := ctx()
		defer cancel()
		resp, err := client.SysctlList(c, &emptypb.Empty{})
		check("SysctlList", err)
		for _, p := range resp.GetParameters() {
			if p.GetName() == args[1] {
				printSysctlParameter(p)
				return
			}
		}
		for _, ctl := range resp.GetCis().GetControls() {
			if ctl.GetKey() == args[1] {
				fmt.Printf("%s = %s\nCIS %s, %s: %s - locked, never changed\n", ctl.GetKey(), ctl.GetValue(), strings.Join(ctl.GetIds(), ", "), resp.GetCis().GetBenchmark(), compliance(ctl.GetCompliant()))
				return
			}
		}
		fmt.Fprintf(os.Stderr, "%s: not a parameter the node shows\n", args[1])
		os.Exit(1)

	case "set":
		fs := flag.NewFlagSet("system sysctl set", flag.ExitOnError)
		timeout := fs.Duration("timeout", 10*time.Minute, sysctlTimeoutHelp)
		noConfirm := fs.Bool("no-confirm", false, sysctlNoConfirmHelp)
		noReload := fs.Bool("no-reload", false, sysctlNoReloadHelp)
		parseAnywhere(fs, args[1:])
		req := &janusv1alpha1.SysctlApplyRequest{}
		for _, a := range fs.Args() {
			name, value, ok := strings.Cut(a, "=")
			if !ok {
				fmt.Fprintf(os.Stderr, "%q: NAME=VALUE expected\n", a)
				os.Exit(2)
			}
			req.Changes = append(req.Changes, &janusv1alpha1.SysctlChangeRequest{Name: strings.TrimSpace(name), Value: value})
		}
		if len(req.Changes) == 0 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system sysctl set [-timeout 10m] [-no-confirm] [-no-reload] NAME=VALUE...")
			os.Exit(2)
		}
		runSysctlApply(client, endpoint, redial, req, *timeout, *noConfirm, *noReload)

	case "reset":
		fs := flag.NewFlagSet("system sysctl reset", flag.ExitOnError)
		timeout := fs.Duration("timeout", 10*time.Minute, sysctlTimeoutHelp)
		noConfirm := fs.Bool("no-confirm", false, sysctlNoConfirmHelp)
		noReload := fs.Bool("no-reload", false, sysctlNoReloadHelp)
		all := fs.Bool("all", false, "every parameter back to its default")
		parseAnywhere(fs, args[1:])
		req := &janusv1alpha1.SysctlApplyRequest{ResetAll: *all}
		for _, name := range fs.Args() {
			req.Changes = append(req.Changes, &janusv1alpha1.SysctlChangeRequest{Name: name, ToDefault: true})
		}
		if len(req.Changes) == 0 && !*all {
			fmt.Fprintln(os.Stderr, "usage: janusctl system sysctl reset [-timeout 10m] [-no-confirm] [-no-reload] NAME... | -all")
			os.Exit(2)
		}
		runSysctlApply(client, endpoint, redial, req, *timeout, *noConfirm, *noReload)

	case "confirm":
		trial, err := sysctlConfirmOver(endpoint, redial)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("confirmed - saved: the node applies these values at every boot")
		printSysctlChanges(trial.GetChanges())

	case "cancel":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.SysctlCancel(c, &emptypb.Empty{})
		check("SysctlCancel", err)
		fmt.Println("cancelled - the values from before the trial are back")
		printSysctlChanges(resp.GetTrial().GetChanges())

	case "history":
		fs := flag.NewFlagSet("system sysctl history", flag.ExitOnError)
		n := fs.Uint("n", 20, "the newest N changes")
		_ = fs.Parse(args[1:])
		c, cancel := ctx()
		defer cancel()
		resp, err := client.SysctlHistory(c, &janusv1alpha1.SysctlHistoryRequest{Limit: uint32(*n)})
		check("SysctlHistory", err)
		for _, e := range resp.GetEntries() {
			fmt.Printf("%s  %-12s %s\n", time.Unix(e.GetTimeUnix(), 0).Format(time.RFC3339), e.GetAction(), actorString(e.GetActor()))
			for _, ch := range e.GetChanges() {
				fmt.Printf("    %s: %s → %s\n", ch.GetName(), shown(ch.GetOldValue()), shown(ch.GetNewValue()))
			}
			if d := e.GetDetail(); d != "" {
				fmt.Printf("    %s\n", localTimes(d))
			}
		}

	case "observed":
		fs := flag.NewFlagSet("system sysctl observed", flag.ExitOnError)
		verbose := fs.Bool("v", false, "what each signal measures, and when an hour counts")
		_ = fs.Parse(args[1:])
		c, cancel := ctx()
		defer cancel()
		resp, err := client.SysctlList(c, &emptypb.Empty{})
		check("SysctlList", err)
		printSysctlObserved(resp.GetObservation(), *verbose)

	default:
		fmt.Fprintf(os.Stderr, "janusctl system sysctl: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

const (
	sysctlTimeoutHelp   = "how long the node waits for the confirmation before putting the values back (1m-1h)"
	sysctlNoConfirmHelp = "leave the values on trial: confirm them with `system sysctl confirm`, or cancel, before the timeout"
	sysctlNoReloadHelp  = "don't reload HAProxy for the parameters it only reads when it opens its listeners"
)

// runSysctlApply puts set's or reset's changes on trial, then confirms
// them over a new connection - unless noConfirm leaves them on trial, to
// look at first.
func runSysctlApply(client janusv1alpha1.SystemServiceClient, endpoint string, redial redialer, req *janusv1alpha1.SysctlApplyRequest, timeout time.Duration, noConfirm, noReload bool) {
	req.ConfirmTimeoutSeconds = uint32(timeout.Seconds())
	req.ReloadHaproxy = !noReload
	c, cancel := ctx()
	defer cancel()
	resp, err := client.SysctlApply(c, req)
	check("SysctlApply", err)
	if !resp.GetAccepted() {
		fmt.Fprintln(os.Stderr, "refused - nothing changed:")
		for _, e := range resp.GetErrors() {
			if e.GetName() == "" {
				fmt.Fprintln(os.Stderr, "  "+e.GetMessage())
			} else {
				fmt.Fprintf(os.Stderr, "  %s: %s\n", e.GetName(), e.GetMessage())
			}
		}
		os.Exit(1)
	}
	trial := resp.GetTrial()
	revertAt := time.Unix(trial.GetRevertAtUnix(), 0)
	fmt.Printf("on trial - the values go back at %s unless confirmed\n", revertAt.Format(time.RFC3339))
	printSysctlChanges(trial.GetChanges())
	if trial.GetHaproxyReloaded() {
		fmt.Println("HAProxy reloaded to open its listeners with them")
	}
	if noConfirm {
		return
	}
	for time.Now().Before(revertAt) {
		if _, err := sysctlConfirmOver(endpoint, redial); err == nil {
			fmt.Println("confirmed over a new connection - saved: the node applies these values at every boot")
			return
		} else {
			fmt.Fprintf(os.Stderr, "confirm: %v - retrying\n", err)
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Fprintln(os.Stderr, "couldn't confirm before the timeout: the node puts the previous values back by itself")
	os.Exit(1)
}

// sysctlConfirmOver confirms over a connection of its own, opened after
// the apply - as the node requires.
func sysctlConfirmOver(endpoint string, redial redialer) (*janusv1alpha1.SysctlTrial, error) {
	conn, err := redial(endpoint)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := janusv1alpha1.NewSystemServiceClient(conn).SysctlConfirm(c, &emptypb.Empty{})
	if err != nil {
		return nil, errors.New(status.Convert(err).Message())
	}
	return resp.GetTrial(), nil
}

func printSysctlList(resp *janusv1alpha1.SysctlListResponse, withCIS bool) {
	if !resp.GetManaged() {
		fmt.Println("(this janusd doesn't run a Janus node: nothing can be changed)")
	}
	tw := table("PARAMETER", "VALUE", "DEFAULT", "STATE", "SUGGESTED")
	var readOnly []*janusv1alpha1.SysctlParameter
	suggested := 0
	for _, p := range resp.GetParameters() {
		if p.GetClass() != janusv1alpha1.SysctlClass_SYSCTL_CLASS_EDITABLE {
			readOnly = append(readOnly, p)
			continue
		}
		suggestion := ""
		if r := p.GetRecommendation(); r != nil {
			suggestion = r.GetValue()
			suggested++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.GetName(), valueOrMissing(p), shown(p.GetDefaultValue()), sysctlState(p), suggestion)
	}
	tw.Flush()
	if suggested > 0 {
		fmt.Println("Suggested values are never applied by themselves - `janusctl system sysctl get NAME` says why.")
	}
	if len(readOnly) > 0 {
		fmt.Println()
		tw = table("READ-ONLY", "VALUE", "WHY")
		for _, p := range readOnly {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.GetName(), valueOrMissing(p), p.GetWhy())
		}
		tw.Flush()
	}
	cis := resp.GetCis()
	fmt.Printf("\n%s (%s): %d/%d controls %s\n", cis.GetBenchmark(), cis.GetProfile(), cis.GetCompliant(), len(cis.GetControls()), compliance(int(cis.GetCompliant()) == len(cis.GetControls())))
	if withCIS {
		tw = table("CONTROL", "KEY", "VALUE", "WANT", "STATE")
		for _, ctl := range cis.GetControls() {
			state := compliance(ctl.GetCompliant())
			if !ctl.GetCompliant() {
				state += ": " + strings.Join(ctl.GetProblems(), "; ")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", strings.Join(ctl.GetIds(), ", "), ctl.GetKey(), ctl.GetValue(), strings.Join(ctl.GetWant(), " or "), state)
		}
		tw.Flush()
	}
	if t := resp.GetTrial(); t != nil {
		fmt.Printf("\nOn trial, by %s - the values go back at %s unless confirmed:\n", actorString(t.GetActor()), time.Unix(t.GetRevertAtUnix(), 0).Format(time.RFC3339))
		printSysctlChanges(t.GetChanges())
	}
}

func printSysctlParameter(p *janusv1alpha1.SysctlParameter) {
	fmt.Printf("%s = %s\n", p.GetName(), valueOrMissing(p))
	fmt.Println(p.GetSummary())
	switch p.GetClass() {
	case janusv1alpha1.SysctlClass_SYSCTL_CLASS_EDITABLE:
		def := shown(p.GetDefaultValue())
		if p.GetDefaultDynamic() {
			def += " (the kernel's own, at boot)"
		}
		fmt.Printf("Default:  %s\n", def)
		if p.GetSaved() {
			fmt.Printf("Saved:    %s\n", shown(p.GetSavedValue()))
		}
		fmt.Printf("State:    %s\n", sysctlState(p))
		fmt.Printf("Allowed:  %s\n", sysctlAllowed(p))
		fmt.Printf("Applies:  %s\n", map[janusv1alpha1.SysctlApplies]string{
			janusv1alpha1.SysctlApplies_SYSCTL_APPLIES_IMMEDIATELY:     "at once",
			janusv1alpha1.SysctlApplies_SYSCTL_APPLIES_NEW_CONNECTIONS: "to new connections",
			janusv1alpha1.SysctlApplies_SYSCTL_APPLIES_HAPROXY_RELOAD:  "when HAProxy opens its listeners: at its next reload",
		}[p.GetApplies()])
		fmt.Printf("HAProxy:  %s\n", p.GetEffect())
		fmt.Printf("Risk:     %s\n", p.GetRisk())
	default:
		fmt.Printf("Not changeable: %s\n", p.GetWhy())
	}
	for _, w := range p.GetWarnings() {
		fmt.Printf("Warning:  %s\n", w)
	}
	for _, s := range p.GetSources() {
		fmt.Printf("Source:   %s - %s\n", s.GetTitle(), s.GetUrl())
	}
	if r := p.GetRecommendation(); r != nil {
		fmt.Printf("\nSuggested: %s - never applied by itself (rule %s)\n", r.GetValue(), r.GetRuleId())
		fmt.Printf("  %s\n", r.GetRule())
		for _, m := range r.GetMeasured() {
			if m.GetWindow() != "" {
				fmt.Printf("  %s: %s - %s\n", m.GetName(), m.GetValue(), m.GetWindow())
			} else {
				fmt.Printf("  %s: %s\n", m.GetName(), m.GetValue())
			}
		}
		for _, s := range r.GetSources() {
			fmt.Printf("  Source: %s - %s\n", s.GetTitle(), s.GetUrl())
		}
		fmt.Printf("To test it: janusctl system sysctl set '%s=%s'\n", p.GetName(), r.GetValue())
	}
}

// printSysctlObserved prints what the node observed for its suggestions.
func printSysctlObserved(o *janusv1alpha1.SysctlObservation, verbose bool) {
	if o == nil {
		fmt.Println("(this janusd doesn't run a Janus node: it observes nothing)")
		return
	}
	fmt.Printf("Observed since %s - a signal counts once seen in %d different hours of the last %d days.\n", time.Unix(o.GetSinceUnix(), 0).Format(time.RFC3339), o.GetMinHours(), o.GetWindowHours()/24)
	tw := table("SIGNAL", "SEEN IN", "AT ITS HIGHEST", "LAST SEEN")
	for _, s := range o.GetSignals() {
		last := "never"
		if s.GetLastSeenUnix() > 0 {
			last = time.Unix(s.GetLastSeenUnix(), 0).Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%d h\t%s\t%s\n", s.GetTitle(), s.GetHours(), shown(s.GetPeak()), last)
	}
	tw.Flush()
	if verbose {
		for _, s := range o.GetSignals() {
			fmt.Printf("\n%s (%s)\n  %s\n  Counts when: %s\n", s.GetTitle(), s.GetId(), s.GetMeasure(), s.GetSeen())
		}
	}
}

// sysctlAllowed describes the values a parameter may take.
func sysctlAllowed(p *janusv1alpha1.SysctlParameter) string {
	var parts []string
	switch p.GetKind() {
	case janusv1alpha1.SysctlKind_SYSCTL_KIND_ENUM:
		for _, v := range p.GetAllowed() {
			parts = append(parts, fmt.Sprint(v))
		}
		return strings.Join(parts, ", ")
	case janusv1alpha1.SysctlKind_SYSCTL_KIND_PORTS:
		b := p.GetBounds()[0]
		return fmt.Sprintf("up to %d ports or ranges between %d and %d, comma-separated", p.GetMaxItems(), b.GetMin(), b.GetMax())
	}
	for _, b := range p.GetBounds() {
		parts = append(parts, fmt.Sprintf("%d-%d", b.GetMin(), b.GetMax()))
	}
	s := strings.Join(parts, " ")
	if p.GetUnit() != "" {
		s += " " + p.GetUnit()
	}
	return s
}

func sysctlState(p *janusv1alpha1.SysctlParameter) string {
	switch {
	case p.GetMissing() != "":
		return "absent"
	case p.GetOnTrial():
		return "trial"
	case p.GetSaved():
		return "saved"
	case p.GetValue() != p.GetDefaultValue():
		return "differs"
	}
	return "default"
}

func printSysctlChanges(changes []*janusv1alpha1.SysctlChange) {
	for _, ch := range changes {
		note := ""
		if ch.GetToDefault() {
			note = " (default)"
		}
		fmt.Printf("  %s: %s → %s%s\n", ch.GetName(), shown(ch.GetOldValue()), shown(ch.GetNewValue()), note)
	}
}

func valueOrMissing(p *janusv1alpha1.SysctlParameter) string {
	if p.GetMissing() != "" {
		return "(" + p.GetMissing() + ")"
	}
	return shown(p.GetValue())
}

// shown makes an empty value visible.
func shown(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

func compliance(ok bool) string {
	if ok {
		return "compliant"
	}
	return "NOT compliant"
}

func actorString(a *janusv1alpha1.SysctlActor) string {
	s := a.GetName()
	if len(a.GetRoles()) > 0 {
		s += " (" + strings.Join(a.GetRoles(), ", ") + ")"
	}
	if a.GetVia() != "" {
		s += " via " + a.GetVia()
	}
	return s
}

// rfc3339 matches a timestamp a node wrote into a detail ("reverts at
// 2026-10-07T14:01:02Z unless confirmed").
var rfc3339 = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2})`)

// localTimes rewrites the timestamps in a node's detail in this machine's
// time zone, as every other time this command prints: the node writes
// them in UTC.
func localTimes(detail string) string {
	return rfc3339.ReplaceAllStringFunc(detail, func(ts string) string {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return ts
		}
		return t.Local().Format(time.RFC3339)
	})
}
