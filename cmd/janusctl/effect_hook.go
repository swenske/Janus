package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
)

// janusctl __effect: what a command line does to the nodes, from the
// command tree (commands.go) and effects.go - the same janusctl that
// runs it, its own version's commands.
//
//	janusctl __effect hook [-allow-reads]
//
// is the Claude Code plugin's PreToolUse hook (claude-code/janusctl/
// hooks/hooks.json, docs/guide/claude-code.md): Claude Code's JSON on
// stdin, the command Claude is about to run in tool_input.command; it
// finds every janusctl invocation in it - after a pipe, &&, ;, in
// $(...), sh -c '...', behind env, timeout, sudo, xargs... - and answers:
//
//   - a critical command, or one it can't tell (a variable for the
//     command, an unknown command, an unterminated quote): "ask", with
//     the reason - Claude Code then asks, in auto mode too;
//   - changes: nothing - the permission rules decide;
//   - reads: nothing, or "allow" when the user turned the plugin's
//     allow_reads option on (CLAUDE_PLUGIN_OPTION_ALLOW_READS) or gave
//     -allow-reads - only for a command line made of janusctl reads
//     alone, no file of this machine named, nothing redirected.
//
// It never blocks and always exits 0: whatever it can't read, it leaves
// to the permission rules - which still apply over its "allow" (a deny or
// ask rule wins).
//
//	janusctl __effect -- WORDS...
//
// prints what one command line does, for a person: its class and path.

// allowReadsEnv is the plugin's allow_reads option, as Claude Code
// exports it to the hook (userConfig, plugin.json).
const allowReadsEnv = "CLAUDE_PLUGIN_OPTION_ALLOW_READS"

func runEffect(args []string) {
	if len(args) > 0 && args[0] == "--" {
		line := strings.Join(args[1:], " ")
		for _, inv := range findInvocations(line) {
			fmt.Println(inv.describe())
		}
		return
	}
	if len(args) == 0 || args[0] != "hook" {
		fmt.Fprintln(os.Stderr, "usage: janusctl __effect hook [-allow-reads] | __effect -- COMMAND LINE")
		os.Exit(2)
	}
	allowReads := isTrue(os.Getenv(allowReadsEnv))
	for _, a := range args[1:] {
		if a == "-allow-reads" || a == "--allow-reads" {
			allowReads = true
		}
	}
	effectHook(os.Stdin, os.Stdout, allowReads)
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// hookInput is what Claude Code sends a PreToolUse hook - the part read.
type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// effectHook answers one PreToolUse call: a decision on w, or nothing.
func effectHook(r io.Reader, w io.Writer, allowReads bool) {
	var in hookInput
	if err := json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&in); err != nil {
		return
	}
	if in.ToolName != "Bash" || (in.HookEventName != "" && in.HookEventName != "PreToolUse") {
		return
	}
	decision, reason := decide(in.ToolInput.Command, allowReads)
	if decision == "" {
		return
	}
	var out hookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = decision
	out.HookSpecificOutput.PermissionDecisionReason = reason
	_ = json.NewEncoder(w).Encode(out)
}

// decide is the hook's answer for a command line: "ask", "allow" or ""
// (no decision), and why.
func decide(line string, allowReads bool) (string, string) {
	invs := findInvocations(line)
	var asks []string
	for _, inv := range invs {
		if r := inv.reason(); (inv.doubt != "" || inv.effect == effectCritical) && !slices.Contains(asks, r) {
			asks = append(asks, r)
		}
	}
	if len(asks) > 0 {
		if len(asks) > 3 {
			asks = append(asks[:3], fmt.Sprintf("and %d more", len(asks)-3))
		}
		return "ask", strings.Join(asks, "; ")
	}
	if allowReads && readsOnly(line, invs) {
		return "allow", "janusctl: reads only - the plugin's allow_reads option"
	}
	return "", ""
}

