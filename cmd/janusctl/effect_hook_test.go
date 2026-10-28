package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The hook's answer for a command line Claude Code is about to run: ask
// before a critical command or one it can't read, nothing for the rest -
// allow for janusctl reads alone, when the user opted in.
func TestEffectDecisions(t *testing.T) {
	const ask, none, allow = "ask", "", "allow"
	for _, tc := range []struct {
		line       string
		want       string // allow_reads off
		wantReads  string // allow_reads on
		reasonPart string
	}{
		// Not janusctl at all.
		{"ls -la", none, none, ""},
		{"git status && go test ./...", none, none, ""},
		{`echo "it's fine"`, none, none, ""},
		{"cat <<'EOF'\nsomething's off\nEOF", none, none, ""},
		{"$EDITOR notes.txt", none, none, ""},
		{"echo 'unterminated", none, none, ""},

		// Reads.
		{"janusctl -n edge-1 system info", none, allow, ""},
		{"janusctl -n edge-1 haproxy backends && janusctl -n edge-1 system logs haproxy -n 200", none, allow, ""},
		{"timeout 10 janusctl -n edge-1 system events", none, allow, ""},
		{"timeout -s INT 10 janusctl -n edge-1 system events", none, allow, ""},
		{"janusctl -n=edge-1 tui -once", none, allow, ""},
		{"janusctl -context lab -all system info 2>&1", none, allow, ""},
		{"janusctl system info 2>/dev/null", none, allow, ""},
		{"janusctl help system reboot", none, allow, ""},
		{"janusctl system reboot -h", none, allow, ""},
		{"janusctl -n edge-1 system reboot --help", none, allow, ""},
		{"janusctl", none, allow, ""},
		{"janusctl system", none, allow, ""},
		{"janusctl version", none, allow, ""},
		{"/usr/bin/janusctl -n edge-1 haproxy get-config", none, allow, ""},
		{"for n in edge-1 edge-2; do janusctl -n $n system info; done", none, none, ""},
		// Reads, but not allowed even when opted in.
		{"janusctl -n edge-1 system logs haproxy | grep 503", none, none, ""},
		{"janusctl -n edge-1 haproxy get-config > haproxy.cfg", none, none, ""},
		{"janusctl -n edge-1 system support -o /tmp/b.tar.gz", none, none, ""},
		{"janusctl -n edge-1 system cp /etc/haproxy -o x.tar", none, none, ""},
		{"JANUSCONFIG=~/.config/janus/janusctl.json janusctl -n edge-1 system info", none, none, ""},
		{"janusctl -endpoint 10.0.0.1:9505 -ca ca.crt -cert a.crt -key a.key system info", none, none, ""},
		{"env janusctl system info", none, none, ""},
		{"janusctl system info; rm -rf build", none, none, ""},
		{"janusctl system cat $(rm -rf build)", none, none, ""},
		{`sh -c "janusctl system info; rm -rf build"`, none, none, ""},
		{"janusctl -n edge-1 system info &", none, allow, ""},

		// Changes: the permission rules decide.
		{"janusctl -n edge-1 haproxy apply-config haproxy.cfg", none, none, ""},
		{"janusctl -n edge-1 haproxy server-state app app2 drain", none, none, ""},
		{"janusctl -n edge-1 haproxy apply-config /dev/stdin <<'EOF'\nglobal\n  # it's the config\nEOF", none, none, ""},
		{"janusctl -n edge-1 network apply net.json && janusctl -n edge-1 network status", none, none, ""},

		// Critical: asked, however written.
		{"janusctl -n edge-1 system reboot", ask, ask, "system reboot on edge-1 is critical"},
		{"janusctl -all lifecycle upgrade https://example.com/r/", ask, ask, "on every node"},
		{"janusctl system info && janusctl -n edge-1 system reboot", ask, ask, "system reboot"},
		{"janusctl -n edge-1 system logs haproxy | head; janusctl -n edge-2 cluster drain", ask, ask, "cluster drain on edge-2"},
		{"JANUSCONFIG=/x janusctl -n edge-1 system reset -wipe-state", ask, ask, "system reset"},
		{"timeout 30 janusctl -n edge-1 lifecycle rollback", ask, ask, "lifecycle rollback"},
		{"env -i PATH=/usr/bin janusctl system shutdown", ask, ask, "system shutdown"},
		{"sudo -E janusctl system reboot", ask, ask, "system reboot"},
		{"nohup janusctl -n edge-1 system reboot >/dev/null 2>&1 &", ask, ask, "system reboot"},
		{"/usr/local/bin/janusctl access trust-reset", ask, ask, "access trust-reset"},
		{"./bin/janusctl -n edge-1 fleet adopt edge-1", ask, ask, "fleet adopt"},
		{"janusctl 'system' \"reboot\"", ask, ask, "system reboot"},
		{"jan''usctl system reboot", ask, ask, "system reboot"},
		{`j\anusctl system reboot`, ask, ask, "system reboot"},
		{"$'\\x6a'anusctl system reboot", ask, ask, "system reboot"},
		{"sh -c 'janusctl -n edge-1 system reboot'", ask, ask, "system reboot"},
		{"bash -lc \"janusctl -n edge-1 system reboot\"", ask, ask, "system reboot"},
		{"ssh admin@host 'janusctl system reboot'", ask, ask, "system reboot"},
		{"ssh admin@host janusctl system reboot", ask, ask, "system reboot"},
		{"echo $(janusctl -n edge-1 system reboot)", ask, ask, "system reboot"},
		{"echo `janusctl system reboot`", ask, ask, "system reboot"},
		{"cat <<EOF\n$(janusctl system reboot)\nEOF", ask, ask, "system reboot"},
		{"(cd /tmp && janusctl system reboot)", ask, ask, "system reboot"},
		{"if true; then janusctl system reboot; fi", ask, ask, "system reboot"},
		{"{ janusctl system reboot; }", ask, ask, "system reboot"},
		{"watch -n 5 janusctl -n edge-1 cluster undrain", ask, ask, "cluster undrain"},
		{"find . -name x -exec janusctl system reboot \\;", ask, ask, "system reboot"},
		{"go run ./cmd/janusctl -n edge-1 system reboot", ask, ask, "system reboot"},
		{"eval janusctl system reboot", ask, ask, "system reboot"},
		{"janusctl -n edge-1 system \\\n  reboot", ask, ask, "system reboot"},
		{"janusctl -n edge-1 pki generate-client-config -role os:admin -o x", ask, ask, "pki generate-client-config"},

		{"bash <<'EOF'\njanusctl -n edge-1 system reboot\nEOF", ask, ask, "system reboot on edge-1"},
		{"sh -s <<EOF\necho \"it's\"\njanusctl -n $N system shutdown\nEOF", ask, ask, "system shutdown"},
		{"bash <<< 'janusctl system reboot'", ask, ask, "system reboot"},
		{"sudo -u ops bash <<'EOF'\njanusctl -n edge-1 lifecycle rollback\nEOF", ask, ask, "lifecycle rollback"},
		{"sh <<EOF\njanusctl system info\nEOF", none, none, ""},
		{"cat <<'EOF' | grep reboot\njanusctl system reboot\nEOF", none, none, ""},

		// Can't tell: asked.
		{"echo 'janusctl system reboot' | sh", ask, ask, "from the pipe"},
		{"janusctl -n edge-1 haproxy get-config | bash", ask, ask, "from the pipe"},
		{"bash < reboot-all.sh # janusctl", ask, ask, "reboot-all.sh"},
		{"echo hi | bash -c cat", none, none, ""},
		{"echo ls | sh", none, none, ""},
		{"echo 'janusctl system reboot' | bash -o pipefail -e", ask, ask, "from the pipe"},
		{"bash ./check.sh janusctl", none, none, ""},
		{"J=janusctl; $J system reboot", ask, ask, "not known before it runs"},
		{"$JANUSCTL -n edge-1 system info", ask, ask, "not known before it runs"},
		{"$NODE_CMD -n edge-1 system info", none, none, ""},
		{`"${JANUSCTL:-janusctl}" system info`, ask, ask, "not known before it runs"},
		{"$(which janusctl) system info", ask, ask, "not known before it runs"},
		{"janusctl system $CMD", ask, ask, "stands where the command goes"},
		{"janusctl $FLAGS system info", ask, ask, "stands where the command goes"},
		{"janusctl -$F edge-1 system info", ask, ask, "global flag"},
		{"janusctl sys* reboot", ask, ask, "stands where the command goes"},
		{"janusctl system {reboot,info}", ask, ask, "stands where the command goes"},
		{"echo reboot | xargs janusctl system", ask, ask, "comes from stdin"},
		{"echo edge-1 | xargs -I{} janusctl -n {} system reboot", ask, ask, "system reboot"},
		{"janusctl system rebooot", ask, ask, "no command system rebooot"},
		{"janusctl frobnicate", ask, ask, "no command frobnicate"},
		{"janusctl -frobnicate system info", ask, ask, "no global flag"},
		{"janusctl system reboot 'unterminated", ask, ask, "couldn't read it"},
		{"echo $(janusctl system info", ask, ask, "couldn't read it"},

		// janusctl named, not run.
		{"grep -rn janusctl docs/", none, none, ""},
		{"rg janusctl $DIR", none, none, ""},
		{"go test ./cmd/janusctl -run TestSkillCommands", none, none, ""},
		{"go build -o bin/janusctl ./cmd/janusctl", none, none, ""},
		{"which janusctl", none, none, ""},
		{"ls -l $(which janusctl)", none, none, ""},
		{"git log --oneline -- cmd/janusctl/effects.go", none, none, ""},
		{`git commit -m "janusctl: a hook asks janusctl what a command does"`, none, none, ""},
		{"cat claude-code/janusctl/hooks/hooks.json", none, none, ""},
		{`git commit -m "claude-code: janusctl system reboot is asked now"`, none, none, ""},
		{`gh pr create --body "janusctl -n edge-1 system reboot: asked"`, none, none, ""},
		{`echo "run janusctl -n edge-1 system reboot yourself"`, none, none, ""},
		{"docker exec c sh -c 'janusctl system reboot'", ask, ask, "system reboot"},
		{`sudo bash -c "janusctl -n edge-1 lifecycle rollback"`, ask, ask, "lifecycle rollback"},
		{`git commit -m "$(janusctl system reboot)"`, ask, ask, "system reboot"},
	} {
		got, reason := decide(tc.line, false)
		if got != tc.want || (tc.reasonPart != "" && !strings.Contains(reason, tc.reasonPart)) {
			t.Errorf("%q: %q (%s), want %q with %q", tc.line, got, reason, tc.want, tc.reasonPart)
		}
		got, reason = decide(tc.line, true)
		if got != tc.wantReads {
			t.Errorf("%q with allow_reads: %q (%s), want %q", tc.line, got, reason, tc.wantReads)
		}
	}
}

