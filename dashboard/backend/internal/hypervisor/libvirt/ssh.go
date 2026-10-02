package libvirt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const sshTimeout = 15 * time.Second

// sshDialer is go-libvirt's socket.Dialer over SSH, in Go: it logs in
// with the Controller's own key, accepts only the pinned host key, and
// opens a channel to libvirt's Unix socket on the host (sshd's
// direct-streamlocal forwarding) - no ssh binary, nothing on the host
// beyond sshd and libvirt.
type sshDialer struct {
	addr, user, socket string
	signer             ssh.Signer
	hostKey            ssh.PublicKey

	mu     sync.Mutex
	client *ssh.Client
	closed bool
}

func (d *sshDialer) Dial() (net.Conn, error) {
	cfg := &ssh.ClientConfig{
		User:              d.user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(d.signer)},
		HostKeyCallback:   ssh.FixedHostKey(d.hostKey),
		HostKeyAlgorithms: hostKeyAlgorithms(d.hostKey),
		Timeout:           sshTimeout,
	}
	client, err := ssh.Dial("tcp", d.addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s: %w", d.user, d.addr, err)
	}
	conn, err := client.Dial("unix", d.socket)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("open libvirt's socket %s on %s: %w", d.socket, d.addr, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		client.Close()
		return nil, errors.New("connection closed")
	}
	d.client = client
	return conn, nil
}

// close tears the SSH connection down, which ends any libvirt call
// still waiting on it.
func (d *sshDialer) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.client != nil {
		d.client.Close()
	}
}

// hostKeyAlgorithms makes the server present the pinned key: with the
// default list, a server holding several host keys may offer another
// one, which FixedHostKey would then refuse.
func hostKeyAlgorithms(key ssh.PublicKey) []string {
	if key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{key.Type()}
}

var errProbed = errors.New("host key captured")

// ProbeHostKey connects to addr's SSH server only far enough to read the
// host key it presents - nothing is authenticated or trusted. The
// operator compares its fingerprint with the host's own
// (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`) before pinning it.
func ProbeHostKey(ctx context.Context, addr string) (ssh.PublicKey, error) {
	dialer := net.Dialer{Timeout: sshTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var key ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: "probe",
		// Ed25519 first: the key the operator is told to compare with
		// (ssh_host_ed25519_key.pub), when the host has one.
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errProbed
		},
	}
	_, _, _, err = ssh.NewClientConn(conn, addr, cfg)
	if key != nil {
		return key, nil
	}
	if err == nil {
		err = errors.New("the server presented no host key")
	}
	return nil, fmt.Errorf("ssh handshake with %s: %w", addr, err)
}