// invocation is one janusctl command found in a command line.
type invocation struct {
	// words is the invocation as written, from janusctl on.
	words []string
	// path is the command's path in the tree, empty for janusctl alone.
	path []string
	effectOf
	help string
	// nodes says where it runs: -n's nodes, every node (-all), a context.
	nodes string
	// doubt: why it can't be told what the command does - asked then.
	doubt string
	// direct: janusctl is the command itself - not behind another
	// program, no variable assigned before it (or behind timeout only).
	direct bool
	// localFiles: a flag or an argument names a file of this machine.
	localFiles bool
	// cmd is the simple command it was found in.
	cmd *shCommand
}

func (inv invocation) shown() string {
	s := strings.Join(inv.words, " ")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

// reason is what the prompt says about an invocation asked for.
func (inv invocation) reason() string {
	if inv.doubt != "" {
		return fmt.Sprintf("janusctl can't tell what `%s` does: %s", inv.shown(), inv.doubt)
	}
	s := fmt.Sprintf("janusctl %s", strings.Join(inv.path, " "))
	if inv.nodes != "" {
		s += " " + inv.nodes
	}
	s += " is critical"
	if inv.help != "" {
		s += " - " + inv.help
	}
	if inv.note != "" && !strings.Contains(inv.help, inv.note) {
		s += " (" + inv.note + ")"
	}
	return s
}

// describe is __effect --'s line for an invocation.
func (inv invocation) describe() string {
	class := inv.String()
	if inv.doubt != "" {
		class = "unknown"
	}
	s := class + "\t" + strings.Join(inv.path, " ")
	if inv.doubt != "" {
		s += "\t" + inv.doubt
	} else if inv.note != "" {
		s += "\t" + inv.note
	}
	return s
}

// findInvocations finds every janusctl invocation in a command line.
func findInvocations(line string) []invocation {
	var out []invocation
	findIn(line, 0, &out)
	return out
}

func findIn(line string, depth int, out *[]invocation) {
	parsed := parseShell(line)
	if parsed.err != nil {
		if mentionsJanusctl(line, parsed) {
			*out = append(*out, invocation{words: []string{line}, doubt: "the shell couldn't read it (" + parsed.err.Error() + ")"})
		}
		return
	}
	named := mentionsJanusctl(line, parsed)
	for i := range parsed.cmds {
		findInCommand(&parsed.cmds[i], named, depth, out)
	}
}

// mentionsJanusctl: janusctl is named somewhere in the line, as written
// or once its quotes are removed.
func mentionsJanusctl(line string, parsed shLine) bool {
	if strings.Contains(line, "janusctl") {
		return true
	}
	for _, c := range parsed.cmds {
		for _, w := range c.words {
			if strings.Contains(w.text, "janusctl") {
				return true
			}
		}
	}
	return false
}

// isJanusctl: the word names janusctl - janusctl, a path to it.
func isJanusctl(w shWord) bool {
	return !w.dynamic && path.Base(w.text) == "janusctl"
}

// prefixWords are reserved words and builtins that run what follows.
var prefixWords = map[string]bool{
	"!": true, "{": true, "}": true, "if": true, "then": true, "else": true, "elif": true,
	"do": true, "while": true, "until": true, "time": true, "builtin": true, "noglob": true,
	"nocorrect": true, "coproc": true, "exec": true,
}

// shellNames are the shells whose -c runs a string.
var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "busybox": true, "fish": true}

// runners are programs that run the command given in their arguments: a
// janusctl behind one is read like one run directly - what can't be
// told is asked. Behind any other program (grep janusctl docs/, go test
// ./cmd/janusctl) only a janusctl command named in full counts.
var runners = map[string]bool{
	"env": true, "sudo": true, "doas": true, "nice": true, "nohup": true, "timeout": true, "stdbuf": true,
	"xargs": true, "parallel": true, "watch": true, "ssh": true, "find": true, "setsid": true, "ionice": true,
	"flock": true, "strace": true, "ltrace": true, "chroot": true, "unshare": true, "nsenter": true,
	"runuser": true, "su": true, "script": true, "time": true, "exec": true, "command": true, "builtin": true,
	"taskset": true, "chrt": true, "systemd-run": true, "unbuffer": true, "caffeinate": true, "chpst": true,
}

