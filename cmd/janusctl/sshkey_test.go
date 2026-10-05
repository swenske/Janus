package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/swenske/Janus/internal/pki"
)

// serveAgent runs an in-memory ssh-agent holding keys on a socket, as
// SSH_AUTH_SOCK.
func serveAgent(t *testing.T, keys ...any) {
	t.Helper()
	ring := agent.NewKeyring()
	for _, k := range keys {
		if err := ring.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = agent.ServeAgent(ring, c)
				c.Close()
			}()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
}

// handshake connects with key's certificate from ca to a server like
// janusd (client certificates from ca required, TLS 1.3), answering the
// name the server saw.
func handshake(t *testing.T, ca *pki.CA, key *sshKey) (string, error) {
	t.Helper()
	der, err := ca.IssueFor(key.tls.Public(), pki.IssueOptions{CommonName: "sam", Roles: []string{pki.RoleAdmin}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(der)
	serverPEM, serverKey, _ := ca.Issue(pki.IssueOptions{CommonName: "node", DNSNames: []string{"node"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	serverCert, _ := tls.X509KeyPair(serverPEM, serverKey)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca.CertPool(), MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	seen := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			seen <- ""
			return
		}
		defer c.Close()
		tc := c.(*tls.Conn)
		if tc.Handshake() != nil || len(tc.ConnectionState().PeerCertificates) == 0 {
			seen <- ""
			return
		}
		seen <- tc.ConnectionState().PeerCertificates[0].Subject.CommonName
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{block.Bytes}, PrivateKey: key.tls}},
		RootCAs:      ca.CertPool(),
		ServerName:   "node",
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.Handshake(); err != nil {
		return "", err
	}
	if name := <-seen; name != "" {
		return name, nil
	}
	return "", errors.New("the server refused the client certificate")
}

// TestAgentKeyTLS: an Ed25519 key in ssh-agent signs a TLS 1.3 handshake
// with a client certificate - what janusctl does with the nodes; an
// ECDSA key in the agent is refused, from its file it works.
func TestAgentKeyTLS(t *testing.T) {
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serveAgent(t, ec, ed)
	ca, _ := pki.NewCA("node CA")

	key, err := openSSHKey("")
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	edSSH, _ := ssh.NewPublicKey(ed.Public())
	if key.fingerprint() != ssh.FingerprintSHA256(edSSH) || key.spec != "agent:"+ssh.FingerprintSHA256(edSSH) {
		t.Fatalf("the agent's Ed25519 key: %s", key.spec)
	}
	if name, err := handshake(t, ca, key); err != nil || name != "sam" {
		t.Fatalf("a handshake signed by ssh-agent: %q %v", name, err)
	}

	ecSSH, _ := ssh.NewPublicKey(&ec.PublicKey)
	if _, err := openSSHKey("agent:" + ssh.FingerprintSHA256(ecSSH)); err == nil {
		t.Error("an ECDSA key from ssh-agent")
	}

	// The same ECDSA key from its file; its .pub picks the agent's key.
	dir := t.TempDir()
	block, _ := ssh.MarshalPrivateKey(ec, "")
	keyFile := filepath.Join(dir, "id_ecdsa")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	fk, err := openSSHKey(keyFile)
	if err != nil || fk.spec != "file:"+keyFile {
		t.Fatalf("the key file: %v", err)
	}
	if name, err := handshake(t, ca, fk); err != nil || name != "sam" {
		t.Fatalf("a handshake with the key file: %q %v", name, err)
	}
	pubFile := filepath.Join(dir, "id_ed25519.pub")
	if err := os.WriteFile(pubFile, ssh.MarshalAuthorizedKey(edSSH), 0o600); err != nil {
		t.Fatal(err)
	}
	if pk, err := openSSHKey(pubFile); err != nil || pk.spec != key.spec {
		t.Errorf("a .pub: %v", err)
	}

	encBlock, _ := ssh.MarshalPrivateKeyWithPassphrase(ec, "", []byte("secret"))
	encFile := filepath.Join(dir, "id_enc")
	if err := os.WriteFile(encFile, pem.EncodeToMemory(encBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	if !encryptedKeyFile("file:"+encFile) || encryptedKeyFile("file:"+keyFile) {
		t.Error("encryptedKeyFile")
	}
}
