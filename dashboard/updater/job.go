package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/updater/updaterapi"
)

// errBusy: an update is already running.
var errBusy = errors.New("an update is already running")

const maxLogLines = 300

// updater runs updates, one at a time, and remembers the last one.
type updater struct {
	cfg      config
	p        platform
	checkins *checkins

	mu  sync.Mutex
	job *updaterapi.Job

	// The compose check is cached: the Controller's page asks for the
	// status every few seconds.
	composeAt  time.Time
	composeErr error
}

func newUpdater(cfg config, p platform) *updater {
	u := &updater{cfg: cfg, p: p, checkins: newCheckins()}
	u.load()
	return u
}

// status is what GET /v1/status returns.
func (u *updater) status(ctx context.Context) updaterapi.Status {
	st := updaterapi.Status{
		Version:    version,
		Service:    u.cfg.Service,
		Variable:   u.cfg.Variable,
		Repository: u.cfg.Repository,
	}
	t, problems := u.p.discover(ctx)
	if t != nil {
		st.Project, st.WorkingDir, st.CurrentImage = t.Project, t.WorkingDir, t.Image
	}
	if t != nil && len(problems) == 0 {
		u.mu.Lock()
		stale := time.Since(u.composeAt) > time.Minute
		u.mu.Unlock()
		if stale {
			err := u.p.checkCompose(ctx, t)
			u.mu.Lock()
			u.composeAt, u.composeErr = time.Now(), err
			u.mu.Unlock()
		}
		u.mu.Lock()
		if u.composeErr != nil {
			problems = append(problems, u.composeErr.Error())
		}
		u.mu.Unlock()
	}
	st.Problems = problems
	st.Ready = len(problems) == 0
	u.mu.Lock()
	st.Job = u.copyJob()
	u.mu.Unlock()
	return st
}

