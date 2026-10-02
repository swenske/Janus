// Package selfregister is a node's client-side half of the Point 2
// suite's self-registration design (see docs/architecture.md and
// internal/api/install.go's own doc comment on InstallRequest's
// controller_address/controller_ca_cert fields): a node provisioned
// with a Controller announces itself once, on the first boot that has
// both a Controller config and no prior successful announcement, by
// minting a fresh, purpose-specific service credential *locally* (this
// node's own CA is already in memory - internal/pki, no RPC needed) and
// POSTing it to the Controller's dedicated registration endpoint
// (dashboard/backend/register.go's startRegistrationListener). The
// node's root admin credential is never sent - only this freshly-minted,
// narrowly-scoped one, so compromising the Controller only ever exposes
// scoped, per-node credentials, never a fleet's root trust.
//
// The Controller's identity is verified against cfg.CACertPEM
// specifically - the CA it was given at provisioning time, not the
// system trust store and not trust-on-first-use - matching
// InstallRequest's own "the node must already know which CA to trust"
// rule.
//
// Deliberately separate from cmd/janusd itself (rather than inlined in
// main.go) so the registration logic - building the request, minting
// the credential, verifying the Controller's identity - has real unit
// tests (selfregister_test.go) against a plain httptest.Server, without
// needing a full janusd boot.
package selfregister

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/pki"
)

// Dir is where rootfs/init's mountState bind-mounts STATE's own
// controller/ subdirectory (see internal/api/install.go's
// writeControllerConfig, which is what wrote address/ca.crt there at
// provisioning time) - mirrors internal/bootcommit.Dir's own
// convention: a package-level var, not a const, so a test can point it
// at a temp directory instead. rootfs/init/main.go's mountState uses
// this same path as a literal string, not this import - consistent
// with how it already hardcodes "/etc/janus/pki"/"/etc/haproxy" rather
// than importing internal/pki for those, since rootfs/init otherwise
// has no reason to depend on this package at all (the actual read/
// register logic only ever runs from cmd/janusd).
var Dir = "/etc/janus/controller"

const (
	addressFile = "address"
	caFile      = "ca.crt"
	tokenFile   = "token"
	markerFile  = "registered"

	dialTimeout = 10 * time.Second
)

// Config is what a provisioned node knows about its Controller, read
// back from STATE at boot.
type Config struct {
	Address   string
	CACertPEM []byte
	// Token is the optional one-time registration token a Controller
	// that created this machine put in its NoCloud volume
	// (internal/nocloud) - presented so that Controller admits the node
	// directly instead of queueing it for manual approval.
	Token string
}

// Read loads Config from dir. A missing address file means no
// Controller was provisioned for this node at all - not an error, the
// overwhelming majority of boots (same convention as
// internal/bootcommit.Read's own treatment of a missing marker).
func Read(dir string) (*Config, error) {
	addrPath := filepath.Join(dir, addressFile)
	addr, err := os.ReadFile(addrPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", addrPath, err)
	}
	address := strings.TrimSpace(string(addr))
	if address == "" {
		return nil, fmt.Errorf("%s is empty", addrPath)
	}

	caPath := filepath.Join(dir, caFile)
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", caPath, err)
	}

	cfg := &Config{Address: address, CACertPEM: ca}
	tok, err := os.ReadFile(filepath.Join(dir, tokenFile))
	switch {
	case err == nil:
		cfg.Token = strings.TrimSpace(string(tok))
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, tokenFile), err)
	}
	return cfg, nil
}

