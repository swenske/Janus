package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// target is the Controller's Compose service, as found on the host.
type target struct {
	Project     string
	Service     string
	WorkingDir  string
	ConfigFiles []string
	ContainerID string
	// Image is what the Controller's container runs (its compose image:
	// field, after interpolation).
	Image string
}

// platform is everything the update job does to the outside world - the
// real one is Docker + Docker Compose, the tests' a fake.
type platform interface {
	// discover finds the Controller's service and lists what keeps the
	// updater from updating it.
	discover(ctx context.Context) (*target, []string)
	// checkCompose makes sure the service's image comes from the variable
	// the updater sets in .env (slower: it runs docker compose config).
	checkCompose(ctx context.Context, t *target) error
	// compose runs docker compose on t's project, with extra environment
	// variables (which win over .env).
	compose(ctx context.Context, t *target, env []string, args ...string) (string, error)
	// containerState is the state of t's service's container now (the
	// container is replaced by an update, its ID changes).
	containerState(ctx context.Context, t *target) (running bool, startedAt time.Time, err error)
	// imageUser is the user image's processes run as (its USER
	// instruction, "65532:65532"); empty for root.
	imageUser(ctx context.Context, image string) (string, error)
}

const (
	labelProject     = "com.docker.compose.project"
	labelService     = "com.docker.compose.service"
	labelOneoff      = "com.docker.compose.oneoff"
	labelWorkingDir  = "com.docker.compose.project.working_dir"
	labelConfigFiles = "com.docker.compose.project.config_files"
)

type realPlatform struct {
	cfg    config
	docker *docker
}

func (p *realPlatform) discover(ctx context.Context) (*target, []string) {
	cfg := p.cfg
	self, err := p.docker.inspect(ctx, ownContainerID())
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return nil, []string{fmt.Sprintf("can't reach Docker at %s - mount it: %s:%s", cfg.DockerSocket, cfg.DockerSocket, cfg.DockerSocket)}
		}
		return nil, []string{fmt.Sprintf("can't find the updater's own container: %v", err)}
	}
	project := cfg.Project
	if project == "" {
		project = self.Config.Labels[labelProject]
	}
	if project == "" {
		return nil, []string{"the updater isn't running under Docker Compose - start it from the Controller's compose.yaml"}
	}
	ids, err := p.docker.byLabels(ctx, labelProject+"="+project, labelService+"="+cfg.Service, labelOneoff+"=False")
	if err != nil {
		return nil, []string{fmt.Sprintf("list the containers of project %s: %v", project, err)}
	}
	switch len(ids) {
	case 0:
		return nil, []string{fmt.Sprintf("no container for service %q in Compose project %q - start the Controller with docker compose up -d, or set JANUS_UPDATER_SERVICE to its service name", cfg.Service, project)}
	case 1:
	default:
		return nil, []string{fmt.Sprintf("service %q has %d containers - the Controller runs one", cfg.Service, len(ids))}
	}
	ctr, err := p.docker.inspect(ctx, ids[0])
	if err != nil {
		return nil, []string{fmt.Sprintf("inspect the Controller's container: %v", err)}
	}
	t := &target{
		Project:     project,
		Service:     cfg.Service,
		WorkingDir:  ctr.Config.Labels[labelWorkingDir],
		ContainerID: ctr.ID,
		Image:       ctr.Config.Image,
	}
	for _, f := range strings.Split(ctr.Config.Labels[labelConfigFiles], ",") {
		if f != "" {
			t.ConfigFiles = append(t.ConfigFiles, f)
		}
	}

	var problems []string
	if t.WorkingDir == "" || len(t.ConfigFiles) == 0 {
		problems = append(problems, "the Controller's container has no Compose project directory - was it started with docker compose?")
	} else {
		for _, f := range t.ConfigFiles {
			if _, err := os.Stat(f); err != nil {
				problems = append(problems, fmt.Sprintf("the updater can't see %s - mount the Compose directory at the same path: %s:%s", f, t.WorkingDir, t.WorkingDir))
				break
			}
		}
		if len(problems) == 0 {
			if f, err := os.CreateTemp(t.WorkingDir, ".janus-updater-*"); err != nil {
				problems = append(problems, fmt.Sprintf("the updater can't write to %s, where .env lives - mount it read-write", t.WorkingDir))
			} else {
				f.Close()
				os.Remove(f.Name())
			}
		}
	}

	if mine, ok := self.mountAt(cfg.DataDir); !ok {
		problems = append(problems, fmt.Sprintf("the updater needs the Controller's data volume at %s, to back it up before an update", cfg.DataDir))
	} else if theirs, ok := ctr.mountAt("/data"); !ok || !mine.same(theirs) {
		problems = append(problems, fmt.Sprintf("the updater's %s (%s) isn't the Controller's /data - mount the same volume", cfg.DataDir, mine))
	}
	sockDir := filepath.Dir(cfg.Socket)
	if mine, ok := self.mountAt(sockDir); !ok {
		problems = append(problems, fmt.Sprintf("the updater's socket directory %s must be a volume the Controller mounts too", sockDir))
	} else if theirs, ok := ctr.mountAt(sockDir); !ok || !mine.same(theirs) {
		problems = append(problems, fmt.Sprintf("the Controller must mount the updater's socket volume (%s) at %s - that's how it asks for an update", mine, sockDir))
	}
	return t, problems
}

