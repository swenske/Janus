package main

import (
	"bytes"
	"regexp"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
)

// While a machine waits for its node to register, the Controller reads
// the node's serial console: a node that can't register says why there
// (janusd's "selfregister:" lines), and the machine's card shows it as a
// warning with a hint - instead of a silent wait until the timeout. The
// usual cause is the network: no route to the Controller (a DHCP server
// handing out no gateway), no DNS for its name, a firewall.

// watchRetry is the wait before reading a console again after it closed
// or was refused (someone else holds it: virsh console on the host).
var watchRetry = 15 * time.Second

// watchTick is how often a watcher checks its machine still waits.
var watchTick = 5 * time.Second

// watchNode reads id's console while the machine waits for its node.
func (r *machineRunner) watchNode(id string) {
	r.mu.Lock()
	if r.watching == nil {
		r.watching = map[string]bool{}
	}
	if r.watching[id] {
		r.mu.Unlock()
		return
	}
	r.watching[id] = true
	r.watchers.Add(1)
	r.mu.Unlock()

	waiting := func() (*machines.Machine, bool) {
		m, ok := r.a.machines.Get(id)
		return m, ok && m.Phase == machines.PhaseRegistering
	}
	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.watching, id)
			r.mu.Unlock()
			r.watchers.Done()
		}()
		tick := time.NewTicker(watchTick)
		defer tick.Stop()
		for {
			m, ok := waiting()
			if !ok {
				return
			}
			open, err := r.a.machineConsole(m)
			if err != nil {
				return
			}
			sub := r.a.consoles.subscribe(id, open, &consoleLines{line: func(l string) { r.nodeSaid(id, l) }})
		read:
			for {
				select {
				case <-sub.done:
					break read
				case <-r.quit:
					r.a.consoles.unsubscribe(sub)
					return
				case <-tick.C:
					if _, ok := waiting(); !ok {
						r.a.consoles.unsubscribe(sub)
						return
					}
				}
			}
			select {
			case <-time.After(watchRetry):
			case <-r.quit:
				return
			}
		}
	}()
}

// stopWatching stops every console reader and waits for them - tests,
// before they put back what the readers use.
func (r *machineRunner) stopWatching() {
	r.stopOnce.Do(func() { close(r.quit) })
	r.watchers.Wait()
}

// nodeSaid takes in one line of a waiting node's console.
func (r *machineRunner) nodeSaid(id, line string) {
	warning, note := registrationWarning(line)
	if warning == "" {
		return
	}
	_, _ = r.a.machines.Update(id, func(m *machines.Machine) error {
		if m.NodeID != "" || m.Warning == warning || (m.Phase != machines.PhaseRegistering && m.Phase != machines.PhaseFailed) {
			return errSkip
		}
		m.Warning = warning
		m.Log("the node says: %s", note)
		return nil
	})
}

var retryingRe = regexp.MustCompile(` - retrying in [0-9a-zµ.]+$`)

// registrationWarning reads a console line: a node's failed registration
// gives the machine's warning, and what it said for its history; any
// other line gives nothing.
func registrationWarning(line string) (warning, said string) {
	_, msg, ok := strings.Cut(line, "selfregister: ")
	if !ok {
		return "", ""
	}
	msg = strings.TrimSpace(msg)
	nextBoot := false
	switch {
	case retryingRe.MatchString(msg):
		// A newer node tries again on its own.
		said = retryingRe.ReplaceAllString(msg, "")
	case strings.HasPrefix(msg, "registration failed, will retry on next boot: "):
		said, nextBoot = strings.TrimPrefix(msg, "registration failed, will retry on next boot: "), true
	case strings.HasPrefix(msg, "determine address to advertise to Controller at "):
		said, nextBoot = "reach the Controller at "+strings.TrimPrefix(msg, "determine address to advertise to Controller at "), true
	case strings.HasPrefix(msg, "successfully announced to Controller at "):
		return "The node registered, but not on its token (expired, or already used): it waits for approval under Nodes - approving it links it to this machine.",
			"registered without its token - waiting for approval"
	default:
		return "", ""
	}
	warning = "The node can't register: " + said + "."
	if hint := registrationHint(said); hint != "" {
		warning += " " + hint
	}
	if nextBoot {
		warning += " This image only tries again at its next boot: reset the machine once the node can reach the Controller."
	}
	return warning, said
}

// registrationHint is the likely cause of a registration error.
func registrationHint(err string) string {
	e := strings.ToLower(err)
	switch {
	case strings.Contains(e, "lookup "), strings.Contains(e, "no such host"):
		return "Its DNS doesn't resolve the Controller's name: give it a DNS server that does, or make the hypervisor's Controller address an IP address."
	case strings.Contains(e, "network is unreachable"), strings.Contains(e, "no route to host"):
		return "It has no route to the Controller: give it a gateway on an interface that leads there - a DHCP server may hand out none."
	case strings.Contains(e, "connection refused"):
		return "Nothing listens there: check the hypervisor's Controller address - the Controller's registration port is 8443 by default."
	case strings.Contains(e, "x509"), strings.Contains(e, "certificate"):
		return "The Controller's certificate doesn't name that address: register at one it names (-advertise-address)."
	case strings.Contains(e, "timeout"), strings.Contains(e, "deadline exceeded"):
		return "Nothing answered in time: a firewall on the way may drop it."
	}
	return ""
}

// consoleLines calls line for each line a console prints, its terminal
// sequences removed.
type consoleLines struct {
	buf  []byte
	line func(string)
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

const maxConsoleLine = 4 << 10

func (c *consoleLines) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			break
		}
		c.line(strings.TrimRight(ansiRe.ReplaceAllString(string(c.buf[:i]), ""), "\r"))
		c.buf = c.buf[i+1:]
	}
	if len(c.buf) > maxConsoleLine {
		c.buf = c.buf[:0] // a line this long is no janusd log line
	}
	return len(p), nil
}
