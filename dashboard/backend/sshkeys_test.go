package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/internal/sshsig"
)

// mfaRoot signs root in on a, with an authenticator app set up - the
// second factor adding an SSH key needs.
func mfaRoot(t *testing.T, a *authApp) string {
	t.Helper()
	root := a.login(t, "root", "root-password")
	_, body := a.req(t, "POST", "/api/auth/mfa/totp/setup", root, nil)
	var setup struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal([]byte(body), &setup)
	code, _ := auth.TOTPCode(setup.Secret, time.Now())
	if c, out := a.req(t, "POST", "/api/auth/mfa/totp/enable", root, map[string]string{"code": code}); c != http.StatusOK {
		t.Fatalf("enable TOTP: %d %s", c, out)
	}
	return root
}

// sshLogin signs in with signer the way janusctl does, answering the
// status and the body.
func sshLogin(t *testing.T, a *authApp, user string, signer ssh.Signer, namespace, serverFP string) (int, string) {
	t.Helper()
	fp := ssh.FingerprintSHA256(signer.PublicKey())
	_, body := a.req(t, "POST", "/api/cli/challenge", "", map[string]string{"user": user, "fingerprint": fp})
	var ch struct {
		Challenge string `json:"challenge"`
		Server    string `json:"server_fingerprint"`
	}
	_ = json.Unmarshal([]byte(body), &ch)
	if serverFP == "" {
		serverFP = ch.Server
	}
	sig, err := sshsig.Sign(rand.Reader, signer, namespace, sshLoginMessage(serverFP, ch.Challenge))
	if err != nil {
		t.Fatal(err)
	}
	return a.req(t, "POST", "/api/cli/ssh-login", "", map[string]string{"user": user, "fingerprint": fp, "challenge": ch.Challenge, "signature": string(sig)})
}