func findInCommand(c *shCommand, named bool, depth int, out *[]invocation) {
	words := c.words
	i := 0
	assigned := false
	for i < len(words) && !words[i].dynamic && isAssignment(words[i].text) {
		assigned = true
		i++
	}
	for i < len(words) && !words[i].dynamic && prefixWords[words[i].text] {
		i++
	}
	if i < len(words) && words[i].text == "command" && !words[i].dynamic && (i+1 >= len(words) || !strings.HasPrefix(words[i+1].text, "-")) {
		i++
	}
	if i < len(words) && words[i].text == "timeout" && !words[i].dynamic {
		if j, ok := skipTimeout(words, i+1); ok {
			i = j // still run directly: allow_reads takes it
		}
	}
	if i >= len(words) {
		return
	}
	head := words[i]

	// Strings that hold a command line of their own, given to a shell
	// or a program that runs one: sh -c '...', ssh host '...', watch
	// '...', su -c '...', docker exec c sh -c '...'. Not a commit
	// message or an echo that names a janusctl command.
	base := path.Base(head.text)
	reparse := !head.dynamic && (runners[base] || shellNames[base])
	for j := i + 1; j < len(words) && depth < 4; j++ {
		w := words[j]
		if shellNames[path.Base(w.text)] && !w.dynamic {
			reparse = true
		}
		if reparse && strings.Contains(w.text, "janusctl") && strings.ContainsAny(w.text, " \t\n;|&") {
			findIn(w.text, depth+1, out)
		}
	}
	if head.text == "eval" && !head.dynamic {
		var parts []string
		for _, w := range words[i+1:] {
			parts = append(parts, w.text)
		}
		if joined := strings.Join(parts, " "); depth < 4 && strings.Contains(joined, "janusctl") {
			findIn(joined, depth+1, out)
		}
		return
	}

	switch {
	case isJanusctl(head):
		inv := resolveInvocation(words[i:], false)
		inv.direct = depth == 0 && !assigned
		inv.cmd = c
		*out = append(*out, inv)
	case head.dynamic:
		// A program not known here: a janusctl in a variable, maybe.
		inv := resolveInvocation(words[i:], false)
		inv.cmd = c
		switch {
		case strings.Contains(strings.ToLower(head.text), "janusctl"):
			inv.doubt = "the program is `" + head.text + "`, not known before it runs"
		case inv.doubt == "" && inv.effect == effectCritical:
			inv.doubt = "the program is `" + head.text + "`, not known before it runs - a janusctl, its words say: " + strings.Join(inv.path, " ") + ", critical"
		default:
			return
		}
		*out = append(*out, inv)
	case shellNames[path.Base(head.text)]:
		shellInput(c, words, i, named, depth, out)
	default:
		// janusctl behind another program: env, sudo, xargs, watch,
		// find -exec, ssh HOST, go run ./cmd/janusctl...
		runner := runners[path.Base(head.text)] && !head.dynamic
		stdinArgs := runner && (path.Base(head.text) == "xargs" || path.Base(head.text) == "parallel")
		if runner {
			if k := slices.IndexFunc(words[i+1:], func(w shWord) bool { return !w.dynamic && shellNames[path.Base(w.text)] }); k >= 0 {
				shellInput(c, words, i+1+k, named, depth, out)
			}
		}
		for j := i + 1; j < len(words); j++ {
			if !isJanusctl(words[j]) {
				continue
			}
			inv := resolveInvocation(words[j:], stdinArgs)
			inv.cmd = c
			switch {
			case len(inv.path) == 0 && inv.doubt == "":
				// janusctl alone: its help - or an argument (which janusctl).
			case runner:
				*out = append(*out, inv)
			case inv.doubt == "" && inv.effect == effectCritical:
				*out = append(*out, inv)
			}
			break
		}
	}
}