// start checks req and starts the update in the background.
func (u *updater) start(req updaterapi.UpdateRequest) (*updaterapi.Job, error) {
	image, err := updaterapi.CheckImage(req.Image, u.cfg.Repository, req.Version)
	if err != nil {
		return nil, err
	}
	if req.Version == req.FromVersion {
		return nil, fmt.Errorf("the Controller already runs %s", req.Version)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.job != nil && u.job.State == updaterapi.JobRunning {
		return nil, errBusy
	}
	now := time.Now().UTC()
	u.job = &updaterapi.Job{
		ID:          now.Format("20060102T150405.000Z"),
		FromVersion: req.FromVersion,
		ToVersion:   req.Version,
		ToImage:     image,
		State:       updaterapi.JobRunning,
		Step:        "check",
		Message:     "Checking the setup",
		StartedAt:   now,
	}
	u.save()
	job := u.copyJob()
	go u.run()
	return job, nil
}

// run is one update, start to finish. Nothing changes before the stop
// step; from the switch on, any failure rolls back.
func (u *updater) run() {
	ctx := context.Background()
	u.mu.Lock()
	job := *u.job
	u.mu.Unlock()
	u.logf("update from %s to %s (%s)", job.FromVersion, job.ToVersion, job.ToImage)

	t, problems := u.p.discover(ctx)
	if len(problems) == 0 {
		if err := u.p.checkCompose(ctx, t); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		u.finish(updaterapi.JobFailed, "Can't update: "+strings.Join(problems, "; "))
		return
	}
	u.edit(func(j *updaterapi.Job) { j.FromImage = t.Image })

	u.step("pull", "Pulling "+job.ToImage)
	if out, err := u.composeLogged(ctx, 15*time.Minute, t, []string{u.cfg.Variable + "=" + job.ToImage}, "pull", t.Service); err != nil {
		u.finish(updaterapi.JobFailed, fmt.Sprintf("Pulling %s failed: %s", job.ToImage, lastLines(out, 2)))
		return
	}

	u.step("stop", "Stopping the Controller")
	if out, err := u.composeLogged(ctx, 2*time.Minute, t, nil, "stop", "--timeout", "30", t.Service); err != nil {
		u.startPrevious(ctx, t)
		u.finish(updaterapi.JobFailed, "Stopping the Controller failed: "+lastLines(out, 2))
		return
	}

	u.step("backup", "Backing up the Controller's data and .env")
	envPath := filepath.Join(t.WorkingDir, ".env")
	envData, envExisted, err := readOptional(envPath)
	if err == nil {
		err = u.backup(job, t.Image, envData, envExisted)
	}
	if err != nil {
		u.startPrevious(ctx, t)
		u.finish(updaterapi.JobFailed, "Backing up failed, nothing was changed: "+err.Error())
		return
	}

	u.step("switch", "Starting "+job.ToVersion)
	if err := writeFileAtomic(envPath, setEnvVar(envData, u.cfg.Variable, job.ToImage), 0o644); err != nil {
		u.rollback(ctx, t, job, envData, envExisted, "Writing .env failed: "+err.Error())
		return
	}
	since := time.Now()
	if out, err := u.composeLogged(ctx, 5*time.Minute, t, nil, "up", "--detach", "--no-deps", t.Service); err != nil {
		u.rollback(ctx, t, job, envData, envExisted, "Starting "+job.ToVersion+" failed: "+lastLines(out, 2))
		return
	}

	u.step("wait", "Waiting for "+job.ToVersion+" to start")
	wctx, cancel := context.WithTimeout(ctx, u.cfg.StartTimeout)
	at, err := u.checkins.wait(wctx, job.ToVersion, since)
	cancel()
	if err != nil {
		u.rollback(ctx, t, job, envData, envExisted, fmt.Sprintf("%s didn't start within %s", job.ToVersion, u.cfg.StartTimeout))
		return
	}
	u.logf("%s started", job.ToVersion)
	// Started, and still running a little later - not restarted since.
	time.Sleep(u.cfg.Stable)
	running, startedAt, err := u.p.containerState(ctx, t)
	if err != nil || !running || startedAt.After(at) {
		u.rollback(ctx, t, job, envData, envExisted, job.ToVersion+" stopped right after starting")
		return
	}
	u.finish(updaterapi.JobDone, "Updated to "+job.ToVersion)
}

// rollback puts the previous data, .env and image back.
func (u *updater) rollback(ctx context.Context, t *target, job updaterapi.Job, envData []byte, envExisted bool, reason string) {
	u.step("rollback", reason+" - rolling back to "+job.FromVersion)
	if out, err := u.composeLogged(ctx, 2*time.Minute, t, nil, "stop", "--timeout", "10", t.Service); err != nil {
		u.logf("stop: %v: %s", err, lastLines(out, 2))
	}
	var errs []string
	if err := replaceContents(u.cfg.DataDir, filepath.Join(u.cfg.StateDir, "backup", "data")); err != nil {
		errs = append(errs, "restoring the data: "+err.Error())
	}
	envPath := filepath.Join(t.WorkingDir, ".env")
	if envExisted {
		if err := writeFileAtomic(envPath, envData, 0o644); err != nil {
			errs = append(errs, "restoring .env: "+err.Error())
		}
	} else if err := os.Remove(envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, "removing .env: "+err.Error())
	}
	if len(errs) > 0 {
		u.finish(updaterapi.JobRollbackFailed, reason+"; rolling back failed: "+strings.Join(errs, "; "))
		return
	}
	since := time.Now()
	if out, err := u.composeLogged(ctx, 5*time.Minute, t, nil, "up", "--detach", "--no-deps", t.Service); err != nil {
		u.finish(updaterapi.JobRollbackFailed, reason+"; starting the previous version failed: "+lastLines(out, 2))
		return
	}
	wctx, cancel := context.WithTimeout(ctx, u.cfg.StartTimeout)
	_, err := u.checkins.wait(wctx, job.FromVersion, since)
	cancel()
	if err != nil {
		u.finish(updaterapi.JobRollbackFailed, fmt.Sprintf("%s; the previous version (%s) didn't start again within %s", reason, job.FromVersion, u.cfg.StartTimeout))
		return
	}
	u.finish(updaterapi.JobRolledBack, reason+" - "+job.FromVersion+" is back, with its data")
}

// startPrevious restarts the Controller after a failure before anything
// changed.
func (u *updater) startPrevious(ctx context.Context, t *target) {
	if out, err := u.composeLogged(ctx, 5*time.Minute, t, nil, "up", "--detach", "--no-deps", t.Service); err != nil {
		u.logf("restarting the Controller: %v: %s", err, lastLines(out, 2))
	}
}

// backupInfo describes the backup: the state before the last update.
type backupInfo struct {
	JobID      string    `json:"job_id"`
	Version    string    `json:"version"`
	Image      string    `json:"image"`
	EnvExisted bool      `json:"env_existed"`
	At         time.Time `json:"at"`
}