// The hook speaks Claude Code's PreToolUse JSON: a decision, or nothing
// at all - never for another tool, never on input it can't read.
func TestEffectHookJSON(t *testing.T) {
	run := func(in string, allowReads bool) string {
		var out bytes.Buffer
		effectHook(strings.NewReader(in), &out, allowReads)
		return out.String()
	}
	in := `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"janusctl -n edge-1 system reboot","description":"Reboot"},"tool_use_id":"t"}`
	var got struct {
		HookSpecificOutput struct {
			HookEventName, PermissionDecision, PermissionDecisionReason string
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(run(in, false)), &got); err != nil {
		t.Fatal(err)
	}
	h := got.HookSpecificOutput
	if h.HookEventName != "PreToolUse" || h.PermissionDecision != "ask" || !strings.Contains(h.PermissionDecisionReason, "janusctl system reboot on edge-1 is critical - soft-stop HAProxy, then reboot") {
		t.Errorf("critical: %+v", h)
	}
	read := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"janusctl -n edge-1 system info"}}`
	if out := run(read, false); out != "" {
		t.Errorf("a read without the option: %q", out)
	}
	if out := run(read, true); !strings.Contains(out, `"permissionDecision":"allow"`) {
		t.Errorf("a read with the option: %q", out)
	}
	for _, in := range []string{
		`{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/x"}}`,
		`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"janusctl system reboot"}}`,
		`not json`,
		``,
	} {
		if out := run(in, true); out != "" {
			t.Errorf("%q: %q", in, out)
		}
	}
}

