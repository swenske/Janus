package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// What janusctl asks in a terminal instead of failing (pick.go): the
// command (janusctl alone, or a group alone), the node (-n ?, or a
// context with several and no -n), and an argument left out when it
// can list what goes there - a service, a map, a path on the node...

// pickTimeout bounds asking the node for a picker's list.
const pickTimeout = 10 * time.Second

// pickCommand asks for one of the commands under c (path its place).
func pickCommand(c *command, path []string) []string {
	var items []candidate
	c.leaves(path, func(p []string, leaf *command) {
		if leaf.name == "help" || leaf.name == "completion" {
			return
		}
		help := leaf.help
		if leaf.args != "" {
			help = leaf.args + " - " + help
		}
		items = append(items, candidate{strings.Join(p, " "), help})
	})
	title := "Which command?"
	if len(path) > 0 {
		title = "Which " + strings.Join(path, " ") + " command?"
	}
	got, err := pick(title, items, false, true)
	exitIfCancelled(err)
	return strings.Fields(got[0])
}

// pickNodes asks for some of the context's nodes.
func pickNodes(ctx *cliContext) string {
	var items []candidate
	for _, n := range ctx.Nodes {
		if n.Fleet {
			items = append(items, candidate{n.Name, n.Address})
		}
	}
	got, err := pick("Which node?", items, true, true)
	exitIfCancelled(err)
	return strings.Join(got, ",")
}

func exitIfCancelled(err error) {
	if err == nil {
		return
	}
	if err == errCancelled {
		os.Exit(130)
	}
	fmt.Fprintln(os.Stderr, "janusctl:", err)
	os.Exit(1)
}

// leafArgs splits args (the command's path first) into the command,
// its flags and its positional arguments.
func leafArgs(args []string) (*command, []string, []string) {
	c, rest := commands.find(args)
	var positional []string
	for i := 0; i < len(rest); i++ {
		w := rest[i]
		if strings.HasPrefix(w, "-") && w != "-" {
			name, _, hasValue := strings.Cut(strings.TrimLeft(w, "-"), "=")
			if fl := c.flag(name); fl != nil && fl.value != "" && !hasValue {
				i++
			}
			continue
		}
		positional = append(positional, w)
	}
	return c, rest, positional
}

// needed is how many positional arguments c must get, given the ones
// it has - "context use" and "context delete" a name.
func needed(c *command, positional []string) int {
	if c.name == "context" && len(positional) == 1 && (positional[0] == "use" || positional[0] == "delete") {
		return 2
	}
	return c.required
}

// fillMissing asks for the positional arguments args lacks that can be
// listed - conn the node's connection, nil for a command without one -,
// in order, stopping at the first that can't. It reports whether it
// asked anything.
func fillMissing(args []string, conn *grpc.ClientConn) ([]string, bool) {
	c, _, positional := leafArgs(args)
	asked := false
	for i := len(positional); i < needed(c, positional) && i < len(c.pos); i++ {
		kind := c.pos[i]
		value, ok := pickArg(c, kind, positional, conn)
		if !ok {
			break
		}
		args = append(args, value)
		positional = append(positional, value)
		asked = true
	}
	return args, asked
}

var argTitles = map[argKind]string{
	argContext: "Which context?", argFleetNode: "Which node?", argIssuer: "Which issuing CA?",
	argRole: "Which role?", argService: "Which service?", argServiceAction: "Do what?",
	argLogService: "Whose logs?", argMap: "Which map?", argMapKey: "Which key?", argCert: "Which certificate?",
	argHAProxyFile: "Which file?", argACMEName: "Which certificate?", argFamily: "Which family?",
	argTable: "Which table?", argSet: "Which set?", argInterface: "Which interface?", argShell: "Which shell?",
}

// pickArg asks for one argument of kind, if janusctl can list it.
func pickArg(c *command, kind argKind, prior []string, conn *grpc.ClientConn) (string, bool) {
	var items []candidate
	src := nodeSource{}
	switch kind {
	case argContext:
		items = src.contexts()
	case argFleetNode:
		items = src.fleetNodes(globals{})
	case argIssuer:
		items = src.issuers(globals{})
	case argRemote:
		if conn == nil {
			return "", false
		}
		v, err := pickRemote(janusv1alpha1.NewSystemServiceClient(conn), c.name != "cat")
		exitIfCancelled(err)
		return v, true
	default:
		if fixed, ok := fixedValues[kind]; ok && kind != argContextAction {
			items = fixed
			break
		}
		title := argTitles[kind]
		if conn == nil || title == "" {
			return "", false
		}
		ctx, cancel := context.WithTimeout(context.Background(), pickTimeout)
		items, _ = onlineCandidates(ctx, conn, kind, prior, "")
		cancel()
	}
	if len(items) == 0 {
		return "", false
	}
	title := argTitles[kind]
	if title == "" {
		title = "Which one?"
	}
	got, err := pick(title, items, false, true)
	exitIfCancelled(err)
	return got[0], true
}

// pickRemote browses the node's files: Enter opens a directory, picks a
// file - or, with dirs, the directory shown (".").
func pickRemote(sys janusv1alpha1.SystemServiceClient, dirs bool) (string, error) {
	dir := "/"
	for {
		ctx, cancel := context.WithTimeout(context.Background(), pickTimeout)
		entries, _ := remoteEntries(ctx, sys, dir)
		cancel()
		var items []candidate
		if dirs {
			items = append(items, candidate{".", "this directory"})
		}
		if dir != "/" {
			items = append(items, candidate{"..", "up"})
		}
		for _, e := range entries {
			name := strings.TrimPrefix(e.value, dir)
			items = append(items, candidate{name, e.help})
		}
		got, err := pick(dir, items, false, false)
		if err != nil {
			return "", err
		}
		switch v := got[0]; {
		case v == ".":
			fmt.Fprintf(os.Stderr, "%s %s\n", styleFor(os.Stderr).green("✔"), dir)
			return dir, nil
		case v == "..":
			dir = path.Dir(strings.TrimSuffix(dir, "/"))
			if dir != "/" {
				dir += "/"
			}
		case strings.HasSuffix(v, "/"):
			dir += v
		default:
			fmt.Fprintf(os.Stderr, "%s %s\n", styleFor(os.Stderr).green("✔"), dir+v)
			return dir + v, nil
		}
	}
}

// echoCommand shows the command line a picker built, to type it next
// time: the global flags given, the arguments.
func echoCommand(globalWords, args []string) {
	words := append(slices.Clone(globalWords), args...)
	for i, w := range words {
		if w == "" || strings.ContainsAny(w, " \t'\"$&|;<>()*?") {
			words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
	}
	st := styleFor(os.Stderr)
	fmt.Fprintln(os.Stderr, st.dim("→ janusctl "+strings.Join(words, " ")))
}