// shellInput reads what the shell at words[i] runs besides its -c
// string (read as a string above): the commands of a heredoc or a
// here-string; from a pipe or a file, they can't be read here - asked
// when janusctl is named.
func shellInput(c *shCommand, words []shWord, i int, named bool, depth int, out *[]invocation) {
	head := words[i]
	dashC, dashS, script := false, false, false
	for k := i + 1; k < len(words) && !script; k++ {
		w := words[k].text
		switch {
		case w == "-o" || w == "+o" || w == "-O" || w == "+O":
			k++ // an option's name
		case strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") || strings.HasPrefix(w, "+"):
			dashC = dashC || strings.Contains(w, "c")
			dashS = dashS || strings.Contains(w, "s")
		case !strings.HasPrefix(w, "-"):
			script = true // a script file, then its arguments
		}
	}
	if dashC || depth >= 4 {
		return
	}
	fromStdin := !script || dashS
	for _, r := range c.redirs {
		switch op := strings.TrimLeft(r.op, "0123456789"); {
		case r.body != nil && fromStdin:
			findIn(*r.body, depth+1, out)
			fromStdin = false
		case op == "<<<" && fromStdin:
			findIn(r.target, depth+1, out)
			fromStdin = false
		case op == "<" && fromStdin && named:
			*out = append(*out, invocation{words: []string{head.text, "<", r.target}, doubt: "a shell runs the commands of " + r.target + ", not read here", cmd: c})
			fromStdin = false
		}
	}
	if fromStdin && c.piped && named {
		*out = append(*out, invocation{words: []string{head.text}, doubt: "a shell runs the commands it reads from the pipe, not read here", cmd: c})
	}
}

func isAssignment(s string) bool {
	name, _, ok := strings.Cut(s, "=")
	if !ok || name == "" || !isNameStart(name[0]) {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isNameChar(name[i]) {
			return name[i] == '+' && i == len(name)-1 // NAME+=value
		}
	}
	return true
}

// skipTimeout skips timeout's options and duration: the index of the
// command it runs.
func skipTimeout(words []shWord, i int) (int, bool) {
	for i < len(words) {
		w := words[i].text
		switch {
		case w == "--foreground" || w == "--preserve-status" || w == "-v" || w == "--verbose":
			i++
		case w == "-s" || w == "-k" || w == "--signal" || w == "--kill-after":
			i += 2
		case strings.HasPrefix(w, "--signal=") || strings.HasPrefix(w, "--kill-after=") || (strings.HasPrefix(w, "-s") || strings.HasPrefix(w, "-k")) && len(w) > 2:
			i++
		case strings.HasPrefix(w, "-"):
			return i, false
		default:
			return i + 1, i+1 < len(words) // the duration
		}
	}
	return i, false
}

// resolveInvocation reads words - janusctl and what follows it - against
// the command tree. stdinArgs: more words come from stdin (xargs).
func resolveInvocation(words []shWord, stdinArgs bool) invocation {
	inv := invocation{}
	for _, w := range words {
		inv.words = append(inv.words, w.text)
	}
	k := 1
	explicitNodeCert := false
	// The global flags, as Go's flag package reads them.
	for k < len(words) {
		w := words[k]
		if !strings.HasPrefix(w.text, "-") || w.text == "-" {
			break
		}
		if flagName, _, _ := strings.Cut(w.text, "="); w.dynamic && strings.ContainsAny(flagName, "$`*?[{") {
			inv.doubt = "`" + w.text + "` stands where a global flag goes"
			return inv
		}
		k++
		if w.text == "--" {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(w.text, "-"), "-"), "=")
		if name == "h" || name == "help" {
			inv.effectOf = effectOf{effect: effectReads}
			inv.help = "its help"
			return inv
		}
		fl := findFlag(globalFlags, name)
		if fl == nil {
			inv.doubt = "this janusctl has no global flag -" + name
			return inv
		}
		if fl.value == "" {
			if name == "all" && (!hasValue || isTrue(value)) {
				inv.nodes = "on every node"
			}
			continue
		}
		if !hasValue {
			if k >= len(words) {
				break
			}
			value = words[k].text
			k++
		}
		switch name {
		case "n":
			inv.nodes = "on " + value
		case "context":
			if inv.nodes == "" {
				inv.nodes = "with context " + value
			}
		case "endpoint", "ca", "cert", "key":
			explicitNodeCert = true
			if name == "endpoint" {
				inv.nodes = "on " + value
			}
		}
	}
	inv.localFiles = explicitNodeCert
	rest := words[k:]
	if len(rest) == 0 {
		if stdinArgs {
			inv.doubt = "its command comes from stdin"
			return inv
		}
		inv.effectOf = effectOf{effect: effectReads}
		inv.help = "its help"
		return inv
	}
	c := commands
	n := 0
	for n < len(rest) && len(c.subs) > 0 {
		w := rest[n]
		if w.dynamic {
			inv.doubt = "`" + w.text + "` stands where the command goes"
			return inv
		}
		next := c.sub(w.text)
		if next == nil {
			break
		}
		c = next
		inv.path = append(inv.path, w.text)
		n++
	}
	args := rest[n:]
	if c == commands {
		if helpWord(rest[0].text) {
			inv.effectOf = effectOf{effect: effectReads}
			return inv
		}
		inv.doubt = "this janusctl has no command " + rest[0].text
		return inv
	}
	if c.hidden {
		inv.effectOf = effectOf{effect: effectReads}
		return inv
	}
	for _, a := range args {
		if a.text == "--" {
			break
		}
		if helpWord(a.text) && !a.dynamic {
			inv.effectOf = effectOf{effect: effectReads}
			inv.help = "its help"
			return inv
		}
	}
	if len(c.subs) > 0 {
		switch {
		case len(args) > 0:
			inv.doubt = "this janusctl has no command " + strings.Join(inv.path, " ") + " " + args[0].text
		case stdinArgs:
			inv.doubt = "its command comes from stdin"
		default:
			inv.effectOf = effectOf{effect: effectReads}
			inv.help = "its help"
		}
		return inv
	}
	inv.effectOf = effects[strings.Join(inv.path, " ")]
	if inv.effect == effectUnset {
		inv.doubt = "effects.go doesn't say what " + strings.Join(inv.path, " ") + " does"
		return inv
	}
	inv.help = c.help
	inv.localFiles = inv.localFiles || namesLocalFiles(c, args)
	return inv
}