// Every critical command of the tree is asked, every read and change is
// left alone - written plainly, behind -n, and as sh -c would run it.
func TestEffectCoversTheTree(t *testing.T) {
	commands.leaves(nil, func(path []string, c *command) {
		e := effects[strings.Join(path, " ")]
		want := ""
		if e.effect == effectCritical {
			want = "ask"
		}
		cmd := strings.Join(path, " ")
		for _, line := range []string{"janusctl " + cmd, "janusctl -n edge-1 " + cmd, "sh -c 'janusctl -context lab " + cmd + "'"} {
			if got, reason := decide(line, false); got != want {
				t.Errorf("%q: %q (%s), want %q", line, got, reason, want)
			}
		}
	})
}

func TestParseShell(t *testing.T) {
	for line, want := range map[string]string{
		`a b | c && d; e`:           "a,b|c|d|e",
		`a "b c" 'd e' f\ g`:        "a,b c,d e,f g",
		`a $(b c) d`:                "b,c|a,$(b c),d",
		"a <<EOF\nb $(c)\nEOF\nd":   "a|c|d",
		"a <<'EOF'\nb $(c)\nEOF\nd": "a|d",
		`a 2>&1 >/dev/null <in b`:   "a,b",
		`x=1 y=2 a`:                 "x=1,y=2,a",
		`a # b c`:                   "a",
		`a#b`:                       "a#b",
		`$'a\tb' $"c"`:              "a\tb,c",
		`(a; b) | c`:                "a|b|c",
		`a <(b) >(c)`:               "b|c|a,/dev/fd/63,/dev/fd/63",
		"case x in a) b;; esac":     "!",
		`a "$(b "c d")" e`:          "b,c d|a,$(b \"c d\"),e",
		`a ${x:-$(b)}`:              "b|a,${x:-$(b)}",
		`a $((1 + $(b)))`:           "b|a,$((1 + $(b)))",
		"a \\\nb":                   "a,b",
		`a "b \" c" \$d`:            `a,b " c,$d`,
		`a &>/dev/null & b`:         "a|b",
	} {
		p := parseShell(line)
		got := ""
		if p.err != nil {
			got = "!"
		} else {
			var cmds []string
			for _, c := range p.cmds {
				var ws []string
				for _, w := range c.words {
					ws = append(ws, w.text)
				}
				cmds = append(cmds, strings.Join(ws, ","))
			}
			got = strings.Join(cmds, "|")
		}
		if got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}
