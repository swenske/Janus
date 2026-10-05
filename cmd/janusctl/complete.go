package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Shell completion: `janusctl completion bash|zsh|fish` prints a script
// that asks `janusctl __complete SHELL -- WORDS...` - the command line
// up to the word being completed - for what may come there: commands,
// flags, and their values from the command tree (commands.go), the
// contexts and nodes from the configuration, and from the node itself
// (services, maps, certificates, paths...) when a context or a node's
// certificate reaches it within a couple of seconds, never asking
// anything.

//go:embed completion/janusctl.bash
var bashScript string

//go:embed completion/_janusctl
var zshScript string

//go:embed completion/janusctl.fish
var fishScript string

// candidate is one thing completion offers, with what it is.
type candidate struct{ value, help string }

// Directives, printed first: what the shell does itself.
const (
	dirFiles   = ":files"   // complete local files
	dirDirs    = ":dirs"    // complete local directories
	dirNoSpace = ":nospace" // no space after the value (a directory on the node)
)

// onlineTimeout bounds what completion waits for the node.
const onlineTimeout = 2 * time.Second

func runCompletion(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: janusctl completion bash|zsh|fish")
		os.Exit(2)
	}
	switch args[0] {
	case "bash":
		fmt.Print(bashScript)
	case "zsh":
		fmt.Print(zshScript)
	case "fish":
		fmt.Print(fishScript)
	default:
		fmt.Fprintf(os.Stderr, "janusctl completion: no script for %q - bash, zsh or fish\n", args[0])
		os.Exit(2)
	}
}

// runComplete is janusctl __complete SHELL -- WORDS... (WORDS[0] being
// janusctl, the last the word under the cursor, possibly empty).
func runComplete(args []string) {
	if len(args) < 2 || args[1] != "--" {
		os.Exit(2)
	}
	shell, words := args[0], args[2:]
	if len(words) > 0 {
		words = words[1:] // janusctl itself
	}
	if len(words) == 0 {
		words = []string{""}
	}
	directives, cands := complete(words, nodeSource{})
	writeCompletion(os.Stdout, shell, directives, cands, words[len(words)-1])
}

