package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/termui"
)

// The dashboard's actions: each one a dialog that says what it will do,
// confirmed with Enter, run in the background, its answer in the
// footer. What the certificate's role can't do isn't offered; with an
// unknown role everything is, and the node's refusal is shown.

// action is one thing the dashboard can ask a node.
type action struct {
	title   string
	prompt  string
	danger  bool     // the dialog starts on Cancel
	choices []string // the last one cancels
	need    []string // the roles allowed (rbac): operators or admins
	run     func(ctx context.Context, conn *grpc.ClientConn, choice int) (string, error)
}

// actionResult is what an action answered.
type actionResult struct {
	title string
	text  string
	err   error
}

// modal is the dialog of an action, over the screen.
type modal struct {
	act    action
	s      *sampler
	cursor int
	busy   bool
}

const actionTimeout = 10 * time.Second

// open shows an action's dialog for a node, if the role may.
func (a *app) open(act action, s *sampler) {
	if !roleAllows(a.opts.role, act.need) {
		a.say(fmt.Sprintf("%s can't: %s (%s)", a.opts.role, act.title, strings.Join(act.need, " or ")), termui.ColorWarn)
		return
	}
	m := &modal{act: act, s: s}
	if act.danger {
		m.cursor = len(act.choices) - 1
	}
	a.modal = m
}

// key moves between the choices, Enter runs the one chosen, Esc leaves.
func (m *modal) key(a *app, k string) {
	if m.busy {
		return
	}
	switch k {
	case termui.KeyLeft, termui.KeyUp, termui.KeyBackTab:
		m.cursor = (m.cursor + len(m.act.choices) - 1) % len(m.act.choices)
	case termui.KeyRight, termui.KeyDown, termui.KeyTab:
		m.cursor = (m.cursor + 1) % len(m.act.choices)
	case termui.KeyEsc, "n", "q":
		a.modal = nil
	case termui.KeyEnter, " ":
		if m.cursor == len(m.act.choices)-1 {
			a.modal = nil
			return
		}
		m.busy = true
		go m.run(a, m.cursor)
	case "y":
		if len(m.act.choices) == 2 {
			m.busy = true
			go m.run(a, 0)
		}
	}
}

// run calls the node and reports on a.actions.
func (m *modal) run(a *app, choice int) {
	res := actionResult{title: m.act.title}
	conn, err := m.s.connection()
	if err != nil {
		res.err = err
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		res.text, res.err = m.act.run(ctx, conn, choice)
	}
	a.actions <- res
}

// finish closes the dialog and says what happened.
func (a *app) finish(res actionResult) {
	s := a.modal.s
	a.modal = nil
	switch {
	case res.err == nil:
		a.say(res.text, termui.ColorOK)
	case status.Code(res.err) == codes.PermissionDenied:
		a.say(res.title+": the node refused: "+status.Convert(res.err).Message(), termui.ColorDanger)
	default:
		a.say(res.title+": "+status.Convert(res.err).Message(), termui.ColorDanger)
	}
	if s != nil {
		s.Kick()
	}
}

// render draws the dialog in the middle of the frame.
func (m *modal) render(f *termui.Frame) {
	w := max(len([]rune(m.act.prompt)), len(strings.Join(m.act.choices, "    "))) + 6
	w = min(max(w, 36), f.W-4)
	h := 7
	r := termui.Rect{X: (f.W - w) / 2, Y: (f.H - h) / 2, W: w, H: h}
	f.Fill(r, ' ', termui.Style{})
	title := styleTitle
	if m.act.danger {
		title = termui.Style{FG: termui.ColorDanger, Bold: true}
	}
	in := f.Box(r, m.act.title, styleFocus, title)
	f.Text(in.X+1, in.Y+1, termui.Truncate(m.act.prompt, in.W-2), termui.Style{}, in.W-2)
	if m.busy {
		f.Text(in.X+1, in.Y+3, "asking the node…", styleMuted, in.W-2)
		return
	}
	x := in.X + 1
	for i, c := range m.act.choices {
		st := termui.Style{FG: termui.ColorKey}
		label := " " + c + " "
		if i == m.cursor {
			st = styleCursor
			st.Bold = true
			if m.act.danger && i != len(m.act.choices)-1 {
				st.FG = termui.ColorDanger
			}
		}
		x += f.Text(x, in.Y+3, label, st, in.W-(x-in.X)) + 2
	}
}

// The actions themselves.

var serverStates = []janusv1alpha1.ServerSetStateRequest_State{janusv1alpha1.ServerSetStateRequest_STATE_READY, janusv1alpha1.ServerSetStateRequest_STATE_DRAIN, janusv1alpha1.ServerSetStateRequest_STATE_MAINT}

func serverAction(sv serverRow) action {
	name := sv.backend + "/" + sv.server
	return action{
		title:   "Server " + name,
		prompt:  fmt.Sprintf("%s is %s - set it:", name, firstOr(sv.status, "unknown")),
		choices: []string{"Ready", "Drain", "Maint", "Cancel"},
		need:    rolesOperators,
		run: func(ctx context.Context, conn *grpc.ClientConn, choice int) (string, error) {
			_, err := janusv1alpha1.NewHAProxyServiceClient(conn).ServerSetState(ctx, &janusv1alpha1.ServerSetStateRequest{Backend: sv.backend, Server: sv.server, State: serverStates[choice]})
			if err != nil {
				return "", err
			}
			return name + ": " + strings.ToLower([]string{"ready", "drain", "maint"}[choice]), nil
		},
	}
}

