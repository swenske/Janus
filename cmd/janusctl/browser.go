package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Signing in through the browser: the account signs in on the
// Controller's page - its second factor and all - and approves a
// certificate for the key janusctl made here, by its fingerprint.
// -browser: the page hands the approval to janusctl's listener on
// 127.0.0.1; -device: janusctl shows a code to enter on the page, from
// any machine, and waits. The certificate lasts 12 hours; then janusctl
// login again.

// browserWait is how long janusctl waits for the page.
const browserWait = 5 * time.Minute

type issued struct {
	CertificatePEM string    `json:"certificate_pem"`
	ExpiresAt      time.Time `json:"expires_at"`
	User           string    `json:"user"`
	Role           string    `json:"role"`
	Nodes          []ctxNode `json:"nodes"`
}

// newCLIKey is a key for a certificate from the page, its CSR, and its
// fingerprint (SHA-256 of its SubjectPublicKeyInfo, hex) - what the page
// shows to approve.
func newCLIKey() (*ecdsa.PrivateKey, string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", "", err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, "", "", err
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, "", "", err
	}
	sum := sha256.Sum256(spki)
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})), hex.EncodeToString(sum[:]), nil
}

// keep writes the key and its certificate, and the context's state.
func (c *cliContext) keep(name string, key *ecdsa.PrivateKey, out issued) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	dir := contextDir(name)
	if err := writeFileAtomic(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "cert.pem"), []byte(out.CertificatePEM), 0o600); err != nil {
		return err
	}
	c.User, c.Role, c.Expires, c.Nodes, c.SSHKey = out.User, out.Role, out.ExpiresAt, out.Nodes, ""
	return nil
}

// browserSignIn opens the Controller's page for the account to approve
// janusctl's key, and takes the approval on a listener of 127.0.0.1.
func (c *cliContext) browserSignIn(name string, open bool) error {
	key, csr, fp, err := newCLIKey()
	if err != nil {
		return err
	}
	stateRaw := make([]byte, 16)
	if _, err := rand.Read(stateRaw); err != nil {
		return err
	}
	state := base64.RawURLEncoding.EncodeToString(stateRaw)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	page := "https://" + c.Controller + "/#/cli-login?" + url.Values{"port": {fmt.Sprint(port)}, "state": {state}, "key": {fp}}.Encode()

	type answer struct{ code, err string }
	got := make(chan answer, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/callback" || q.Get("state") != state {
			http.NotFound(w, r)
			return
		}
		msg := "janusctl is signed in: you can close this tab."
		a := answer{code: q.Get("code")}
		if e := q.Get("error"); e != "" || a.code == "" {
			a = answer{err: "the sign-in was denied on the Controller's page"}
			msg = "Denied: janusctl isn't signed in."
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>janusctl</title><body style=\"font-family:sans-serif;margin:3rem\"><p>%s</p></body>", html.EscapeString(msg))
		select {
		case got <- a:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background()) //nolint:errcheck // done with it

	fmt.Fprintf(os.Stderr, "Sign janusctl in on the Controller's page:\n  %s\nThe page shows the key to approve: %s\n", page, colonHex(fp)[:47]+"…")
	if open {
		openBrowser(page)
	}
	var a answer
	select {
	case a = <-got:
	case <-time.After(browserWait):
		return errors.New("no approval from the page within 5 minutes")
	}
	if a.err != "" {
		return errors.New(a.err)
	}
	var out issued
	if err := c.call("POST", "/api/cli/exchange", "", map[string]string{"code": a.code, "csr_pem": csr}, &out); err != nil {
		return err
	}
	return c.keep(name, key, out)
}

// deviceSignIn shows a code to enter on the Controller's page, from any
// machine, and waits for its approval.
func (c *cliContext) deviceSignIn(name string) error {
	key, csr, fp, err := newCLIKey()
	if err != nil {
		return err
	}
	var d struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
	if err := c.call("POST", "/api/cli/device", "", map[string]string{"csr_pem": csr}, &d); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "On any machine, open https://%s/#/cli-device and enter\n\n    %s\n\nThe page shows the key to approve: %s\nWaiting…\n", c.Controller, d.UserCode, colonHex(fp)[:47]+"…")
	interval := time.Duration(max(d.Interval, 2)) * time.Second
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		var out issued
		err := c.call("POST", "/api/cli/device/token", "", map[string]string{"device_code": d.DeviceCode}, &out)
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusPreconditionRequired {
			continue
		}
		if err != nil {
			return err
		}
		return c.keep(name, key, out)
	}
	return errors.New("the code ended before it was approved: janusctl login -device again")
}

// openBrowser opens url in the desktop's browser, if there's one.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}
