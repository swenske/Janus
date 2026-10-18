package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// runHAProxyRuntime handles the runtime diagnostics - lookups, stick
// tables, streams, captured errors; false if sub isn't one.
func runHAProxyRuntime(client janusv1alpha1.HAProxyServiceClient, sub string, args []string) bool {
	c, cancel := ctx()
	defer cancel()
	switch sub {
	case "map-lookup", "acl-lookup":
		if len(args) != 2 {
			fmt.Fprintf(os.Stderr, "usage: janusctl haproxy %s %s VALUE\n", sub, strings.ToUpper(strings.TrimSuffix(sub, "-lookup")))
			os.Exit(2)
		}
		req := &janusv1alpha1.PatternLookupRequest{Name: args[0], Value: args[1]}
		var resp *janusv1alpha1.PatternLookupResponse
		var err error
		if sub == "map-lookup" {
			resp, err = client.MapLookup(c, req)
		} else {
			resp, err = client.ACLLookup(c, req)
		}
		check("lookup", err)
		fmt.Printf("found    %s\nmethod   %s\n", map[bool]string{true: "yes", false: "no"}[resp.GetFound()], resp.GetMethod())
		if !resp.GetFound() {
			os.Exit(1)
		}
		fmt.Printf("pattern  %s\n", resp.GetPattern())
		if sub == "map-lookup" {
			fmt.Printf("value    %s\n", resp.GetValue())
		}

	case "tables":
		resp, err := client.TableList(c, &emptypb.Empty{})
		check("TableList", err)
		tw := table("TABLE", "TYPE", "USED", "SIZE")
		for _, t := range resp.GetTables() {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", t.GetName(), t.GetType(), t.GetUsed(), t.GetSize())
		}
		tw.Flush()

	case "table":
		fs := flag.NewFlagSet("haproxy table", flag.ExitOnError)
		key := fs.String("key", "", "that entry only")
		filter := fs.String("filter", "", "DATA:OP:VALUE,... - the entries whose data match")
		limit := fs.Uint("limit", 0, "entries shown (500 by default, 5000 at most)")
		parseAnywhere(fs, args)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy table [-key KEY] [-filter DATA:OP:VALUE,...] [-limit N] TABLE")
			os.Exit(2)
		}
		resp, err := client.TableShow(c, &janusv1alpha1.TableShowRequest{Table: fs.Arg(0), Key: *key, Filters: parseTableFilters(*filter), Limit: uint32(*limit)}) //nolint:gosec // G115: the node caps it at 5000
		check("TableShow", err)
		t := resp.GetTable()
		fmt.Printf("%s: %s keys, %d of %d entries used\n", t.GetName(), t.GetType(), t.GetUsed(), t.GetSize())
		printTableEntries(resp.GetEntries())
		if resp.GetTruncated() {
			fmt.Fprintf(os.Stderr, "janusctl: %d entries shown, more match (-limit, -filter)\n", len(resp.GetEntries()))
		}

	case "table-clear":
		fs := flag.NewFlagSet("haproxy table-clear", flag.ExitOnError)
		key := fs.String("key", "", "that entry")
		filter := fs.String("filter", "", "DATA:OP:VALUE,... - the entries whose data match")
		all := fs.Bool("all", false, "every entry")
		parseAnywhere(fs, args)
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy table-clear (-key KEY | -filter DATA:OP:VALUE,... | -all) TABLE")
			os.Exit(2)
		}
		_, err := client.TableClear(c, &janusv1alpha1.TableClearRequest{Table: fs.Arg(0), Key: *key, Filters: parseTableFilters(*filter), All: *all})
		check("TableClear", err)

	case "sessions":
		fs := flag.NewFlagSet("haproxy sessions", flag.ExitOnError)
		frontend := fs.String("frontend", "", "a frontend's only")
		backend := fs.String("backend", "", "a backend's only")
		source := fs.String("source", "", "a client's only, with or without its port")
		limit := fs.Uint("limit", 0, "streams shown (500 by default, 5000 at most)")
		_ = fs.Parse(args)
		resp, err := client.SessionList(c, &janusv1alpha1.SessionListRequest{Frontend: *frontend, Backend: *backend, Source: *source, Limit: uint32(*limit)}) //nolint:gosec // G115: the node caps it at 5000
		check("SessionList", err)
		tw := table("ID", "SOURCE", "FRONTEND", "BACKEND", "SERVER", "AGE", "EXPIRES")
		for _, s := range resp.GetSessions() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.GetId(), s.GetSource(), s.GetFrontend(), dash(s.GetBackend()), dash(s.GetServer()), s.GetAge(), dash(s.GetExpires()))
		}
		tw.Flush()
		if resp.GetTruncated() {
			fmt.Fprintf(os.Stderr, "janusctl: %d streams shown, more match (-limit, -frontend, -backend, -source)\n", len(resp.GetSessions()))
		}

	case "session-kill":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: janusctl haproxy session-kill ID")
			os.Exit(2)
		}
		_, err := client.SessionShutdown(c, &janusv1alpha1.SessionShutdownRequest{Id: args[0]})
		check("SessionShutdown", err)

	case "errors":
		fs := flag.NewFlagSet("haproxy errors", flag.ExitOnError)
		proxy := fs.String("proxy", "", "a frontend's or backend's only")
		direction := fs.String("direction", "", "request or response only")
		_ = fs.Parse(args)
		resp, err := client.ErrorsShow(c, &janusv1alpha1.ErrorsShowRequest{Proxy: *proxy, Direction: *direction})
		check("ErrorsShow", err)
		fmt.Printf("%d errors captured since HAProxy started, the last of each proxy kept\n", resp.GetTotal())
		for _, e := range resp.GetErrors() {
			printCapturedError(e)
		}

	default:
		return false
	}
	return true
}

