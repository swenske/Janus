package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
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
		fmt.Fprintln(os.Stderr, "usage: janusctl system log-forwarding [status|get|set FILE|test [-file FILE] TARGET]")
		os.Exit(2)
	}
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
	tw := table("TARGET", "STATE", "SENT", "DROPPED", "QUEUED", "LAST SENT", "WHERE")
	for _, st := range resp.GetTargets() {
		last := "-"
		if ns := st.GetLastSentUnixNs(); ns > 0 {
			last = time.Since(time.Unix(0, ns)).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\n", st.GetName(), st.GetState(), st.GetSent(), st.GetDropped(), st.GetQueued(), last, where[st.GetName()])
	}
	tw.Flush()
	for _, st := range resp.GetTargets() {
		if st.GetLastError() != "" {
			fmt.Printf("%s: %s\n", st.GetName(), st.GetLastError())
		}
	}
}