// TestSSHLogin: an account's SSH key - added from a sign-in with a second
// factor - signs janusctl in alone, for a certificate of that key with
// its role; a signature for another server or purpose, an old challenge,
// another account's key don't.
func TestSSHLogin(t *testing.T) {
	a := newMFAApp(t)
	withReadyFleet(t, a.app)
	root := mfaRoot(t, a)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	line := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	olga := a.login(t, "olga", "olga-password")
	if code, body := a.req(t, "POST", "/api/auth/ssh-keys", olga, map[string]any{"name": "laptop", "public_key": line}); code != http.StatusForbidden || !strings.Contains(body, "second factor") {
		t.Errorf("a key from a sign-in without a second factor: %d %s", code, body)
	}
	if code, body := a.req(t, "POST", "/api/auth/ssh-keys", root, map[string]any{"name": "laptop", "public_key": line, "role": "operator", "expires_in_days": 30}); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}
	if _, body := a.req(t, "GET", "/api/auth/ssh-keys", root, nil); !strings.Contains(body, `"name":"laptop"`) || !strings.Contains(body, `"role":"operator"`) {
		t.Errorf("list: %s", body)
	}

	code, body := sshLogin(t, a, "root", signer, SSHLoginNamespace, "")
	var out struct {
		CertificatePEM string    `json:"certificate_pem"`
		Role           string    `json:"role"`
		User           string    `json:"user"`
		ExpiresAt      time.Time `json:"expires_at"`
		Nodes          []cliNode `json:"nodes"`
	}
	if _ = json.Unmarshal([]byte(body), &out); code != http.StatusOK || out.User != "root" || out.Role != "os:operator" || out.Nodes == nil {
		t.Fatalf("SSH login: %d %s", code, body)
	}
	block, _ := pem.Decode([]byte(out.CertificatePEM))
	leaf, _ := x509.ParseCertificate(block.Bytes)
	if !leaf.PublicKey.(ed25519.PublicKey).Equal(priv.Public()) {
		t.Error("the certificate isn't for the SSH key itself")
	}
	if d := time.Until(out.ExpiresAt); d < 11*time.Hour || d > 12*time.Hour {
		t.Errorf("valid %v", d)
	}

	if code, _ := sshLogin(t, a, "root", signer, "git", ""); code != http.StatusUnauthorized {
		t.Errorf("a signature for git: %d", code)
	}
	if code, _ := sshLogin(t, a, "root", signer, SSHLoginNamespace, strings.Repeat("ab", 32)); code != http.StatusUnauthorized {
		t.Errorf("a signature for another server: %d", code)
	}
	if code, _ := sshLogin(t, a, "olga", signer, SSHLoginNamespace, ""); code != http.StatusUnauthorized {
		t.Errorf("root's key for olga: %d", code)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)
	if code, _ := sshLogin(t, a, "root", otherSigner, SSHLoginNamespace, ""); code != http.StatusUnauthorized {
		t.Errorf("a key root doesn't have: %d", code)
	}
	// A challenge is good once.
	fp := ssh.FingerprintSHA256(signer.PublicKey())
	_, chBody := a.req(t, "POST", "/api/cli/challenge", "", map[string]string{"user": "root", "fingerprint": fp})
	var ch struct {
		Challenge string `json:"challenge"`
		Server    string `json:"server_fingerprint"`
	}
	_ = json.Unmarshal([]byte(chBody), &ch)
	sig, _ := sshsig.Sign(rand.Reader, signer, SSHLoginNamespace, sshLoginMessage(ch.Server, ch.Challenge))
	req := map[string]string{"user": "root", "fingerprint": fp, "challenge": ch.Challenge, "signature": string(sig)}
	if code, _ := a.req(t, "POST", "/api/cli/ssh-login", "", req); code != http.StatusOK {
		t.Fatalf("first use: %d", code)
	}
	if code, _ := a.req(t, "POST", "/api/cli/ssh-login", "", req); code != http.StatusUnauthorized {
		t.Errorf("the same challenge again: %d", code)
	}

	// An admin sees the account's keys, and can take them all away.
	if code, body := a.req(t, "GET", "/api/users", root, nil); code != http.StatusOK || !strings.Contains(body, `"name":"laptop"`) {
		t.Errorf("the accounts' keys: %d %s", code, body)
	}

	// Removed, it signs in no more. The fingerprint goes in the path
	// escaped, as the page sends it: a raw "//" in it (one base64
	// fingerprint in a hundred has one) is cleaned by the mux into a
	// redirect instead.
	if code, _ := a.req(t, "DELETE", "/api/auth/ssh-keys/"+url.PathEscape("SHA256:ab//cd/"), root, nil); code != http.StatusNotFound {
		t.Errorf("removing an unknown key with slashes in its fingerprint: %d, want 404", code)
	}
	if code, _ := a.req(t, "DELETE", "/api/auth/ssh-keys/"+url.PathEscape(fp), root, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	a.loginLimiter = auth.NewLoginLimiter()
	if code, _ := sshLogin(t, a, "root", signer, SSHLoginNamespace, ""); code != http.StatusUnauthorized {
		t.Errorf("a removed key: %d", code)
	}
	if code, _ := a.req(t, "POST", "/api/auth/ssh-keys", root, map[string]any{"name": "again", "public_key": line}); code != http.StatusCreated {
		t.Fatal(code)
	}
	if code, body := a.req(t, "DELETE", "/api/users/root/ssh-keys", root, nil); code != http.StatusOK || !strings.Contains(body, `"removed":1`) {
		t.Errorf("revoking root's keys: %d %s", code, body)
	}
	a.loginLimiter = auth.NewLoginLimiter()
	if code, _ := sshLogin(t, a, "root", signer, SSHLoginNamespace, ""); code != http.StatusUnauthorized {
		t.Errorf("a revoked key: %d", code)
	}
	entries, _ := a.audit.Recent(50, func(e audit.Entry) bool { return e.Path == "/api/cli/ssh-login" })
	if len(entries) == 0 || entries[len(entries)-1].User != "root" || !strings.HasPrefix(entries[len(entries)-1].Via, "ssh key SHA256:") {
		t.Errorf("the audit of SSH sign-ins: %+v", entries)
	}
}
