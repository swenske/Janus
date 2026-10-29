package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// runLogForwarding is janusctl system log-forwarding: where the node's
// logs go (docs/guide/log-forwarding.md).
func runLogForwarding(conn *grpc.ClientConn, args []string) {
	client := janusv1alpha1.NewSystemServiceClient(conn)
	sub := "status"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	c, cancel := ctx()
	defer cancel()
	switch sub {
	case "status":
		resp, err := client.LogForwardingGet(c, &emptypb.Empty{})
		check("LogForwardingGet", err)
		printLogForwarding(resp)

	case "get":
		resp, err := client.LogForwardingGet(c, &emptypb.Empty{})
		check("LogForwardingGet", err)
		out, err := protojson.Marshal(resp.GetConfig())
		check("marshal", err)
		// protojson's own spacing changes from run to run: indented here
		// so the same configuration prints the same.
		var b bytes.Buffer
		check("indent", json.Indent(&b, out, "", "  "))
		fmt.Println(b.String())

	case "set":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system log-forwarding set FILE (JSON, as get prints it; secrets left out keep their saved values)")
			os.Exit(2)
		}
		cfg := &janusv1alpha1.LogForwardingConfig{}
		data, err := os.ReadFile(args[0])
		check("read", err)
		check("parse "+args[0], protojson.Unmarshal(data, cfg))
		resp, err := client.LogForwardingSet(c, cfg)
		check("LogForwardingSet", err)
		printLogForwarding(resp)

	case "pause", "resume":
		if len(args) != 1 {
			fmt.Fprintf(os.Stderr, "usage: janusctl system log-forwarding %s TARGET\n", sub)
			os.Exit(2)
		}
		resp, changed, err := pauseTarget(c, client, args[0], sub == "pause")
		if _, rpc := status.FromError(err); err != nil && rpc {
			check("LogForwarding", err)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s%v\n", styleFor(os.Stderr).red("✖ "), err)
			os.Exit(1)
		}
		if !changed {
			fmt.Printf("%s: already %s\n", args[0], map[bool]string{true: "paused", false: "sending"}[sub == "pause"])
		}
		printLogForwarding(resp)

	case "test":
		fs := flag.NewFlagSet("system log-forwarding test", flag.ExitOnError)
		file := fs.String("file", "", "a configuration (JSON) to take the target from, rather than the saved one")
		_ = fs.Parse(args)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl system log-forwarding test [-file FILE] TARGET")
			os.Exit(2)
		}
		cfg := &janusv1alpha1.LogForwardingConfig{}
		if *file != "" {
			data, err := os.ReadFile(*file)
			check("read", err)
			check("parse "+*file, protojson.Unmarshal(data, cfg))
		} else {
			resp, err := client.LogForwardingGet(c, &emptypb.Empty{})
			check("LogForwardingGet", err)
			cfg = resp.GetConfig()
		}
		var target *janusv1alpha1.LogTarget
		for _, t := range cfg.GetTargets() {
			if t.GetName() == fs.Arg(0) {
				target = t
			}
		}
		if target == nil {
			fmt.Fprintf(os.Stderr, "janusctl: no target %q\n", fs.Arg(0))
			os.Exit(1)
		}
		resp, err := client.LogForwardingTest(c, target)
		check("LogForwardingTest", err)
		if !resp.GetOk() {
			fmt.Fprintf(os.Stderr, "%s%s: %s\n", styleFor(os.Stderr).red("✖ "), target.GetName(), resp.GetDetail())
			os.Exit(1)
		}
		fmt.Printf("%s: %s\n", target.GetName(), resp.GetDetail())

	default:
		fmt.Fprintln(os.Stderr, "usage: janusctl system log-forwarding [status|get|set FILE|pause TARGET|resume TARGET|test [-file FILE] TARGET]")
		os.Exit(2)
	}
}