// parseTableFilters reads "http_req_rate:gt:100,gpc0:eq:0"; a malformed
// one is a usage error.
func parseTableFilters(s string) []*janusv1alpha1.TableFilter {
	var out []*janusv1alpha1.TableFilter
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		parts := strings.Split(f, ":")
		var v int64
		var err error
		if len(parts) == 3 {
			v, err = strconv.ParseInt(parts[2], 10, 64)
		}
		if len(parts) != 3 || err != nil {
			fmt.Fprintf(os.Stderr, "janusctl: filter %q: DATA:OP:VALUE, e.g. http_req_rate:gt:100\n", f)
			os.Exit(2)
		}
		out = append(out, &janusv1alpha1.TableFilter{Data: parts[0], Op: parts[1], Value: v})
	}
	return out
}

// printTableEntries lays entries out as a table: KEY, USE, EXPIRES, then
// one column per stored data, as the first entry names them.
func printTableEntries(entries []*janusv1alpha1.TableEntry) {
	var names []string
	if len(entries) > 0 {
		for _, d := range entries[0].GetData() {
			names = append(names, strings.ToUpper(d.GetName()))
		}
	}
	tw := table(append([]string{"KEY", "USE", "EXPIRES"}, names...)...)
	for _, e := range entries {
		row := []string{e.GetKey(), strconv.FormatUint(e.GetUse(), 10), (time.Duration(e.GetExpiresMs()) * time.Millisecond).Round(time.Second).String()}
		for _, d := range e.GetData() {
			row = append(row, d.GetValue())
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
}

// printCapturedError shows a captured error as HAProxy does, a caret
// under the byte it refused.
func printCapturedError(e *janusv1alpha1.CapturedError) {
	fmt.Printf("\n[%s] %s %s: invalid %s", e.GetWhen(), e.GetProxyKind(), e.GetProxy(), e.GetDirection())
	if e.GetSource() != "" {
		fmt.Printf(" from %s", e.GetSource())
	}
	if e.GetServer() != "" {
		fmt.Printf(", server %s", e.GetServer())
	}
	fmt.Printf(", refused at byte %d\n", e.GetPosition())
	for _, l := range e.GetLines() {
		mark := " "
		if l.GetContinued() {
			mark = "+"
		}
		prefix := fmt.Sprintf("  %05d%s ", l.GetOffset(), mark)
		fmt.Println(prefix + l.GetText())
		if at := l.GetErrorAt(); at >= 0 {
			fmt.Println(strings.Repeat(" ", len(prefix)+int(at)) + "^")
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
