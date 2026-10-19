package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// applyConfig streams an ApplyConfig - a file's configuration, or a
// version of the history (config-revert) - printing each stage; the exit
// status is 1 when HAProxy refused it.
func applyConfig(client janusv1alpha1.HAProxyServiceClient, req *janusv1alpha1.ApplyConfigRequest) {
	c, cancel := ctx()
	defer cancel()
	stream, err := client.ApplyConfig(c, req)
	if err != nil {
		log.Fatalf("ApplyConfig: %v", err)
	}
	var last *janusv1alpha1.ApplyConfigResponse
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		check("ApplyConfig", err)
		fmt.Println(progressLine(os.Stdout, resp.GetStage(), 0, false, resp.GetMessage()))
		last = resp
	}
	if last != nil && !last.GetAccepted() {
		os.Exit(1)
	}
}

// runConfigHistory prints "haproxy config-history": the versions, newest
// first.
func runConfigHistory(client janusv1alpha1.HAProxyServiceClient, args []string) {
	fs := flag.NewFlagSet("haproxy config-history", flag.ExitOnError)
	limit := fs.Uint("limit", 0, "the newest N only")
	parseAnywhere(fs, args)
	c, cancel := ctx()
	defer cancel()
	resp, err := client.ConfigHistory(c, &janusv1alpha1.ConfigHistoryRequest{Limit: uint32(*limit)}) //nolint:gosec // G115: a count typed on the command line
	check("ConfigHistory", err)
	printConfigHistory(os.Stdout, resp, time.Local)
}

// printConfigHistory prints the versions with their times in loc.
func printConfigHistory(out io.Writer, resp *janusv1alpha1.ConfigHistoryResponse, loc *time.Location) {
	if len(resp.GetVersions()) == 0 {
		fmt.Fprintln(out, "no configuration applied since this node began keeping a history")
		return
	}
	t := newTable(out)
	fmt.Fprintln(t, "ID\tAPPLIED\tBY\tCHANGES\tSHA256\t")
	for _, v := range resp.GetVersions() {
		applied, by := "-", "-"
		if v.GetAppliedUnix() != 0 {
			applied = time.Unix(v.GetAppliedUnix(), 0).In(loc).Format("2006-01-02 15:04:05")
		}
		if a := v.GetAuthor(); a.GetName() != "" {
			by = a.GetName()
			if len(a.GetRoles()) > 0 {
				by += " (" + strings.Join(a.GetRoles(), ", ") + ")"
			}
			if a.GetVia() != "" {
				by += " via " + a.GetVia()
			}
		}
		var notes []string
		if v.GetRunning() {
			notes = append(notes, "running")
		}
		if v.GetRevertedFrom() != 0 {
			notes = append(notes, fmt.Sprintf("back to %d", v.GetRevertedFrom()))
		}
		if v.GetBeforeHistory() {
			notes = append(notes, "found when the history began")
		}
		fmt.Fprintf(t, "%d\t%s\t%s\t+%d -%d\t%s\t%s\n", v.GetId(), applied, by, v.GetLinesAdded(), v.GetLinesRemoved(), v.GetSha256()[:min(12, len(v.GetSha256()))], strings.Join(notes, ", "))
	}
	_ = t.Flush()
	if resp.GetRedacted() {
		fmt.Fprintln(out, "Figures from the configurations with their secrets masked: this certificate may read them, not apply one.")
	}
}
