package main

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

// SSH keys: janusctl login -ssh-key signs the Controller's challenge with
// one of the account's SSH keys (internal/sshsig), and the certificate it
// gets is for that key itself - the key then signs janusctl's TLS
// handshakes with the nodes too. From its file: Ed25519, ECDSA or RSA
// (a passphrase is asked on the terminal). From ssh-agent: Ed25519 only -
// TLS 1.3 hands an ECDSA or RSA key a digest to sign, and an agent only
// signs whole messages.

// sshKey is a key janusctl signs with: SSH's way (the challenge) and
// TLS's (the handshakes).
type sshKey struct {
	// spec is how the context finds it again: "agent:<fingerprint>" or
	// "file:<path>".
	spec   string
	ssh    ssh.Signer
	tls    crypto.Signer
	closer io.Closer
}

func (k *sshKey) fingerprint() string { return ssh.FingerprintSHA256(k.ssh.PublicKey()) }

func (k *sshKey) Close() {
	if k.closer != nil {
		k.closer.Close()
	}
}

// openSSHKey opens the key spec names - "agent:<fingerprint>" or
// "file:<path>" as a context keeps it, or what -ssh-key was given: a
// private key file, or a public one (.pub) whose key ssh-agent holds.
// Empty: ssh-agent's first Ed25519 key.
func openSSHKey(spec string) (*sshKey, error) {
	switch {
	case strings.HasPrefix(spec, "agent:"):
		return agentKey(strings.TrimPrefix(spec, "agent:"))
	case strings.HasPrefix(spec, "file:"):
		return fileKey(strings.TrimPrefix(spec, "file:"))
	case spec == "":
		return agentKey("")
	}
	data, err := os.ReadFile(spec)
	if err != nil {
		return nil, err
	}
	if pub, _, _, _, err := ssh.ParseAuthorizedKey(data); err == nil {
		return agentKey(ssh.FingerprintSHA256(pub))
	}
	return fileKey(spec)
}

// encryptedKeyFile reports whether spec is a key file a passphrase
// protects.
func encryptedKeyFile(spec string) bool {
	path, ok := strings.CutPrefix(spec, "file:")
	if !ok {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_, err = ssh.ParseRawPrivateKey(data)
	var missing *ssh.PassphraseMissingError
	return errors.As(err, &missing)
}

func fileKey(path string) (*sshKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := ssh.ParseRawPrivateKey(data)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		pass, perr := askPassphrase(fmt.Sprintf("Passphrase for %s: ", path))
		if perr != nil {
			return nil, perr
		}
		raw, err = ssh.ParseRawPrivateKeyWithPassphrase(data, pass)
	}
	if err != nil {
		return nil, fmt.Errorf("the SSH key %s: %w", path, err)
	}
	if p, ok := raw.(*ed25519.PrivateKey); ok {
		raw = *p
	}
	signer, ok := raw.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("the SSH key %s can't sign", path)
	}
	sshSigner, err := ssh.NewSignerFromSigner(signer)
	if err != nil {
		return nil, err
	}
	return &sshKey{spec: "file:" + path, ssh: sshSigner, tls: signer}, nil
}

// askPassphrase reads a passphrase from the terminal, unechoed.
func askPassphrase(prompt string) ([]byte, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("the key file needs its passphrase and there's no terminal to ask it on: add the key to ssh-agent")
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	pass, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	return pass, err
}

func agentKey(fingerprint string) (*sshKey, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, errors.New("no ssh-agent (SSH_AUTH_SOCK): give the key file with -ssh-key")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("ssh-agent: %w", err)
	}
	ag := agent.NewClient(conn)
	signers, err := ag.Signers()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh-agent: %w", err)
	}
	for _, s := range signers {
		pub := s.PublicKey()
		if fingerprint != "" && ssh.FingerprintSHA256(pub) != fingerprint {
			continue
		}
		if pub.Type() != ssh.KeyAlgoED25519 {
			if fingerprint != "" {
				conn.Close()
				return nil, fmt.Errorf("a %s key in ssh-agent can't sign janusctl's connections: in ssh-agent, Ed25519 only - give an ECDSA or RSA key's file with -ssh-key", pub.Type())
			}
			continue
		}
		// The agent's own key type carries no crypto key: parsed again.
		parsed, err := ssh.ParsePublicKey(pub.Marshal())
		if err != nil {
			conn.Close()
			return nil, err
		}
		edPub, ok := parsed.(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey)
		if !ok {
			conn.Close()
			return nil, errors.New("ssh-agent: an unreadable Ed25519 key")
		}
		return &sshKey{spec: "agent:" + ssh.FingerprintSHA256(pub), ssh: s, tls: agentSigner{ag: ag, key: pub, pub: edPub}, closer: conn}, nil
	}
	conn.Close()
	if fingerprint != "" {
		return nil, fmt.Errorf("ssh-agent doesn't hold the key %s: ssh-add it", fingerprint)
	}
	return nil, errors.New("ssh-agent holds no Ed25519 key: ssh-add one, or give a key file with -ssh-key")
}

// agentSigner signs TLS handshakes with an Ed25519 key in ssh-agent: TLS
// 1.3 hands an Ed25519 key the whole message, which is what an agent
// signs.
type agentSigner struct {
	ag  agent.Agent
	key ssh.PublicKey
	pub ed25519.PublicKey
}

func (s agentSigner) Public() crypto.PublicKey { return s.pub }

func (s agentSigner) Sign(_ io.Reader, message []byte, opts crypto.SignerOpts) ([]byte, error) {
	if opts.HashFunc() != crypto.Hash(0) {
		return nil, errors.New("ssh-agent signs Ed25519 messages only")
	}
	sig, err := s.ag.Sign(s.key, message)
	if err != nil {
		return nil, fmt.Errorf("ssh-agent: %w", err)
	}
	return sig.Blob, nil
}

// randReader is where SSH signatures take their randomness.
var randReader = rand.Reader
