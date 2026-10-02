// Package client calls the Janus Controller's API (docs/hypervisors.md):
// its hypervisors and the machines it creates on them. It authenticates
// with an API token and checks the Controller's certificate against the
// one given - the Controller's own, self-signed by default.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is one Controller.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// New returns a client for the Controller at endpoint ("https://host"),
// checking its certificate against caPEM (empty: the system's trust
// store), or not at all with insecure.
func New(endpoint, token, caPEM string, insecure bool) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("endpoint %q isn't an https:// URL", endpoint)
	}
	if token == "" {
		return nil, errors.New("an API token is required (the Controller's API tokens tab)")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec // only when asked for
	if caPEM != "" && !insecure {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, errors.New("ca_cert isn't a PEM certificate")
		}
		tlsConfig.RootCAs = pool
	}
	return &Client{
		endpoint: u.String(),
		token:    token,
		http:     &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{TLSClientConfig: tlsConfig}},
	}, nil
}

// Error is the Controller's answer to a refused request.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("Janus Controller: %s (HTTP %d)", e.Message, e.Status) }

// IsNotFound reports a 404: the resource is gone.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if msg == "" {
			msg = resp.Status
		}
		return &Error{Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// --- hypervisors ---

type LibvirtConfig struct {
	Host       string   `json:"host"`
	User       string   `json:"user"`
	Socket     string   `json:"socket,omitempty"`
	Pool       string   `json:"pool"`
	Networks   []string `json:"networks"`
	NamePrefix string   `json:"name_prefix,omitempty"`
}

type HypervisorRequest struct {
	Name              string         `json:"name"`
	Kind              string         `json:"kind"`
	ControllerAddress string         `json:"controller_address"`
	Libvirt           *LibvirtConfig `json:"libvirt"`
}

type Hypervisor struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Kind               string         `json:"kind"`
	ControllerAddress  string         `json:"controller_address"`
	Libvirt            *LibvirtConfig `json:"libvirt"`
	AuthorizedKey      string         `json:"authorized_key"`
	Trusted            bool           `json:"trusted"`
	HostKeyFingerprint string         `json:"host_key_fingerprint"`
	Machines           int            `json:"machines"`
}

func (c *Client) Hypervisors(ctx context.Context) ([]Hypervisor, error) {
	var out []Hypervisor
	return out, c.do(ctx, http.MethodGet, "/api/hypervisors", nil, &out)
}

func (c *Client) Hypervisor(ctx context.Context, id string) (*Hypervisor, error) {
	var out Hypervisor
	return &out, c.do(ctx, http.MethodGet, "/api/hypervisors/"+url.PathEscape(id), nil, &out)
}

func (c *Client) CreateHypervisor(ctx context.Context, req HypervisorRequest) (*Hypervisor, error) {
	var out Hypervisor
	return &out, c.do(ctx, http.MethodPost, "/api/hypervisors", req, &out)
}

func (c *Client) UpdateHypervisor(ctx context.Context, id string, req HypervisorRequest) (*Hypervisor, error) {
	var out Hypervisor
	return &out, c.do(ctx, http.MethodPatch, "/api/hypervisors/"+url.PathEscape(id), req, &out)
}

func (c *Client) DeleteHypervisor(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/hypervisors/"+url.PathEscape(id), nil, nil)
}

// TrustHypervisor pins the host key the hypervisor presents, if its
// fingerprint is the given one - the Controller checks.
func (c *Client) TrustHypervisor(ctx context.Context, id, fingerprint string) (*Hypervisor, error) {
	var out Hypervisor
	return &out, c.do(ctx, http.MethodPost, "/api/hypervisors/"+url.PathEscape(id)+"/trust", map[string]string{"fingerprint": fingerprint}, &out)
}

// --- machines ---

type ImageSource struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type NIC struct {
	Network   string   `json:"network"`
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Mode      string   `json:"mode"`
	Addresses []string `json:"addresses,omitempty"`
	Gateway   string   `json:"gateway,omitempty"`
}

type MachineSpec struct {
	Name         string       `json:"name"`
	HypervisorID string       `json:"hypervisor_id"`
	VCPUs        int          `json:"vcpus"`
	MemoryMiB    int          `json:"memory_mib"`
	Version      string       `json:"version,omitempty"`
	Extensions   []string     `json:"extensions,omitempty"`
	Image        *ImageSource `json:"image,omitempty"`
	NICs         []NIC        `json:"nics"`
	DNS          []string     `json:"dns,omitempty"`
	NTP          []string     `json:"ntp,omitempty"`
}

type Event struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

type Machine struct {
	ID             string      `json:"id"`
	Spec           MachineSpec `json:"spec"`
	HypervisorName string      `json:"hypervisor_name"`
	Phase          string      `json:"phase"`
	Error          string      `json:"error"`
	Version        string      `json:"version"`
	Schematic      string      `json:"schematic"`
	VMName         string      `json:"vm_name"`
	VMUUID         string      `json:"vm_uuid"`
	NodeID         string      `json:"node_id"`
	NodeAddress    string      `json:"node_address"`
	Events         []Event     `json:"events"`
}

// MachineUpdate is a PATCH: the fields given are the new values.
type MachineUpdate struct {
	VCPUs      *int      `json:"vcpus,omitempty"`
	MemoryMiB  *int      `json:"memory_mib,omitempty"`
	Version    *string   `json:"version,omitempty"`
	Extensions *[]string `json:"extensions,omitempty"`
	NICs       *[]NIC    `json:"nics,omitempty"`
	DNS        *[]string `json:"dns,omitempty"`
	NTP        *[]string `json:"ntp,omitempty"`
}

func (c *Client) Machine(ctx context.Context, id string) (*Machine, error) {
	var out Machine
	return &out, c.do(ctx, http.MethodGet, "/api/machines/"+url.PathEscape(id), nil, &out)
}

func (c *Client) CreateMachine(ctx context.Context, spec MachineSpec) (*Machine, error) {
	var out Machine
	return &out, c.do(ctx, http.MethodPost, "/api/machines", spec, &out)
}

func (c *Client) UpdateMachine(ctx context.Context, id string, u MachineUpdate) (*Machine, error) {
	var out Machine
	return &out, c.do(ctx, http.MethodPatch, "/api/machines/"+url.PathEscape(id), u, &out)
}

func (c *Client) DeleteMachine(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/machines/"+url.PathEscape(id), nil, nil)
}

// PollInterval is how often WaitMachine asks - a variable for tests.
var PollInterval = 3 * time.Second

// WaitMachine polls a machine until done says so, ctx ends, or it's gone
// (nil, nil when gone is what was waited for).
func (c *Client) WaitMachine(ctx context.Context, id string, done func(*Machine) bool) (*Machine, error) {
	for {
		m, err := c.Machine(ctx, id)
		if IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if done(m) {
			return m, nil
		}
		select {
		case <-ctx.Done():
			return m, fmt.Errorf("still %s: %w", m.Phase, ctx.Err())
		case <-time.After(PollInterval):
		}
	}
}

// LastEvent is the machine's newest event, for an error message.
func (m *Machine) LastEvent() string {
	if len(m.Events) == 0 {
		return ""
	}
	return m.Events[len(m.Events)-1].Message
}
