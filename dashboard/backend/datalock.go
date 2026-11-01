package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// dataLockFile is the file a Controller holds locked for as long as it
// runs on a data directory: two Controllers on one directory (a second
// container started by mistake, a `dashboardd restore` while the
// Controller runs, the same volume mounted on two hosts) would both run
// its loops - two writers of the same files, two reconcilers of the same
// clusters, two owners of the same machines.
const dataLockFile = ".dashboardd.lock"

// dataLock is the open lock file: kept here for the process's life (a
// file the garbage collector finalizes is closed - and its lock with it).
// Go opens it close-on-exec, so restartSelf's exec lets it go and the
// restarted Controller takes it again.
var dataLock *os.File

// lockDataDir takes dir's lock, or says who holds it. `dashboardd
// reset-user` never takes it: it's made to run beside the Controller.
func lockDataDir(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, dataLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := make([]byte, 256)
		n, _ := f.ReadAt(holder, 0)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			who := strings.TrimSpace(string(holder[:n]))
			if who == "" {
				who = "another process"
			}
			return fmt.Errorf("another Controller runs on %s (%s): stop it first - two on one data directory would both write it", dir, who)
		}
		return fmt.Errorf("lock %s: %w", dir, err)
	}
	host, _ := os.Hostname()
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(fmt.Appendf(nil, "pid %d on %s\n", os.Getpid(), host), 0)
	}
	dataLock = f
	return nil
}
