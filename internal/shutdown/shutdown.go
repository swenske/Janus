// Package shutdown holds what janusd does before the machine reboots or
// powers off, whichever way it goes down: the Reboot, Shutdown, Reset and
// Upgrade calls, a failed boot's revert, init's SIGTERM.
package shutdown

import "sync"

var (
	mu    sync.Mutex
	hooks []func()
	done  bool
)

// Before adds f to what Run runs.
func Before(f func()) {
	mu.Lock()
	defer mu.Unlock()
	hooks = append(hooks, f)
}

// Run runs the hooks in the order they were added - once: the calls
// after the first do nothing.
func Run() {
	mu.Lock()
	if done {
		mu.Unlock()
		return
	}
	done = true
	hs := hooks
	mu.Unlock()
	for _, f := range hs {
		f()
	}
}
