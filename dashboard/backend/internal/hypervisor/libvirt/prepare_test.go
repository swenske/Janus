package libvirt

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

func TestHostPreparation(t *testing.T) {
	c := &hypervisor.LibvirtConfig{Host: "kvm01", User: "janus-ctl2", Pool: "janus2", Networks: []string{"lan", "dmz"}, NamePrefix: "j2-"}
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOKd8n6l0Xc1Vf2B0Yw3fN5mJ8vVvYb6ehgZK1vE0pQ7 janus-controller-0123abcd"
	for _, tc := range []struct{ name, key string }{{"before it's added", ""}, {"added", key}} {
		steps := HostPreparation("kvm!01\n`$(reboot)`", c, tc.key)
		if len(steps) != 6 {
			t.Fatalf("%s: %d steps", tc.name, len(steps))
		}
		script := hypervisor.PreparationScript("kvm!01\n`$(reboot)`", steps)
		for _, s := range steps {
			if s.Title == "" || s.About == "" || s.Script == "" || !strings.HasSuffix(s.Script, "\n") {
				t.Errorf("%s: step %+v", tc.name, s)
			}
			// Each step stands alone: a here-document never spans two.
			if strings.Count(s.Script, "<<'JANUS'") != strings.Count("\n"+s.Script, "\nJANUS\n") {
				t.Errorf("%s: unbalanced here-documents in %q", tc.name, s.Title)
			}
			if out, err := exec.Command("sh", "-n", "-c", s.Script).CombinedOutput(); err != nil {
				t.Errorf("%s: step %q: sh -n: %v %s", tc.name, s.Title, err, out)
			}
			// Each step can be pasted into an interactive bash.
			if line := hypervisor.PasteUnsafe(s.Script); line != "" {
				t.Errorf("%s: step %q: bash would take a history expansion in %q", tc.name, s.Title, line)
			}
		}
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: script: sh -n: %v %s", tc.name, err, out)
		}
		for _, want := range []string{
			"#!/bin/sh\n", "set -eu\n", "# --- 5. polkit: libvirt enforces the boundary ---\n",
			"useradd --create-home --shell /usr/sbin/nologin janus-ctl2\n",
			"usermod --append --groups libvirt,janus-controllers janus-ctl2\n",
			"/etc/ssh/sshd_config.d/50-janus-ctl2.conf", "Match User janus-ctl2\n",
			"table inet janus_ctl2 {", `meta skuid "janus-ctl2" counter reject`, "systemctl restart janus-ctl2-egress\n",
			"virsh -q pool-define-as janus2 dir --target /var/lib/libvirt/janus2\n", "for net in lan dmz; do",
			"/etc/polkit-1/rules.d/50-janus-ctl2.rules", `var JANUS_PREFIX = "j2-";`, `var JANUS_POOL = "janus2";`,
			`var JANUS_NETWORKS = ["lan", "dmz"];`, `var JANUS_GROUP = "janus-controllers";`, "(function () {", "})();\n",
			"runuser -u janus-ctl2 -- virsh -c qemu:///system list --all --name\n",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: no %q in the script", tc.name, want)
			}
		}
		// The free-form name stays in comments, on one line.
		if strings.Contains(script, "\n`$(reboot)`") {
			t.Errorf("%s: the name broke out of its comment", tc.name)
		}
		hasKey := strings.Contains(script, "grep -qxF '"+key+"' ~janus-ctl2/.ssh/authorized_keys || echo '"+key+"' >> ~janus-ctl2/.ssh/authorized_keys\n")
		if hasKey != (tc.key != "") {
			t.Errorf("%s: key line in the script: %v", tc.name, hasKey)
		}
		if tc.key == "" && !strings.Contains(script, "its card then gives the command") {
			t.Errorf("%s: doesn't say where the key comes from", tc.name)
		}
	}
	// The default prefix, the documented account.
	steps := HostPreparation("kvm01", &hypervisor.LibvirtConfig{Host: "kvm01", User: "janus-ctl", Pool: "janus", Networks: []string{"lan"}}, "")
	if s := hypervisor.PreparationScript("kvm01", steps); !strings.Contains(s, `var JANUS_PREFIX = "janus-";`) || !strings.Contains(s, "table inet janus_ctl {") {
		t.Error("defaults: no janus- prefix or janus_ctl table")
	}
}

// docs/hypervisors.md shows the preparation for its example hypervisor:
// it's this one, word for word.
func TestDocsShowThePreparation(t *testing.T) {
	raw, err := os.ReadFile("../../../../../docs/hypervisors.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	c := &hypervisor.LibvirtConfig{Host: "kvm01.example.net", User: "janus-ctl", Pool: "janus", Networks: []string{"lan", "dmz"}}
	for i, s := range HostPreparation("kvm01", c, "") {
		if !strings.Contains(doc, "```sh\n"+s.Script+"```\n") {
			t.Errorf("docs/hypervisors.md doesn't show step %d (%s) as the Controller writes it:\n%s", i+1, s.Title, s.Script)
		}
	}
}
