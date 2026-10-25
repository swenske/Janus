package libvirt

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// A host is its pinned SSH host key: two records reaching one sshd are
// one host, another key another host - whatever libvirtd says its UUID
// is (random on a board without one, a placeholder on many).
func TestHostID(t *testing.T) {
	key := func() ssh.PublicKey {
		t.Helper()
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		k, err := ssh.NewPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	kvm01, kvm02 := key(), key()
	a := &Driver{hostKey: kvm01}
	b := &Driver{hostKey: kvm01} // another record, "localhost" for "127.0.0.1"
	c := &Driver{hostKey: kvm02}
	if a.HostID() != b.HostID() || !strings.HasPrefix(a.HostID(), "libvirt:SHA256:") {
		t.Errorf("one sshd, two IDs: %q %q", a.HostID(), b.HostID())
	}
	if a.HostID() == c.HostID() {
		t.Errorf("two hosts, one ID: %q", a.HostID())
	}
}
