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
	steps := HostPreparation("pve!1\n`$(reboot)`", c)
	script := hypervisor.PreparationScript("pve!1\n`$(reboot)`", steps)
	for _, s := range append(steps, hypervisor.PrepStep{Title: "all", About: "all", Script: script}) {
		if s.Title == "" || s.About == "" || !strings.HasSuffix(s.Script, "\n") {
			t.Errorf("step %+v", s)
		}
		if out, err := exec.Command("sh", "-n", "-c", s.Script).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v %s", s.Title, err, out)
		}
		// Each step can be pasted into an interactive bash - the token's
		// "user!name" included.
		if line := hypervisor.PasteUnsafe(s.Script); line != "" {
			t.Errorf("%s: bash would take a history expansion in %q", s.Title, line)
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
		Storage: "local-lvm", ImageStorage: "janus-images", Networks: []string{"vmbr0.10", "vmbr0.100-109"},
	}
	for i, s := range HostPreparation("pve01", c) {
		if !strings.Contains(string(raw), "```sh\n"+s.Script+"```\n") {
			t.Errorf("docs/hypervisors.md doesn't show step %d (%s) as the Controller writes it:\n%s", i+1, s.Title, s.Script)
		}
	}
}

// Each kind of allowed network gets its right: a VLAN its own, a range
// each of its VLANs, bridge.* the bridge propagated (which also covers
// the bridge alone, and any VLAN or range named besides).
func TestPreparationNetworks(t *testing.T) {
	c := &hypervisor.ProxmoxConfig{
		URL: "https://pve:8006", Node: "pve1", TokenID: "janus-ctl@pve!controller", Pool: "janus", Storage: "local-lvm", ImageStorage: "janus-images",
		Networks: []string{"vmbr0.10", "vmbr0.100-109", "vmbr1", "vmbr2", "vmbr2.*", "vmbr2.20"},
	}
	script := HostPreparation("pve1", c)[2].Script
	for _, want := range []string{
		"pveum acl modify /sdn/zones/localnetwork/vmbr0/10 --users janus-ctl@pve --roles JanusNetwork\n",
		"for vlan in $(seq 100 109); do pveum acl modify /sdn/zones/localnetwork/vmbr0/$vlan --users janus-ctl@pve --roles JanusNetwork; done\n",
		"pveum acl modify /sdn/zones/localnetwork/vmbr1 --users janus-ctl@pve --roles JanusNetwork --propagate 0\n",
		"pveum acl modify /sdn/zones/localnetwork/vmbr2 --users janus-ctl@pve --roles JanusNetwork\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("no %q in:\n%s", want, script)
		}
	}
	for _, unwanted := range []string{"vmbr2 --users janus-ctl@pve --roles JanusNetwork --propagate 0", "vmbr2/20"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("%q in:\n%s", unwanted, script)
		}
	}
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v %s", err, out)
	}
	if line := hypervisor.PasteUnsafe(script); line != "" {
		t.Errorf("bash would expand %q", line)
	}
}
