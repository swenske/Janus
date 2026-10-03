package proxmox

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

func TestHostPreparation(t *testing.T) {
	c := &hypervisor.ProxmoxConfig{
		URL: "https://pve:8006", Node: "pve1", TokenID: "janus-ctl@pve!controller", Pool: "janus",
		Storage: "local-lvm", ImageStorage: "janus-images", Networks: []string{"vmbr0.10", "vmbr1"},
	}
	steps := HostPreparation("pve1\n`$(reboot)`", c)
	script := hypervisor.PreparationScript("pve1\n`$(reboot)`", steps)
	for _, s := range append(steps, hypervisor.PrepStep{Title: "all", About: "all", Script: script}) {
		if s.Title == "" || s.About == "" || !strings.HasSuffix(s.Script, "\n") {
			t.Errorf("step %+v", s)
		}
		if out, err := exec.Command("sh", "-n", "-c", s.Script).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v %s", s.Title, err, out)
		}
	}
	for _, want := range []string{
		"pvesh create /pools --poolid janus ",
		"pvesm add dir janus-images --path /var/lib/janus-images --content import,iso --nodes pve1\n",
		`role JanusVM "` + privsVM + `"`,
		`role JanusImages "Datastore.Allocate,`,
		"pveum acl modify /pool/janus --users janus-ctl@pve --roles JanusVM\n",
		"pveum acl modify /storage/local-lvm --users janus-ctl@pve --roles JanusDisks\n",
		"pveum acl modify /storage/janus-images --users janus-ctl@pve --roles JanusImages\n",
		"pveum acl modify /sdn/zones/localnetwork/vmbr0/10 --users janus-ctl@pve --roles JanusNetwork\n",
		// An untagged bridge: not every VLAN on it.
		"pveum acl modify /sdn/zones/localnetwork/vmbr1 --users janus-ctl@pve --roles JanusNetwork --propagate 0\n",
		"pveum acl modify /nodes/pve1 --users janus-ctl@pve --roles JanusNode\n",
		"pveum user token add janus-ctl@pve controller --privsep 0 ",
		"openssl x509 -in \"$f\" -noout -fingerprint -sha256\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("no %q in the script", want)
		}
	}
	if strings.Contains(script, "\n`$(reboot)`") {
		t.Error("the name broke out of its comment")
	}
}

// docs/hypervisors.md shows the preparation for its example node: it's
// this one, word for word.
func TestDocsShowThePreparation(t *testing.T) {
	raw, err := os.ReadFile("../../../../../docs/hypervisors.md")
	if err != nil {
		t.Fatal(err)
	}
	c := &hypervisor.ProxmoxConfig{
		URL: "https://pve01.example.net:8006", Node: "pve01", TokenID: "janus-ctl@pve!controller", Pool: "janus",
		Storage: "local-lvm", ImageStorage: "janus-images", Networks: []string{"vmbr0.10", "vmbr0.20"},
	}
	for i, s := range HostPreparation("pve01", c) {
		if !strings.Contains(string(raw), "```sh\n"+s.Script+"```\n") {
			t.Errorf("docs/hypervisors.md doesn't show step %d (%s) as the Controller writes it:\n%s", i+1, s.Title, s.Script)
		}
	}
}