var reloadAction = action{
	title:   "Reload HAProxy",
	prompt:  "Reload HAProxy with its current configuration (seamless)?",
	choices: []string{"Reload", "Cancel"},
	need:    rolesOperators,
	run: func(ctx context.Context, conn *grpc.ClientConn, _ int) (string, error) {
		resp, err := janusv1alpha1.NewHAProxyServiceClient(conn).Reload(ctx, &emptypb.Empty{})
		if err != nil {
			return "", err
		}
		if !resp.GetSuccess() {
			return "", errors.New(firstOr(resp.GetMessage(), "the reload failed"))
		}
		return "HAProxy reloaded", nil
	},
}

var rebootModes = []janusv1alpha1.RebootMode{janusv1alpha1.RebootMode_REBOOT_MODE_DEFAULT, janusv1alpha1.RebootMode_REBOOT_MODE_POWERCYCLE, janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC}

var rebootAction = action{
	title:   "Reboot the node",
	prompt:  "HAProxy drains and stops, then the node reboots. kexec skips the firmware.",
	danger:  true,
	choices: []string{"Reboot", "Power cycle", "kexec", "Cancel"},
	need:    rolesOperators,
	run: func(ctx context.Context, conn *grpc.ClientConn, choice int) (string, error) {
		_, err := janusv1alpha1.NewSystemServiceClient(conn).Reboot(ctx, &janusv1alpha1.RebootRequest{Mode: rebootModes[choice]})
		if err != nil {
			return "", err
		}
		return "rebooting (" + strings.ToLower([]string{"firmware", "power cycle", "kexec"}[choice]) + ")", nil
	},
}

var restartAction = action{
	title:   "Restart janusd",
	prompt:  "janusd exits and init starts it again; HAProxy keeps serving.",
	danger:  true,
	choices: []string{"Restart", "Cancel"},
	need:    rolesOperators,
	run: func(ctx context.Context, conn *grpc.ClientConn, _ int) (string, error) {
		_, err := janusv1alpha1.NewSystemServiceClient(conn).Restart(ctx, &emptypb.Empty{})
		if err != nil {
			return "", err
		}
		return "janusd restarting", nil
	},
}

// pendingTrials are the trials the node has on, from the slow info.
func pendingTrials(v *nodeView) []string {
	var out []string
	if v.slow.network.GetTrialPending() {
		out = append(out, "network")
	}
	if v.slow.firewall.GetTrialPending() {
		out = append(out, "firewall")
	}
	if v.slow.sysctl != nil {
		out = append(out, "sysctl")
	}
	return out
}

// confirmAction keeps a trial's changes: the network configuration,
// the firewall ruleset or the sysctl values on trial.
func confirmAction(trials []string) action {
	return action{
		title:   "Confirm a trial",
		prompt:  "Keep the changes on trial (they revert otherwise): " + strings.Join(trials, ", "),
		choices: append(append([]string{}, trials...), "Cancel"),
		need:    rolesAdmins,
		run: func(ctx context.Context, conn *grpc.ClientConn, choice int) (string, error) {
			switch trials[choice] {
			case "network":
				resp, err := janusv1alpha1.NewNetworkServiceClient(conn).NetworkConfigConfirm(ctx, &emptypb.Empty{})
				if err != nil {
					return "", err
				}
				return "network configuration confirmed via " + resp.GetConfirmedVia(), nil
			case "firewall":
				if _, err := janusv1alpha1.NewNetworkServiceClient(conn).FirewallConfirm(ctx, &emptypb.Empty{}); err != nil {
					return "", err
				}
				return "firewall ruleset confirmed", nil
			default:
				if _, err := janusv1alpha1.NewSystemServiceClient(conn).SysctlConfirm(ctx, &emptypb.Empty{}); err != nil {
					return "", err
				}
				return "sysctl values confirmed", nil
			}
		},
	}
}

// actionHints are the footer's keys for what the role may do.
func (n *nodeScreen) actionHints(a *app) []string {
	var h []string
	if roleAllows(a.opts.role, rolesOperators) {
		if n.focus == panelHAProxy {
			h = append(h, "Enter server")
		}
		h = append(h, "R reload", "B reboot", "J restart")
	}
	if v := n.view(); roleAllows(a.opts.role, rolesAdmins) && len(pendingTrials(&v)) > 0 {
		h = append(h, "C confirm")
	}
	return h
}

// actionKey handles the keys that act; false when k isn't one.
func (n *nodeScreen) actionKey(a *app, k string) bool {
	switch k {
	case termui.KeyEnter:
		if n.focus != panelHAProxy {
			return false
		}
		if sv := n.selectedServer(); sv != nil {
			a.open(serverAction(*sv), n.s)
		} else {
			a.say("select a server (↑↓) to set it ready, drain or maint", termui.ColorDefault)
		}
	case "R":
		a.open(reloadAction, n.s)
	case "B":
		a.open(rebootAction, n.s)
	case "J":
		a.open(restartAction, n.s)
	case "C":
		v := n.view()
		trials := pendingTrials(&v)
		if len(trials) == 0 {
			a.say("no trial pending on this node", termui.ColorDefault)
			return true
		}
		a.open(confirmAction(trials), n.s)
	default:
		return false
	}
	return true
}
