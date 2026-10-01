// Package updaterapi is the contract between the Janus Controller
// (dashboardd) and its updater (janus-controller-updater, see
// dashboard/updater): JSON over a Unix socket in a Docker volume only the
// two containers mount. The Controller never touches Docker itself - the
// updater, the one container given the Docker socket, does the update
// when the Controller asks for it, and the Controller tells it each time
// it has started (which version), so the updater knows whether the new
// version came up or has to be rolled back.
package updaterapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultSocket is where the updater listens and the Controller looks for
// it - a directory both mount from the same volume (dashboard/README.md).
const DefaultSocket = "/run/janus-updater/updater.sock"

// Job states.
const (
	JobRunning = "running"
	JobDone    = "done"
	// JobFailed: refused or failed before anything changed (the image
	// couldn't be pulled, the setup is incomplete...).
	JobFailed = "failed"
	// JobRolledBack: the new version didn't come up; the previous image,
	// configuration and data are back.
	JobRolledBack = "rolled-back"
	// JobRollbackFailed: the previous version didn't come back up either -
	// needs a human (dashboard/README.md, "When an update fails").
	JobRollbackFailed = "rollback-failed"
	// JobInterrupted: the updater itself stopped during the update.
	JobInterrupted = "interrupted"
)

// Status is the updater's state and whether it can update.
type Status struct {
	// Version is the updater's own (it's the Controller's image, so the
	// version it was started from).
	Version string `json:"version"`
	// Ready: the setup is complete; Problems says what's missing otherwise.
	Ready    bool     `json:"ready"`
	Problems []string `json:"problems,omitempty"`

	Project    string `json:"project,omitempty"`
	Service    string `json:"service,omitempty"`
	WorkingDir string `json:"working_dir,omitempty"`
	// Variable is the .env variable the Controller's image comes from.
	Variable   string `json:"variable,omitempty"`
	Repository string `json:"repository,omitempty"`
	// CurrentImage is the image the Controller's container runs.
	CurrentImage string `json:"current_image,omitempty"`

	// Job is the current or last update.
	Job *Job `json:"job,omitempty"`
}

// Job is one update.
type Job struct {
	ID          string    `json:"id"`
	FromVersion string    `json:"from_version"`
	FromImage   string    `json:"from_image,omitempty"`
	ToVersion   string    `json:"to_version"`
	ToImage     string    `json:"to_image"`
	State       string    `json:"state"`
	Step        string    `json:"step,omitempty"`
	Message     string    `json:"message,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	Log         []string  `json:"log,omitempty"`
}

// UpdateRequest asks for the Controller to be moved to Version, running
// Image (its repository and tag must match - an image name is never
// free-form). An empty Image means "<repository>:<version>".
type UpdateRequest struct {
	Version     string `json:"version"`
	Image       string `json:"image,omitempty"`
	FromVersion string `json:"from_version"`
}

// StartedRequest: the Controller has started and is listening.
type StartedRequest struct {
	Version string `json:"version"`
}

// --- versions ---

// Releases are CalVer: vYYYY.MM.DD, or vYYYY.MM.DD-N for the Nth release
// of a day. A build between releases is git describe's
// vYYYY.MM.DD[-N]-K-g<sha>[-dirty]: K commits after that release.
var (
	releaseRE  = regexp.MustCompile(`^v(\d{4})\.(\d{2})\.(\d{2})(?:-(\d+))?$`)
	describeRE = regexp.MustCompile(`^v(\d{4})\.(\d{2})\.(\d{2})(?:-(\d+))?(?:-\d+-g[0-9a-f]+)?(?:-dirty)?$`)
)

// IsRelease reports whether v is a release version.
func IsRelease(v string) bool { return releaseRE.MatchString(v) }

// Newer reports whether release is newer than running. known is false
// when running isn't a release or a build after one ("dev", a bare SHA):
// nothing to compare against.
func Newer(release, running string) (newer, known bool) {
	r, ok := parse(releaseRE, release)
	if !ok {
		return false, false
	}
	c, ok := parse(describeRE, running)
	if !ok {
		return false, false
	}
	for i := range r {
		if r[i] != c[i] {
			return r[i] > c[i], true
		}
	}
	return false, true
}

func parse(re *regexp.Regexp, v string) ([4]int, bool) {
	m := re.FindStringSubmatch(v)
	if m == nil {
		return [4]int{}, false
	}
	var out [4]int
	for i := range 4 {
		if m[i+1] != "" {
			out[i], _ = strconv.Atoi(m[i+1])
		}
	}
	return out, true
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// CheckImage returns the image to run for version from repository: image
// itself if it's that repository's tag for that version, optionally
// pinned to a digest ("repo:vX@sha256:..."), or "repo:vX" when image is
// empty.
func CheckImage(image, repository, version string) (string, error) {
	if !IsRelease(version) {
		return "", fmt.Errorf("%q isn't a release version", version)
	}
	want := repository + ":" + version
	if image == "" {
		return want, nil
	}
	ref, digest, pinned := strings.Cut(image, "@")
	if ref != want {
		return "", fmt.Errorf("image %q isn't %s", image, want)
	}
	if pinned && !digestRE.MatchString(digest) {
		return "", fmt.Errorf("image %q: %q isn't a sha256 digest", image, digest)
	}
	return image, nil
}

// --- client ---

// Client talks to the updater over its Unix socket.
type Client struct {
	Socket string
	hc     *http.Client
}

// NewClient returns a client for the updater listening on socket.
func NewClient(socket string) *Client {
	return &Client{Socket: socket, hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

// Status returns the updater's status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var st Status
	return &st, c.do(ctx, http.MethodGet, "/v1/status", nil, &st)
}

// Update starts an update; the job is followed through Status.
func (c *Client) Update(ctx context.Context, req UpdateRequest) (*Job, error) {
	var job Job
	return &job, c.do(ctx, http.MethodPost, "/v1/update", req, &job)
}

// Started tells the updater this Controller has started.
func (c *Client) Started(ctx context.Context, version string) error {
	return c.do(ctx, http.MethodPost, "/v1/started", StartedRequest{Version: version}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://updater"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &Error{Code: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Error is the updater refusing a request.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }
