package hypervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func validLibvirt() *Hypervisor {
	return &Hypervisor{Name: "kvm01", Kind: KindLibvirt, Libvirt: &LibvirtConfig{
		Host: "10.200.10.2", User: "janus-ctl", Pool: "janus", Networks: []string{"lab-mgmt", "lab-front"},
	}}
}

func TestStoreAddReloadUpdateRemove(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := validLibvirt()
	if err := s.Add(h); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if h.ID == "" || len(h.SSHKey) == 0 {
		t.Fatal("Add didn't set an ID and a key")
	}
	if fi, err := os.Stat(filepath.Join(dir, "hypervisors", h.ID, "ssh.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ssh.key: %v, %v", fi, err)
	}
	line := h.AuthorizedKey()
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil || !strings.HasPrefix(line, "ssh-ed25519 ") {
		t.Errorf("AuthorizedKey = %q (%v)", line, err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := s2.Get(h.ID)
	if !ok || got.Name != "kvm01" || got.Libvirt.Pool != "janus" || got.AuthorizedKey() != line {
		t.Fatalf("reloaded %+v", got)
	}

	got.Libvirt.HostKey = strings.TrimSuffix(line, " janus-controller-"+h.ID)
	got.Kind = "something-else" // kept from the stored one
	if err := s2.Update(got); err == nil {
		t.Fatal("Update validated an unknown kind")
	}
	got.Kind = KindLibvirt
	if err := s2.Update(got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if again, _ := s2.Get(h.ID); again.Libvirt.HostKey == "" || again.AuthorizedKey() != line {
		t.Error("Update lost the host key or changed the SSH key")
	}

	// The host it reaches is the status's to record, kept across an
	// Update made from a form that doesn't carry it, and a reload.
	if err := s2.SetHost(h.ID, "libvirt:4c4c4544-0042", "kvm01"); err != nil {
		t.Fatal(err)
	}
	form, _ := s2.Get(h.ID)
	form.HostID, form.HostName = "", ""
	if err := s2.Update(form); err != nil {
		t.Fatal(err)
	}
	if s3, err := Open(dir); err != nil {
		t.Fatal(err)
	} else if again, _ := s3.Get(h.ID); again.HostID != "libvirt:4c4c4544-0042" || again.HostName != "kvm01" {
		t.Errorf("the host after an Update and a reload: %q %q", again.HostID, again.HostName)
	}

	// Get returns copies: changing one changes nothing stored.
	c, _ := s2.Get(h.ID)
	c.Libvirt.Networks[0] = "changed"
	if again, _ := s2.Get(h.ID); again.Libvirt.Networks[0] != "lab-mgmt" {
		t.Error("Get returned shared state")
	}

	if err := s2.Remove(h.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hypervisors", h.ID)); !os.IsNotExist(err) {
		t.Errorf("files left after Remove: %v", err)
	}
}

func TestValidate(t *testing.T) {
	for name, mutate := range map[string]func(*Hypervisor){
		"no name":      func(h *Hypervisor) { h.Name = " " },
		"no host":      func(h *Hypervisor) { h.Libvirt.Host = "" },
		"bad user":     func(h *Hypervisor) { h.Libvirt.User = "root;rm" },
		"bad pool":     func(h *Hypervisor) { h.Libvirt.Pool = "../x" },
		"no networks":  func(h *Hypervisor) { h.Libvirt.Networks = nil },
		"bad network":  func(h *Hypervisor) { h.Libvirt.Networks = []string{"a b"} },
		"bad prefix":   func(h *Hypervisor) { h.Libvirt.NamePrefix = "x/" },
		"bad host key": func(h *Hypervisor) { h.Libvirt.HostKey = "nope" },
		"rel socket":   func(h *Hypervisor) { h.Libvirt.Socket = "libvirt-sock" },
		"no settings":  func(h *Hypervisor) { h.Libvirt = nil },
		"bad label":    func(h *Hypervisor) { h.Labels = map[string]string{"DC": "lgs"} },
		"bad value":    func(h *Hypervisor) { h.Labels = map[string]string{"dc": "a b"} },
	} {
		h := validLibvirt()
		mutate(h)
		if err := h.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validLibvirt().Validate(); err != nil {
		t.Errorf("valid config refused: %v", err)
	}
	if got := validLibvirt().Libvirt.SSHAddress(); got != "10.200.10.2:22" {
		t.Errorf("SSHAddress = %s", got)
	}
}

// A hypervisor's labels: saved with it, given back by a reload, replaced
// by an Update, never shared with what Get returns.
func TestLabels(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := validLibvirt()
	h.Labels = map[string]string{"dc": "lgs", "rack": "12"}
	if err := s.Add(h); err != nil {
		t.Fatal(err)
	}
	h.Labels["dc"] = "changed" // the caller's map: not the store's
	got, _ := s.Get(h.ID)
	if got.Labels["dc"] != "lgs" {
		t.Errorf("Add kept the caller's map: %v", got.Labels)
	}
	got.Labels["dc"] = "changed"
	if again, _ := s.Get(h.ID); again.Labels["dc"] != "lgs" {
		t.Error("Get returned the store's labels")
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := s2.Get(h.ID)
	if len(reloaded.Labels) != 2 || reloaded.Labels["dc"] != "lgs" || reloaded.Labels["rack"] != "12" {
		t.Fatalf("reloaded labels: %v", reloaded.Labels)
	}
	reloaded.Labels = map[string]string{"dc": "scw"}
	if err := s2.Update(reloaded); err != nil {
		t.Fatal(err)
	}
	if again, _ := s2.Get(h.ID); len(again.Labels) != 1 || again.Labels["dc"] != "scw" {
		t.Errorf("updated labels: %v", again.Labels)
	}
	reloaded.Labels = map[string]string{"Bad": "x"}
	if err := s2.Update(reloaded); err == nil {
		t.Error("Update took a bad label")
	}
	if err := s2.SetHost(h.ID, "libvirt:x", "kvm01"); err != nil {
		t.Fatal(err)
	}
	if again, _ := s2.Get(h.ID); again.Labels["dc"] != "scw" {
		t.Errorf("SetHost lost the labels: %v", again.Labels)
	}
}