// backup copies the Controller's data and .env into <state>/backup,
// replacing the previous backup only once the new one is complete.
func (u *updater) backup(job updaterapi.Job, image string, envData []byte, envExisted bool) error {
	dir := filepath.Join(u.cfg.StateDir, "backup")
	tmp := dir + ".new"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	if err := copyTree(u.cfg.DataDir, filepath.Join(tmp, "data")); err != nil {
		return err
	}
	if envExisted {
		if err := os.WriteFile(filepath.Join(tmp, "env"), envData, 0o600); err != nil {
			return err
		}
	}
	info, _ := json.MarshalIndent(backupInfo{JobID: job.ID, Version: job.FromVersion, Image: image, EnvExisted: envExisted, At: time.Now().UTC()}, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "info.json"), append(info, '\n'), 0o600); err != nil {
		return err
	}
	if err := syncDir(tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	return syncDir(u.cfg.StateDir)
}

// composeLogged runs compose with a timeout, its output into the job log.
func (u *updater) composeLogged(ctx context.Context, timeout time.Duration, t *target, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u.logf("docker compose %s", strings.Join(args, " "))
	out, err := u.p.compose(ctx, t, env, args...)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			u.logf("  %s", line)
		}
	}
	if err != nil {
		u.logf("  -> %v", err)
	}
	return out, err
}

func (u *updater) step(step, message string) {
	u.logf("%s", message)
	u.edit(func(j *updaterapi.Job) { j.Step, j.Message = step, message })
}

func (u *updater) finish(state, message string) {
	u.logf("%s: %s", state, message)
	u.edit(func(j *updaterapi.Job) {
		j.State, j.Message = state, message
		j.FinishedAt = time.Now().UTC()
	})
}

func (u *updater) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	log.Print(line)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.job == nil {
		return
	}
	u.job.Log = append(u.job.Log, time.Now().UTC().Format("15:04:05")+" "+line)
	if len(u.job.Log) > maxLogLines {
		u.job.Log = u.job.Log[len(u.job.Log)-maxLogLines:]
	}
}

func (u *updater) edit(f func(*updaterapi.Job)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f(u.job)
	u.save()
}

// copyJob returns a copy of the job; u.mu held.
func (u *updater) copyJob() *updaterapi.Job {
	if u.job == nil {
		return nil
	}
	j := *u.job
	j.Log = append([]string(nil), u.job.Log...)
	return &j
}

// --- state ---

func (u *updater) statePath() string { return filepath.Join(u.cfg.StateDir, "state.json") }

// save writes the job to the state volume; u.mu held.
func (u *updater) save() {
	data, err := json.MarshalIndent(struct {
		Job *updaterapi.Job `json:"job"`
	}{u.job}, "", "  ")
	if err == nil {
		err = writeFileAtomic(u.statePath(), append(data, '\n'), 0o600)
	}
	if err != nil {
		log.Printf("save state: %v", err)
	}
}

// load reads the last job back - one still "running" was cut short by
// the updater stopping.
func (u *updater) load() {
	data, exists, err := readOptional(u.statePath())
	if err != nil || !exists {
		if err != nil {
			log.Printf("read state: %v", err)
		}
		return
	}
	var st struct {
		Job *updaterapi.Job `json:"job"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("read state: %v", err)
		return
	}
	u.job = st.Job
	if u.job != nil && u.job.State == updaterapi.JobRunning {
		u.job.State = updaterapi.JobInterrupted
		u.job.Message = fmt.Sprintf("The updater stopped during this update (step %q). Check that the Controller runs; its data and .env from before the update are in the updater's state volume, under backup/.", u.job.Step)
		u.job.FinishedAt = time.Now().UTC()
		u.save()
	}
}

// --- check-ins ---

// checkins records each time a Controller says it has started.
type checkins struct {
	mu      sync.Mutex
	list    []checkin
	changed chan struct{}
}

type checkin struct {
	version string
	at      time.Time
}

func newCheckins() *checkins { return &checkins{changed: make(chan struct{})} }

func (c *checkins) add(version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, checkin{version, time.Now()})
	if len(c.list) > 50 {
		c.list = c.list[len(c.list)-50:]
	}
	close(c.changed)
	c.changed = make(chan struct{})
}

// wait returns when version has checked in since since (when, then).
func (c *checkins) wait(ctx context.Context, version string, since time.Time) (time.Time, error) {
	for {
		c.mu.Lock()
		for _, ci := range c.list {
			if ci.version == version && !ci.at.Before(since) {
				c.mu.Unlock()
				return ci.at, nil
			}
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-ch:
		}
	}
}