// writeCompletion prints what the shell's script reads: the directives,
// then one candidate per line - "value" (bash), "value:help" with
// value's colons escaped (zsh's _describe), "value<TAB>help" (fish).
func writeCompletion(w io.Writer, shell string, directives []string, cands []candidate, cur string) {
	for _, d := range directives {
		fmt.Fprintln(w, d)
	}
	for _, c := range cands {
		if !strings.HasPrefix(c.value, cur) {
			continue
		}
		help := strings.ReplaceAll(c.help, "\n", " ")
		switch shell {
		case "zsh":
			v := strings.ReplaceAll(strings.ReplaceAll(c.value, `\`, `\\`), ":", `\:`)
			if help != "" {
				fmt.Fprintf(w, "%s:%s\n", v, help)
			} else {
				fmt.Fprintln(w, v)
			}
		case "fish":
			fmt.Fprintf(w, "%s\t%s\n", c.value, help)
		default:
			fmt.Fprintln(w, c.value)
		}
	}
}

// source answers what's on the node, and what the configuration knows.
type source interface {
	contexts() []candidate
	nodes(g globals) []candidate
	fleetNodes(g globals) []candidate
	issuers(g globals) []candidate
	online(g globals, kind argKind, prior []string, cur string) ([]candidate, []string)
}

// globals are the global flags given before the command.
type globals struct {
	context, nodes, endpoint, ca, cert, key string
	all                                     bool
}

// complete is what may come at words' last word, the words before it
// already typed.
func complete(words []string, src source) ([]string, []candidate) {
	cur := words[len(words)-1]
	done := words[:len(words)-1]

	// The global flags, before the command.
	var g globals
	i := 0
	for i < len(done) && strings.HasPrefix(done[i], "-") {
		name, value, hasValue := strings.Cut(strings.TrimLeft(done[i], "-"), "=")
		fl := findFlag(globalFlags, name)
		i++
		if fl == nil || fl.value == "" {
			if name == "all" {
				g.all = true
			}
			continue
		}
		if !hasValue {
			if i == len(done) {
				return completeValue(*fl, nil, cur, g, src)
			}
			value = done[i]
			i++
		}
		g.set(name, value)
	}
	if i == len(done) && strings.HasPrefix(cur, "-") {
		return completeFlags(globalFlags, cur, g, src, nil)
	}

	// The command, as deep as the words go.
	c := commands
	for i < len(done) {
		next := c.sub(done[i])
		if next == nil || next.hidden {
			break
		}
		c = next
		i++
	}
	if len(c.subs) > 0 {
		if i < len(done) {
			return nil, nil // an unknown command
		}
		var out []candidate
		for _, s := range c.subs {
			if !s.hidden {
				out = append(out, candidate{s.name, s.help})
			}
		}
		return nil, out
	}

	// The command's own flags and positional arguments, flags anywhere.
	var prior []string
	for i < len(done) {
		w := done[i]
		i++
		if strings.HasPrefix(w, "-") && w != "-" {
			name, _, hasValue := strings.Cut(strings.TrimLeft(w, "-"), "=")
			fl := c.flag(name)
			if fl != nil && fl.value != "" && !hasValue {
				if i == len(done) {
					return completeValue(*fl, prior, cur, g, src)
				}
				i++
			}
			continue
		}
		prior = append(prior, w)
	}
	if strings.HasPrefix(cur, "-") {
		return completeFlags(c.flags, cur, g, src, prior)
	}
	n := len(prior)
	if n >= len(c.pos) {
		if !c.variadic || len(c.pos) == 0 {
			return nil, nil
		}
		n = len(c.pos) - 1
	}
	return completeKind(c.pos[n], prior, cur, g, src)
}

func (g *globals) set(name, value string) {
	switch name {
	case "context":
		g.context = value
	case "n":
		g.nodes = value
	case "endpoint":
		g.endpoint = value
	case "ca":
		g.ca = value
	case "cert":
		g.cert = value
	case "key":
		g.key = value
	}
}

func findFlag(flags []flagDef, name string) *flagDef {
	for i := range flags {
		if flags[i].name == name {
			return &flags[i]
		}
	}
	return nil
}

// completeFlags offers flags - or, for "-name=VALUE", the value.
func completeFlags(flags []flagDef, cur string, g globals, src source, prior []string) ([]string, []candidate) {
	if name, value, ok := strings.Cut(strings.TrimLeft(cur, "-"), "="); ok {
		fl := findFlag(flags, name)
		if fl == nil {
			return nil, nil
		}
		prefix := cur[:len(cur)-len(value)]
		dirs, cands := completeValue(*fl, prior, value, g, src)
		out := make([]candidate, len(cands)) // never the shared fixedValues
		for i, c := range cands {
			out[i] = candidate{prefix + c.value, c.help}
		}
		return dirs, out
	}
	var out []candidate
	for _, fl := range flags {
		help := fl.help
		if fl.value != "" {
			help = fl.value + " - " + help
		}
		out = append(out, candidate{"-" + fl.name, help})
	}
	return nil, out
}

func completeValue(fl flagDef, prior []string, cur string, g globals, src source) ([]string, []candidate) {
	if fl.name == "n" && fl.kind == argNodes {
		return completeNodes(cur, g, src)
	}
	return completeKind(fl.kind, prior, cur, g, src)
}

// completeNodes offers the context's nodes - after the ones already
// given, comma-separated.
func completeNodes(cur string, g globals, src source) ([]string, []candidate) {
	given, _ := cutLast(cur, ",")
	prefix := ""
	if given != "" {
		prefix = given + ","
	}
	taken := strings.Split(given, ",")
	var out []candidate
	for _, n := range src.nodes(g) {
		if slices.Contains(taken, n.value) {
			continue
		}
		out = append(out, candidate{prefix + n.value, n.help})
	}
	return nil, out
}

func cutLast(s, sep string) (string, string) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func completeKind(kind argKind, prior []string, cur string, g globals, src source) ([]string, []candidate) {
	if fixed, ok := fixedValues[kind]; ok {
		return nil, fixed
	}
	switch kind {
	case argFile:
		return []string{dirFiles}, nil
	case argDir:
		return []string{dirDirs}, nil
	case argContext:
		return nil, src.contexts()
	case argNodes:
		return completeNodes(cur, g, src)
	case argFleetNode:
		return nil, src.fleetNodes(g)
	case argIssuer:
		return nil, src.issuers(g)
	case argNone:
		return nil, nil
	}
	cands, dirs := src.online(g, kind, prior, cur)
	return dirs, cands
}

// nodeSource is the real source: the configuration, the node.
type nodeSource struct{}

func (nodeSource) contexts() []candidate {
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	var out []candidate
	for name, c := range cfg.Contexts {
		help := c.Controller
		if c.Fleet != nil {
			help = "fleet without a Controller"
		}
		if name == cfg.Current {
			help += " (current)"
		}
		out = append(out, candidate{name, help})
	}
	slices.SortFunc(out, func(a, b candidate) int { return strings.Compare(a.value, b.value) })
	return out
}

func (nodeSource) nodes(g globals) []candidate {
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	_, c := currentContext(cfg, g.context)
	if c == nil {
		return nil
	}
	var out []candidate
	for _, n := range c.Nodes {
		help := n.Address
		if !n.Fleet {
			help += " - not in the fleet"
		}
		out = append(out, candidate{n.Name, help})
	}
	return out
}

func (s nodeSource) fleetNodes(g globals) []candidate { return s.nodes(g) }

func (nodeSource) issuers(g globals) []candidate {
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	name, c := currentContext(cfg, g.context)
	if c == nil || c.Fleet == nil {
		return nil
	}
	lf, err := openLocalFleet(name)
	if err != nil {
		return nil
	}
	var out []candidate
	for _, ca := range lf.cas {
		out = append(out, candidate{issuerName(ca), "until " + ca.NotAfter.Local().Format("2006-01-02")})
	}
	return out
}

// online asks the node - the context's (its first node), or the one the
// global flags name - within onlineTimeout; nothing if it can't without
// asking anything (an expired certificate, a passphrase).
func (nodeSource) online(g globals, kind argKind, prior []string, cur string) ([]candidate, []string) {
	conn, err := completionConn(g)
	if err != nil {
		return nil, nil
	}
	defer conn.Close()
	c, cancel := context.WithTimeout(context.Background(), onlineTimeout)
	defer cancel()
	return onlineCandidates(c, conn, kind, prior, cur)
}

func onlineCandidates(c context.Context, conn *grpc.ClientConn, kind argKind, prior []string, cur string) ([]candidate, []string) {
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	hap := janusv1alpha1.NewHAProxyServiceClient(conn)
	net := janusv1alpha1.NewNetworkServiceClient(conn)
	var out []candidate
	switch kind {
	case argService:
		resp, err := sys.ServiceList(c, &emptypb.Empty{})
		if err != nil {
			return nil, nil
		}
		for _, s := range resp.GetServices() {
			out = append(out, candidate{s.GetId(), s.GetState() + ", " + s.GetHealth()})
		}
	case argMap:
		resp, err := hap.MapList(c, &emptypb.Empty{})
		if err != nil {
			return nil, nil
		}
		for _, m := range resp.GetMaps() {
			out = append(out, candidate{m, "map"})
		}
	case argMapKey:
		if len(prior) == 0 {
			return nil, nil
		}
		resp, err := hap.MapGet(c, &janusv1alpha1.MapGetRequest{Map: prior[len(prior)-1]})
		if err != nil {
			return nil, nil
		}
		for k, v := range resp.GetEntries() {
			out = append(out, candidate{k, "→ " + v})
		}
		slices.SortFunc(out, func(a, b candidate) int { return strings.Compare(a.value, b.value) })
	case argCert:
		resp, err := hap.CertificateList(c, &emptypb.Empty{})
		if err != nil {
			return nil, nil
		}
		for _, x := range resp.GetCertificates() {
			out = append(out, candidate{x.GetName(), "until " + x.GetNotAfter()})
		}
	case argHAProxyFile:
		resp, err := hap.FileList(c, &emptypb.Empty{})
		if err != nil {
			return nil, nil
		}
		for _, f := range resp.GetFiles() {
			out = append(out, candidate{f.GetName(), humanBytes(f.GetSize())})
		}
	case argACMEName:
		resp, err := hap.ACMEStatus(c, &emptypb.Empty{})
		if err != nil {
			return nil, nil
		}
		for _, x := range resp.GetCertificates() {
			out = append(out, candidate{x.GetName(), strings.Join(x.GetDomains(), ", ")})
		}
	case argTable, argSet:
		resp, err := net.FirewallSets(c, &emptypb.Empty{})
		if err != nil {
			return nil, nil
		}
		seen := map[string]bool{}
		for _, s := range resp.GetSets() {
			if len(prior) >= 1 && s.GetFamily() != prior[0] {
				continue
			}
			v, help := s.GetTable(), "table ("+s.GetFamily()+")"
			if kind == argSet {
				if len(prior) < 2 || s.GetTable() != prior[1] {
					continue
				}
				v, help = s.GetName(), s.GetType()
			}
			if !seen[v] {
				seen[v] = true
				out = append(out, candidate{v, help})
			}
		}
	case argInterface:
		resp, err := net.NetworkStatus(c, &emptypb.Empty{})
		if err != nil {
			return nil, nil
		}
		for _, i := range resp.GetInterfaces() {
			state := "down"
			if i.GetUp() {
				state = "up"
			}
			out = append(out, candidate{i.GetName(), state})
		}
	case argRemote:
		return remoteEntries(c, sys, cur)
	}
	return out, nil
}

// remoteEntries completes a path on the node: what's in the directory
// cur names so far - directories with their "/", no space after them.
func remoteEntries(c context.Context, sys janusv1alpha1.SystemServiceClient, cur string) ([]candidate, []string) {
	dir := "/"
	if i := strings.LastIndex(cur, "/"); i >= 0 {
		dir = cur[:i+1]
	} else if cur != "" {
		return nil, nil // paths on the node are absolute
	}
	stream, err := sys.List(c, &janusv1alpha1.ListRequest{Root: dir})
	if err != nil {
		return nil, nil
	}
	var out []candidate
	for {
		e, err := stream.Recv()
		if errors.Is(err, io.EOF) || err != nil {
			break
		}
		name := e.GetRelativeName()
		if name == "" || name == "." || e.GetError() != "" {
			continue
		}
		full := path.Join(dir, name)
		if e.GetIsDir() {
			out = append(out, candidate{full + "/", "directory"})
		} else {
			out = append(out, candidate{full, humanBytes(uint64(e.GetSize()))})
		}
	}
	return out, []string{dirNoSpace}
}

// completionConn reaches the node like a command would, without ever
// asking anything: the node's own certificate (-endpoint -ca -cert
// -key), or the context's while it's valid - not renewed here.
func completionConn(g globals) (*grpc.ClientConn, error) {
	if g.endpoint != "" || g.ca != "" || g.cert != "" || g.key != "" {
		if g.endpoint == "" || g.ca == "" || g.cert == "" || g.key == "" {
			return nil, errors.New("the node's certificate takes -endpoint -ca -cert -key")
		}
		return dial(g.endpoint, g.ca, g.cert, g.key)
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	name, ctx := currentContext(cfg, g.context)
	if ctx == nil || !time.Now().Before(ctx.Expires) || strings.HasPrefix(ctx.SSHKey, "file:") {
		return nil, errors.New("no context usable without asking")
	}
	nodes, err := selectNodes(ctx, g.nodes, g.all || g.nodes == "")
	if err != nil || len(nodes) == 0 {
		return nil, errors.New("no node")
	}
	tlsConfig, err := nodeTLS(name, ctx, nodes[0])
	if err != nil {
		return nil, err
	}
	return dialTLS(nodes[0].Address, tlsConfig)
}