// checkCompose: with a probe value for the image variable, compose must
// resolve the service's image to exactly that.
func (p *realPlatform) checkCompose(ctx context.Context, t *target) error {
	const probe = "janus-updater.invalid/probe:check"
	out, err := p.compose(ctx, t, []string{p.cfg.Variable + "=" + probe}, "config", "--format", "json")
	if err != nil {
		return fmt.Errorf("docker compose config failed: %v: %s", err, lastLines(out, 3))
	}
	var cfg struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return fmt.Errorf("read docker compose config: %v", err)
	}
	svc, ok := cfg.Services[t.Service]
	if !ok {
		return fmt.Errorf("the compose files have no service %q", t.Service)
	}
	if svc.Image != probe {
		return fmt.Errorf("service %s's image doesn't come from %s - use image: ${%s:-%s:latest}", t.Service, p.cfg.Variable, p.cfg.Variable, p.cfg.Repository)
	}
	return nil
}

func (p *realPlatform) compose(ctx context.Context, t *target, env []string, args ...string) (string, error) {
	full := []string{"--ansi", "never", "--progress", "plain", "--project-name", t.Project, "--project-directory", t.WorkingDir}
	for _, f := range t.ConfigFiles {
		full = append(full, "--file", f)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, p.cfg.ComposeBin, full...)
	cmd.Dir = t.WorkingDir
	// Only what compose needs - nothing of the updater's own environment,
	// so the image variable comes from .env unless env sets it.
	cmd.Env = append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + filepath.Join(p.cfg.StateDir, "home"),
		"DOCKER_CONFIG=" + filepath.Join(p.cfg.StateDir, "docker"),
		"TMPDIR=" + filepath.Join(p.cfg.StateDir, "tmp"),
		"DOCKER_HOST=unix://" + p.cfg.DockerSocket,
		"PWD=" + t.WorkingDir,
	}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *realPlatform) containerState(ctx context.Context, t *target) (bool, time.Time, error) {
	ids, err := p.docker.byLabels(ctx, labelProject+"="+t.Project, labelService+"="+t.Service, labelOneoff+"=False")
	if err != nil {
		return false, time.Time{}, err
	}
	if len(ids) != 1 {
		return false, time.Time{}, fmt.Errorf("service %s has %d containers", t.Service, len(ids))
	}
	c, err := p.docker.inspect(ctx, ids[0])
	if err != nil {
		return false, time.Time{}, err
	}
	started, _ := time.Parse(time.RFC3339Nano, c.State.StartedAt)
	return c.State.Running && !c.State.Restarting, started, nil
}

// lastLines is the end of a command's output, for an error message.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

func (p *realPlatform) imageUser(ctx context.Context, image string) (string, error) {
	return p.docker.imageUser(ctx, image)
}