func helpWord(s string) bool { return s == "-h" || s == "-help" || s == "--help" }

// namesLocalFiles: a flag or a positional argument of c names a file or
// a directory of this machine.
func namesLocalFiles(c *command, args []shWord) bool {
	pos := 0
	for k := 0; k < len(args); k++ {
		w := args[k].text
		if strings.HasPrefix(w, "-") && w != "-" {
			name, _, hasValue := strings.Cut(strings.TrimLeft(w, "-"), "=")
			fl := c.flag(name)
			if fl == nil {
				continue
			}
			if fl.kind == argFile || fl.kind == argDir {
				return true
			}
			if fl.value != "" && !hasValue {
				k++
			}
			continue
		}
		kind := argNone
		switch {
		case pos < len(c.pos):
			kind = c.pos[pos]
		case c.variadic && len(c.pos) > 0:
			kind = c.pos[len(c.pos)-1]
		}
		if kind == argFile || kind == argDir {
			return true
		}
		pos++
	}
	return false
}

// readsOnly: the whole line is janusctl reads - each command a janusctl
// invocation of its own that reads, run directly, naming no file here,
// redirecting nothing but to /dev/null or another descriptor.
func readsOnly(line string, invs []invocation) bool {
	parsed := parseShell(line)
	if parsed.err != nil || len(parsed.cmds) == 0 || len(invs) != len(parsed.cmds) {
		return false
	}
	for _, inv := range invs {
		if inv.doubt != "" || inv.effect != effectReads || !inv.direct || inv.localFiles || inv.cmd == nil {
			return false
		}
		for _, r := range inv.cmd.redirs {
			if !harmlessRedirect(r) {
				return false
			}
		}
		if slices.ContainsFunc(inv.cmd.words, func(w shWord) bool { return strings.Contains(w.text, "`") || strings.Contains(w.text, "$(") }) {
			return false
		}
	}
	return true
}

// harmlessRedirect: to /dev/null, or from a descriptor to another.
func harmlessRedirect(r shRedir) bool {
	op := strings.TrimLeft(r.op, "0123456789")
	switch op {
	case ">", ">>", "&>", "&>>", ">|":
		return r.target == "/dev/null"
	case ">&", "<&":
		t := strings.TrimSuffix(r.target, "-")
		return t == "" || strings.Trim(t, "0123456789") == ""
	}
	return false
}