// RemoveToken deletes the registration token once it has been used: the
// Controller only ever accepts it once, so there is nothing left to keep
// it for.
func RemoveToken(dir string) error {
	err := os.Remove(filepath.Join(dir, tokenFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// AlreadyRegistered reports whether this node has ever successfully
// announced itself to its Controller - checked before every attempt so
// a node announces exactly once, no matter how many times it reboots
// afterward.
func AlreadyRegistered(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, markerFile))
	return err == nil
}

// MarkRegistered persists that this node has successfully announced
// itself - written only after Register succeeds, so a failed attempt
// (Controller unreachable, say) is retried on the next boot rather than
// silently given up on forever.
//
// The marker is synced to disk before this returns: a node powered off
// within ext4's commit interval after announcing itself would otherwise
// announce itself again on its next boot (seen on the CI runner, where
// the self-register test stops the VM seconds after the announcement).
func MarkRegistered(dir string) error {
	tmp := filepath.Join(dir, "."+markerFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(time.Now().UTC().Format(time.RFC3339) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, markerFile)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// registerRequest mirrors dashboard/backend/register.go's own
// registerRequest field-for-field - this is the wire contract between
// the two. Kept as a plain literal struct here rather than a shared
// package: it's five fields, and pulling the dashboard's own module in
// as a dependency of the node's control-plane daemon isn't worth it for
// that.
type registerRequest struct {
	Name           string `json:"name"`
	Address        string `json:"address"`
	CACertPEM      string `json:"ca_cert_pem"`
	ServiceCertPEM string `json:"service_cert_pem"`
	ServiceKeyPEM  string `json:"service_key_pem"`
	// Omitted when the node has none - an older Controller never sees it.
	RegistrationToken string `json:"registration_token,omitempty"`
}

// registerResponse is the Controller's 201 body. Admitted is true when
// the registration token was accepted (the node is already approved); an
// older Controller, or one without a matching token, leaves it false.
type registerResponse struct {
	Admitted bool `json:"admitted"`
}

// Register mints a fresh admin-role service credential from ca (see the
// package doc comment for why it's this and never the node's own root
// admin credential) and POSTs a self-announcement to cfg.Address's
// /register endpoint, identifying itself as hostname and advertising
// advertiseAddr as the address a Controller should dial back to reach
// this node's own gRPC API. It reports whether the Controller admitted
// the node directly (cfg.Token accepted) rather than queueing it for
// approval.
func Register(cfg *Config, ca *pki.CA, hostname, advertiseAddr string) (admitted bool, err error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.CACertPEM) {
		return false, fmt.Errorf("controller CA certificate doesn't parse")
	}

	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{
		CommonName:  hostname,
		Roles:       []string{pki.RoleAdmin},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return false, fmt.Errorf("issue service credential: %w", err)
	}

	body, err := json.Marshal(registerRequest{
		Name:           hostname,
		Address:        advertiseAddr,
		CACertPEM:      string(ca.CertPEM),
		ServiceCertPEM: string(certPEM),
		ServiceKeyPEM:  string(keyPEM),

		RegistrationToken: cfg.Token,
	})
	if err != nil {
		return false, fmt.Errorf("encode registration request: %w", err)
	}

	client := &http.Client{
		Timeout: dialTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
	resp, err := client.Post("https://"+cfg.Address+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("POST https://%s/register: %w", cfg.Address, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusCreated {
		return false, fmt.Errorf("controller at %s refused registration (%s): %s", cfg.Address, resp.Status, strings.TrimSpace(string(respBody)))
	}
	var r registerResponse
	_ = json.Unmarshal(respBody, &r) // registered either way; only the admitted hint is lost
	return r.Admitted, nil
}

// DetectAdvertiseAddress picks the local IP this machine would use to
// reach controllerAddr (a connected UDP "dial" - a well-known Go idiom
// that consults the routing table without ever sending a packet, UDP
// being connectionless) and pairs it with grpcAddr's own port (e.g.
// janusd's -addr flag, ":9505") - the address a Controller should
// actually dial to reach this node's real gRPC API, which is never the
// same interface/path the registration POST itself goes out on in
// general (different port, and on a multi-homed node potentially a
// different local IP entirely for the two destinations).
func DetectAdvertiseAddress(controllerAddr, grpcAddr string) (string, error) {
	_, grpcPort, err := net.SplitHostPort(grpcAddr)
	if err != nil {
		return "", fmt.Errorf("parse gRPC listen address %q: %w", grpcAddr, err)
	}

	conn, err := net.Dial("udp", controllerAddr)
	if err != nil {
		return "", fmt.Errorf("determine local route to %s: %w", controllerAddr, err)
	}
	defer conn.Close()
	localIP, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("unexpected local address type %T dialing %s", conn.LocalAddr(), controllerAddr)
	}

	return net.JoinHostPort(localIP.IP.String(), grpcPort), nil
}
