package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

func runFirewall(conn *grpc.ClientConn, endpoint string, redial redialer, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewNetworkServiceClient(conn)
	switch args[0] {
	case "status":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.FirewallList(c, &emptypb.Empty{})
		check("FirewallList", err)
		fmt.Println("State:     ", strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_")))
		if resp.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
			return
		}
		fmt.Println("Saved:     ", map[bool]string{true: "yes", false: "no - the node has no firewall"}[resp.GetConfigured()])
		if resp.GetTrialPending() {
			fmt.Printf("On trial:   reverts at %s unless confirmed\n", time.Unix(resp.GetTrialRevertAtUnix(), 0).Format(time.RFC3339))
		}
		fmt.Println("Live ruleset:")
		os.Stdout.Write(resp.GetRuleset())
	case "get":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.FirewallGetRuleset(c, &emptypb.Empty{})
		check("FirewallGetRuleset", err)
		if resp.GetIsDefault() {
			fmt.Fprintln(os.Stderr, "# no ruleset saved - the node has no firewall")
		}
		os.Stdout.Write(resp.GetRuleset())
	case "check":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: janusctl network firewall check FILE")
			os.Exit(2)
		}
		rs, err := os.ReadFile(args[1])
		check("read", err)
		c, cancel := ctx()
		defer cancel()
		resp, err := client.FirewallApplyRuleset(c, &janusv1alpha1.FirewallApplyRulesetRequest{Ruleset: rs, ValidateOnly: true})
		check("FirewallApplyRuleset", err)
		if !resp.GetAccepted() {
			printErrors(resp.GetErrors())
			os.Exit(1)
		}
		fmt.Println("valid")
	case "apply":
		runFirewallApply(client, endpoint, redial, args[1:])
	case "confirm":
		c, cancel := ctx()
		defer cancel()
		_, err := client.FirewallConfirm(c, &emptypb.Empty{})
		check("FirewallConfirm", err)
		fmt.Println("confirmed - the ruleset is saved")
	case "sets":
		c, cancel := ctx()
		defer cancel()
		resp, err := client.FirewallSets(c, &emptypb.Empty{})
		check("FirewallSets", err)
		for _, s := range resp.GetSets() {
			printSet(s)
		}
	case "set-add", "set-del":
		fs := flag.NewFlagSet("network firewall "+args[0], flag.ExitOnError)
		timeout := fs.Duration("timeout", 0, "set-add: the elements expire after this (and aren't kept across reboots)")
		_ = fs.Parse(args[1:])
		if fs.NArg() < 4 {
			fmt.Fprintf(os.Stderr, "usage: janusctl network firewall %s FAMILY TABLE SET ELEMENT...\n", args[0])
			os.Exit(2)
		}
		req := &janusv1alpha1.FirewallSetUpdateRequest{Family: fs.Arg(0), Table: fs.Arg(1), Set: fs.Arg(2)}
		for _, v := range fs.Args()[3:] {
			if args[0] == "set-add" {
				req.Add = append(req.Add, &janusv1alpha1.FirewallSetElement{Value: v, TimeoutSeconds: uint32(timeout.Seconds())})
			} else {
				req.Delete = append(req.Delete, v)
			}
		}
		c, cancel := ctx()
		defer cancel()
		resp, err := client.FirewallSetUpdate(c, req)
		check("FirewallSetUpdate", err)
		printSet(resp.GetSet())
	default:
		fmt.Fprintf(os.Stderr, "janusctl network firewall: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

func runFirewallApply(client janusv1alpha1.NetworkServiceClient, endpoint string, redial redialer, args []string) {
	fs := flag.NewFlagSet("network firewall apply", flag.ExitOnError)
	timeout := fs.Duration("timeout", 30*time.Second, "how long the node waits for the confirmation before reverting (5s-300s)")
	noConfirm := fs.Bool("no-confirm", false, "leave the ruleset on trial: confirm it with `network firewall confirm` before the timeout")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: janusctl network firewall apply [-timeout 30s] [-no-confirm] FILE")
		os.Exit(2)
	}
	rs, err := os.ReadFile(fs.Arg(0))
	check("read", err)
	c, cancel := ctx()
	defer cancel()
	resp, err := client.FirewallApplyRuleset(c, &janusv1alpha1.FirewallApplyRulesetRequest{Ruleset: rs, ConfirmTimeoutSeconds: uint32(timeout.Seconds())})
	check("FirewallApplyRuleset", err)
	if !resp.GetAccepted() {
		printErrors(resp.GetErrors())
		os.Exit(1)
	}
	revertAt := time.Unix(resp.GetRevertAtUnix(), 0)
	fmt.Printf("applied on trial - reverts at %s unless confirmed\n", revertAt.Format(time.RFC3339))
	if *noConfirm {
		return
	}
	// Over a new connection: the one used for the apply is established,
	// and stays open whatever the ruleset - it proves nothing.
	for time.Now().Before(revertAt) {
		if err := firewallConfirmOver(endpoint, redial); err == nil {
			fmt.Println("confirmed over a new connection - the ruleset is saved")
			return
		} else {
			fmt.Fprintf(os.Stderr, "confirm: %v - retrying\n", err)
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Fprintln(os.Stderr, "couldn't confirm before the timeout: the node reverts to the previous ruleset by itself")
	os.Exit(1)
}

func firewallConfirmOver(endpoint string, redial redialer) error {
	conn, err := redial(endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	c, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := janusv1alpha1.NewNetworkServiceClient(conn).FirewallConfirm(c, &emptypb.Empty{}); err != nil {
		return errors.New(status.Convert(err).Message())
	}
	return nil
}

func printErrors(errs []string) {
	fmt.Fprintln(os.Stderr, "refused:")
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "  "+e)
	}
}

func printSet(s *janusv1alpha1.FirewallSet) {
	if s == nil {
		return
	}
	flags := ""
	if len(s.GetFlags()) > 0 {
		flags = " (" + strings.Join(s.GetFlags(), ", ") + ")"
	}
	fmt.Printf("%s %s %s: %s%s, %d elements\n", s.GetFamily(), s.GetTable(), s.GetName(), s.GetType(), flags, len(s.GetElements()))
	for _, e := range s.GetElements() {
		extra := ""
		if e.GetPersistent() {
			extra = "  kept"
		}
		if e.GetTimeoutSeconds() > 0 {
			extra = fmt.Sprintf("  expires in %s", (time.Duration(e.GetExpiresSeconds()) * time.Second).String())
		}
		fmt.Printf("  %s%s\n", e.GetValue(), extra)
	}
}