// pauseTarget pauses or resumes the node's target name: its
// configuration read, the target flipped, sent back - without its
// secrets, which the node keeps (LogForwardingSet). changed is false
// when the target already was.
func pauseTarget(c context.Context, client janusv1alpha1.SystemServiceClient, name string, paused bool) (resp *janusv1alpha1.LogForwardingResponse, changed bool, err error) {
	resp, err = client.LogForwardingGet(c, &emptypb.Empty{})
	if err != nil {
		return nil, false, err
	}
	cfg := resp.GetConfig()
	if changed, err = setPaused(cfg, name, paused); err != nil || !changed {
		return resp, false, err
	}
	resp, err = client.LogForwardingSet(c, cfg)
	return resp, err == nil, err
}

// setPaused pauses or resumes the target name of cfg - a configuration
// as LogForwardingGet gives it. It says whether that changed anything: a
// target already paused (or sending) is left as it is. A configuration a
// Controller's profile set isn't changed here: the Controller would put
// it back (docs/guide/log-forwarding.md, "Pausing a target").
func setPaused(cfg *janusv1alpha1.LogForwardingConfig, name string, paused bool) (bool, error) {
	if m := cfg.GetManagedBy(); m != "" {
		by := strings.Join(strings.Fields(m)[:min(4, len(strings.Fields(m)))], " ")
		return false, fmt.Errorf("set by the Controller (%s): pause or resume the target on its profile, the Controller's Log forwarding page - it would undo a change made here", by)
	}
	var names []string
	for _, t := range cfg.GetTargets() {
		if t.GetName() == name {
			if t.GetDisabled() == paused {
				return false, nil
			}
			t.Disabled = paused
			return true, nil
		}
		names = append(names, t.GetName())
	}
	if len(names) == 0 {
		return false, fmt.Errorf("no target %q: the node sends its logs nowhere", name)
	}
	return false, fmt.Errorf("no target %q - the node's: %s", name, strings.Join(names, ", "))
}

func printLogForwarding(resp *janusv1alpha1.LogForwardingResponse) {
	cfg := resp.GetConfig()
	if len(cfg.GetTargets()) == 0 {
		fmt.Println("no target: the logs aren't sent anywhere")
		return
	}
	if cfg.GetManagedBy() != "" {
		fmt.Printf("managed by %s\n", cfg.GetManagedBy())
	}
	where := map[string]string{}
	for _, t := range cfg.GetTargets() {
		switch {
		case t.GetSyslog() != nil:
			where[t.GetName()] = t.GetSyslog().GetTransport() + "://" + t.GetSyslog().GetAddress()
		case t.GetElasticsearch() != nil:
			where[t.GetName()] = t.GetElasticsearch().GetUrl()
		}
		if len(t.GetSources()) > 0 {
			where[t.GetName()] += " (" + strings.Join(t.GetSources(), ",") + ")"
		}
	}
	// VIA is the interface the node routes the target's connection by
	// (an older node doesn't say: "-").
	tw := table("TARGET", "STATE", "SENT", "BYTES", "DROPPED", "QUEUED", "VIA", "LAST SENT", "WHERE")
	for _, st := range resp.GetTargets() {
		last := "-"
		if ns := st.GetLastSentUnixNs(); ns > 0 {
			// The node's clock may be a few seconds ahead of this one.
			last = "now"
			if d := time.Since(time.Unix(0, ns)).Round(time.Second); d > 0 {
				last = d.String() + " ago"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\t%d\t%s\t%s\t%s\n", st.GetName(), st.GetState(), st.GetSent(), humanBytes(st.GetBytesSent()),
			st.GetDropped(), st.GetQueued(), cmp.Or(st.GetInterface(), "-"), last, where[st.GetName()])
	}
	tw.Flush()
	for _, st := range resp.GetTargets() {
		if st.GetLastError() != "" {
			fmt.Printf("%s: %s\n", st.GetName(), st.GetLastError())
		}
	}
}
