// Package proxmox is the Controller's hypervisor.Driver for Proxmox VE:
// its REST API, with an API token whose rights cover one resource pool,
// the storages and networks the Controller may use, and reading the node
// (docs/hypervisors.md: preparing a Proxmox host). The token sees no
// other virtual machine at all; on top of that, every machine carries
// this Controller's ownership tag in its description, checked before any
// operation on it - as with libvirt.
package proxmox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// client is the API, authenticated with the token, over TLS checked
// against the pinned certificate or the given CA.
type client struct {
	base string // https://host:8006/api2/json
	auth string // PVEAPIToken=user@realm!name=secret
	http *http.Client
	tls  *tls.Config
}

// apiError is an error the API answered with.
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func newClient(c *hypervisor.ProxmoxConfig, secret string) (*client, error) {
	tc, err := tlsConfig(c)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		TLSClientConfig:     tc,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		// Never through a proxy: the API's certificate is checked
		// end to end.
		Proxy: nil,
	}
	return &client{
		base: c.APIBase(),
		auth: "PVEAPIToken=" + c.TokenID + "=" + secret,
		http: &http.Client{Transport: tr},
		tls:  tc,
	}, nil
}

// tlsConfig trusts the API's certificate by its pinned fingerprint, or
// by the CA that signs it; neither: nothing.
func tlsConfig(c *hypervisor.ProxmoxConfig) (*tls.Config, error) {
	host := ""
	if u, err := url.Parse(c.URL); err == nil {
		host = u.Hostname()
	}
	switch {
	case c.CACert != "":
		ca, err := hypervisor.ParseCACert(c.CACert)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca)
		return &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}, nil
	case c.Fingerprint != "":
		want := hypervisor.NormalizeFingerprint(c.Fingerprint)
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The pinned certificate is the trust: no CA, no name.
			InsecureSkipVerify: true, //nolint:gosec // checked below
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error { //nolint:gosec // G123: no ClientSessionCache, so no resumption - every connection presents its chain
				if len(raw) == 0 {
					return errors.New("the API presented no certificate")
				}
				if got := Fingerprint(raw[0]); got != want {
					return fmt.Errorf("the API presents the certificate %s, not the trusted %s", got, want)
				}
				return nil
			},
		}, nil
	}
	return nil, errors.New("the API's certificate isn't trusted yet")
}

// Fingerprint is a DER certificate's SHA-256, as Proxmox shows it.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// ProbeCertificate reads the certificate the API at c presents -
// trusting nothing yet: what the operator compares with the node's own.
func ProbeCertificate(ctx context.Context, c *hypervisor.ProxmoxConfig) (*x509.Certificate, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // only read, never trusted
	conn, err := d.DialContext(ctx, "tcp", c.Address())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("the API presented no certificate")
	}
	return certs[0], nil
}

// do calls the API: params form-encoded, the "data" of the answer into
// out (when not nil).
func (c *client) do(ctx context.Context, method, path string, params url.Values, out any) error {
	var body io.Reader
	u := c.base + path
	if params != nil {
		if method == http.MethodGet || method == http.MethodDelete {
			u += "?" + params.Encode()
		} else {
			body = strings.NewReader(params.Encode())
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.send(req, out)
}

func (c *client) send(req *http.Request, out any) error {
	req.Header.Set("Authorization", c.auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// Proxmox says why in the status line, sometimes in the body.
		msg := strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprintf("%d ", resp.StatusCode)))
		var e struct {
			Message string            `json:"message"`
			Errors  map[string]string `json:"errors"`
		}
		if json.Unmarshal(raw, &e) == nil {
			if m := strings.TrimSpace(e.Message); m != "" {
				msg = m
			}
			for k, v := range e.Errors {
				msg += fmt.Sprintf("; %s: %s", k, strings.TrimSpace(v))
			}
		}
		return &apiError{Status: resp.StatusCode, Msg: fmt.Sprintf("Proxmox: %s", msg)}
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("read Proxmox's answer: %w", err)
	}
	if len(env.Data) == 0 || bytes.Equal(env.Data, []byte("null")) {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// wait waits for a task (its UPID) to end, and reports how it ended.
func (c *client) wait(ctx context.Context, upid string) error {
	node := upidNode(upid)
	path := "/nodes/" + url.PathEscape(node) + "/tasks/" + url.PathEscape(upid) + "/status"
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("the Proxmox task failed: %s", strings.TrimSpace(st.ExitStatus))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// task calls the API for a task (an UPID) and waits for it.
func (c *client) task(ctx context.Context, method, path string, params url.Values) error {
	var upid string
	if err := c.do(ctx, method, path, params, &upid); err != nil {
		return err
	}
	if upid == "" {
		return nil // done already
	}
	return c.wait(ctx, upid)
}

// upidNode is the node a task runs on: "UPID:node:...".
func upidNode(upid string) string {
	parts := strings.Split(upid, ":")
	if len(parts) > 2 {
		return parts[1]
	}
	return ""
}

// isPermission reports whether err is the API refusing: for a virtual
// machine, one that's gone or outside the pool the token may see.
func isPermission(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == http.StatusForbidden
}

// isMissing reports whether err is a virtual machine that doesn't exist.
func isMissing(err error) bool {
	var e *apiError
	return errors.As(err, &e) && (strings.Contains(e.Msg, "does not exist") || e.Status == http.StatusNotFound)
}
