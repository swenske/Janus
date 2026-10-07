package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// docker is the few Docker Engine API calls the updater makes itself -
// reading containers, never changing them: every change goes through
// Docker Compose (compose.go), exactly what an operator would run.
type docker struct {
	hc *http.Client
}

func newDocker(socket string) *docker {
	return &docker{hc: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

type mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

type containerInfo struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		StartedAt  string `json:"StartedAt"`
	} `json:"State"`
	Mounts []mount `json:"Mounts"`
}

func (d *docker) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker %s: %s: %s", path, resp.Status, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func (d *docker) inspect(ctx context.Context, id string) (*containerInfo, error) {
	var c containerInfo
	if err := d.get(ctx, "/containers/"+url.PathEscape(id)+"/json", &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// imageUser is the user an image's processes run as - its USER
// instruction ("65532:65532"), empty when they run as root. The image
// was pulled already.
func (d *docker) imageUser(ctx context.Context, ref string) (string, error) {
	var img struct {
		Config struct {
			User string `json:"User"`
		} `json:"Config"`
	}
	if err := d.get(ctx, "/images/"+url.PathEscape(ref)+"/json", &img); err != nil {
		return "", err
	}
	return img.Config.User, nil
}

// byLabels lists the containers (running or not) carrying every label.
func (d *docker) byLabels(ctx context.Context, labels ...string) ([]string, error) {
	filters, err := json.Marshal(map[string][]string{"label": labels})
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID string `json:"Id"`
	}
	if err := d.get(ctx, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), &list); err != nil {
		return nil, err
	}
	ids := make([]string, len(list))
	for i, c := range list {
		ids[i] = c.ID
	}
	return ids, nil
}

var containerIDRE = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// ownContainerID finds the container this process runs in: Docker
// bind-mounts /etc/hostname, /etc/hosts and /etc/resolv.conf from
// /var/lib/docker/containers/<id>/, which shows in mountinfo. The
// hostname (a container ID prefix unless compose sets hostname:) is the
// fallback.
func ownContainerID() string {
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if m := containerIDRE.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	h, _ := os.Hostname()
	return h
}

// mountAt returns c's mount at destination, if any.
func (c *containerInfo) mountAt(destination string) (mount, bool) {
	for _, m := range c.Mounts {
		if m.Destination == destination {
			return m, true
		}
	}
	return mount{}, false
}

// same reports whether two mounts share their source: the same volume, or
// the same host path.
func (m mount) same(o mount) bool {
	if m.Type != o.Type {
		return false
	}
	if m.Type == "volume" {
		return m.Name != "" && m.Name == o.Name
	}
	return m.Source != "" && m.Source == o.Source
}

func (m mount) String() string {
	if m.Type == "volume" {
		return "volume " + m.Name
	}
	return m.Source
}
