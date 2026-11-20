package haproxy

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HandoverConfigName is the configuration file the Manager writes beside
// ConfigPath when Handover is set: the socket's declaration, read after
// haproxy.cfg and ExtraConfigs.
const HandoverConfigName = "janus-handover.cfg"

// handoverConfig writes the configuration declaring the Handover socket,
// if it doesn't say so already, and returns its path. Written to a
// temporary file of its own renamed over it: a validation may read it -
// or write it - while a start writes it.
func (m *Manager) handoverConfig() (string, error) {
	path := filepath.Join(filepath.Dir(m.ConfigPath), HandoverConfigName)
	want := []byte("# Written by janusd: the socket a HAProxy hands its listeners over to the\n" +
		"# next one through at a reload (-x). Not haproxy.cfg's to declare.\n" +
		"global\n" +
		"    stats socket " + m.Handover + " mode 600 level user expose-fd listeners\n")
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return path, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+HandoverConfigName+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // gone once renamed
	_, err = tmp.Write(want)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

// listenWords are the configuration keywords that shape the listening
// sockets when HAProxy opens them - and only then: a socket handed over
// keeps what the previous process opened it with. HAProxy doesn't
// listen() again on a socket already listening: a bind's backlog changed
// from 500 to 7 stayed 500 across a reload with -x, became 7 without
// (3.4.8, measured) - net.core.somaxconn, which caps it, the same.
// Generous on purpose: a word too many only costs a reload the hand-over.
var listenWords = map[string]bool{
	"bind": true, "backlog": true, "maxconn": true, "unix-bind": true,
	"nbthread": true, "thread-groups": true, "cpu-map": true, "cpu-policy": true, "cpu-set": true,
}

// listenFingerprint is what the listening sockets of a HAProxy started
// with args would be opened with: the lines of its configuration files
// with a listenWords or tune.* keyword or a stats socket, under their
// sections, args
// themselves (-L names the local peer) and net.core.somaxconn. "" when a
// file can't be read: unknown, no hand-over.
func listenFingerprint(args []string) string {
	var b strings.Builder
	somaxconn, err := os.ReadFile(filepath.Join(ProcRoot, "sys/net/core/somaxconn"))
	if err != nil {
		return ""
	}
	b.WriteString("somaxconn " + strings.TrimSpace(string(somaxconn)) + "\nargs " + strings.Join(args, " ") + "\n")
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-f" {
			continue
		}
		data, err := os.ReadFile(args[i+1])
		if err != nil {
			return ""
		}
		b.WriteString("file " + args[i+1] + "\n")
		for _, raw := range strings.Split(string(data), "\n") {
			fields := parseLine(raw)
			if len(fields) == 0 {
				continue
			}
			w := fields[0]
			if sectionKeywords[w] || listenWords[w] || strings.HasPrefix(w, "tune.") || w == "stats" && len(fields) > 1 && fields[1] == "socket" {
				b.WriteString(strings.Join(fields, " ") + "\n")
			}
		}
	}
	return b.String()
}

// handsOver reports whether HAProxy pid answers on the Handover socket:
// started with it, it can hand its listeners over. -x to a socket that
// isn't there, or another process's, makes the new HAProxy exit
// ("Failed to connect to the old process socket", measured on 3.4.8) -
// a process started by an older janusd, or before Handover was set,
// gets a plain -sf.
func (m *Manager) handsOver(pid int) bool {
	if m.Handover == "" {
		return false
	}
	out, err := m.statsCommandAt(m.Handover, "show info")
	return err == nil && parseShowInfo(string(out)).Pid == pid
}

// waitReleased waits, until deadline, for HAProxy old to let go of the
// listening sockets it handed over to p: until it does, both accept on
// them - janusd's stats socket among them, so a runtime command could
// still reach old. It lets go at its soft-stop, which -sf asks for once
// p has started (measured on 3.4.8: answers from the previous process
// on the shared stats socket during the first ~30 ms, none after, even
// with a client keeping it alive). Called with mu held.
func (m *Manager) waitReleased(old int, p *process, deadline time.Time) {
	for {
		shared := sharedSockets(old, p.cmd.Process.Pid)
		if shared == 0 || p.exited() {
			return
		}
		if !time.Now().Before(deadline) {
			log.Printf("haproxy: pid %d still holds %d of the listening sockets it handed over to pid %d", old, shared, p.cmd.Process.Pid)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sharedSockets counts the sockets processes a and b both hold: the
// listeners one handed over to the other (-x). An exited process holds
// none.
func sharedSockets(a, b int) int {
	sa := socketInodes(a)
	n := 0
	for ino := range socketInodes(b) {
		if sa[ino] {
			n++
		}
	}
	return n
}

// socketInodes are the sockets pid holds, by inode (its fds' links,
// "socket:[N]").
func socketInodes(pid int) map[string]bool {
	dir := filepath.Join(ProcRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err == nil && strings.HasPrefix(link, "socket:[") {
			out[link] = true
		}
	}
	return out
}

// HandoverSocketFor is the Handover socket janusd uses beside its stats
// socket.
func HandoverSocketFor(statsSocket string) string {
	return filepath.Join(filepath.Dir(statsSocket), "haproxy-handover.sock")
}

func (m *Manager) handoverArgs() []string {
	if m.Handover == "" {
		return nil
	}
	path, err := m.handoverConfig()
	if err != nil {
		// HAProxy then runs without it: reloads close the listeners, as
		// before.
		log.Printf("haproxy: write %s: %v - reloads hand no listener over", HandoverConfigName, err)
		return nil
	}
	return []string{"-f", path}
}
