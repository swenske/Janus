package libvirt

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// What to run on a libvirt host, as root, before the Controller can use
// it: docs/hypervisors.md's "Preparing a libvirt host" with one
// hypervisor's own values - its account, pool, networks and name
// prefix. Every step stands alone and can run again.
//
// The values are the ones Validate accepts (letters, digits, '_', '.',
// '-'), so they need no quoting; the hypervisor's name only ever goes in
// comments.

// ControllersGroup is the group every Janus Controller's account on a
// host joins: each account's polkit rule leaves the others to their own
// rule, instead of granting them everything as members of the libvirt
// group.
const ControllersGroup = "janus-controllers"

type prepData struct {
	Name, User, Group, Pool, PoolDir, Prefix, Table, Unit, Key string
	Networks                                                   []string
}

var tableRe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// HostPreparation gives the steps for c. authorizedKey is the
// Controller's key for this hypervisor - "" before it's added, when
// there's none yet.
func HostPreparation(name string, c *hypervisor.LibvirtConfig, authorizedKey string) []hypervisor.PrepStep {
	d := prepData{
		Name:     hypervisor.CommentSafe(name),
		User:     c.User,
		Group:    ControllersGroup,
		Pool:     c.Pool,
		PoolDir:  "/var/lib/libvirt/" + c.Pool,
		Prefix:   c.Prefix(),
		Table:    tableRe.ReplaceAllString(c.User, "_"),
		Unit:     c.User + "-egress",
		Key:      strings.TrimSpace(authorizedKey),
		Networks: c.Networks,
	}
	var steps []hypervisor.PrepStep
	for _, s := range prepSteps {
		steps = append(steps, hypervisor.PrepStep{Title: render(s.title, d), About: render(s.about, d), Script: render(s.script, d)})
	}
	return steps
}

func render(text string, d prepData) string {
	var b strings.Builder
	if err := template.Must(template.New("").Funcs(template.FuncMap{
		"js": func(s []string) string {
			q := make([]string, len(s))
			for i, v := range s {
				q[i] = fmt.Sprintf("%q", v)
			}
			return "[" + strings.Join(q, ", ") + "]"
		},
		"words": func(s []string) string { return strings.Join(s, " ") },
	}).Parse(text)).Execute(&b, d); err != nil {
		panic(err) // the templates are constants: a test runs them all
	}
	return b.String()
}

var prepSteps = []struct{ title, about, script string }{
	{
		title: "The Controller's account",
		about: "A dedicated account with no password and no shell. The libvirt group gives it libvirt's socket; " +
			"{{.Group}} is the group of every Janus Controller's account on this host (step 5).",
		script: `getent group libvirt >/dev/null || { echo "There's no libvirt group: is libvirt installed?" >&2; exit 1; }
getent group {{.Group}} >/dev/null || groupadd --system {{.Group}}
id -u {{.User}} >/dev/null 2>&1 || useradd --create-home --shell /usr/sbin/nologin {{.User}}
usermod --append --groups libvirt,{{.Group}} {{.User}}
passwd --lock {{.User}} >/dev/null
install -d -m 700 -o {{.User}} -g "$(id -gn {{.User}})" ~{{.User}}/.ssh
[ -e ~{{.User}}/.ssh/authorized_keys ] || install -m 600 -o {{.User}} -g "$(id -gn {{.User}})" /dev/null ~{{.User}}/.ssh/authorized_keys
{{if .Key -}}
grep -qxF '{{.Key}}' ~{{.User}}/.ssh/authorized_keys || echo '{{.Key}}' >> ~{{.User}}/.ssh/authorized_keys
{{- else -}}
# The Controller's key for this hypervisor goes in ~{{.User}}/.ssh/authorized_keys:
# it's made when the hypervisor is added - its card then gives the command.
{{- end}}
`,
	},
	{
		title: "sshd: nothing but libvirt's socket",
		about: "The account gets no shell, no TTY and nothing listening on its behalf: only libvirt's Unix socket. " +
			"AllowTcpForwarding has to stay local - with no, OpenSSH refuses Unix sockets too - and step 3 makes sure no TCP gets anywhere.",
		script: `grep -qi '^Include /etc/ssh/sshd_config.d/' /etc/ssh/sshd_config || echo "warning: /etc/ssh/sshd_config doesn't include sshd_config.d" >&2
# Its earlier name, from a docs/hypervisors.md that named no account.
if grep -qx 'Match User {{.User}}' /etc/ssh/sshd_config.d/50-janus-controller.conf 2>/dev/null; then rm /etc/ssh/sshd_config.d/50-janus-controller.conf; fi
cat > /etc/ssh/sshd_config.d/50-{{.User}}.conf <<'JANUS'
# Janus Controller, hypervisor {{.Name}}: its account only opens libvirt's socket.
Match User {{.User}}
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    AllowTcpForwarding local
    AllowStreamLocalForwarding local
    PermitListen none
    X11Forwarding no
    AllowAgentForwarding no
    PermitTTY no
    ForceCommand /usr/bin/false
JANUS
sshd -t
systemctl reload ssh 2>/dev/null || systemctl reload sshd
sshd -T -C user={{.User}},host=localhost,addr=127.0.0.1 | grep -qx 'allowstreamlocalforwarding local' ||
    echo "warning: another sshd setting wins over 50-{{.User}}.conf for {{.User}}: check sshd -T -C user={{.User}}" >&2
`,
	},
	{
		title: "No network traffic from that account",
		about: "It only ever needs libvirt's Unix socket: an nftables table, loaded at boot, rejects any IP traffic it would send, " +
			"so a stolen key can't reach other machines through this host. The Controller's own SSH connection isn't affected - its socket belongs to sshd.",
		script: `mkdir -p /etc/nftables.d
cat > /etc/nftables.d/{{.User}}.nft <<'JANUS'
table inet {{.Table}}
delete table inet {{.Table}}
table inet {{.Table}} {
	chain output {
		type filter hook output priority filter; policy accept;
		meta skuid "{{.User}}" counter reject
	}
}
JANUS
cat > /etc/systemd/system/{{.Unit}}.service <<'JANUS'
[Unit]
Description=No IP traffic from the Janus Controller account {{.User}}
Before=ssh.service
After=nss-user-lookup.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/nftables.d/{{.User}}.nft
ExecStop=/usr/sbin/nft delete table inet {{.Table}}

[Install]
WantedBy=multi-user.target
JANUS
systemctl daemon-reload
systemctl enable --quiet {{.Unit}}
systemctl restart {{.Unit}}
`,
	},
	{
		title: "The storage pool, and the networks",
		about: "Pool {{.Pool}} (type dir) holds the base images and the machines' disks. " +
			"The networks its machines may use ({{words .Networks}}) must already be in libvirt: they're only checked.",
		script: `install -d -m 711 {{.PoolDir}}
virsh -q pool-info {{.Pool}} >/dev/null 2>&1 || virsh -q pool-define-as {{.Pool}} dir --target {{.PoolDir}}
virsh -q pool-info {{.Pool}} | grep -q '^State: *running' || virsh -q pool-start {{.Pool}}
virsh -q pool-autostart {{.Pool}}
for net in {{words .Networks}}; do
    virsh -q net-info "$net" >/dev/null 2>&1 || echo "warning: libvirt has no network $net" >&2
done
`,
	},
	{
		title: "polkit: libvirt enforces the boundary",
		about: "Steps 1 to 4 are enough to work, but the libvirt group can do anything libvirt can - root on this host, in effect. " +
			"With libvirt's polkit access driver, every API call is checked: {{.User}} only sees and acts on domains named {{.Prefix}}*, pool {{.Pool}} " +
			"and networks {{words .Networks}}. Root and the rest of the libvirt group keep every right they had; another Controller's account " +
			"({{.Group}}) is left to its own rule. Turning the driver on restarts libvirtd once - running machines aren't affected.",
		script: `# Its earlier name, from a docs/hypervisors.md that named no account.
if grep -q 'JANUS_USER = "{{.User}}";' /etc/polkit-1/rules.d/50-janus-controller.rules 2>/dev/null; then rm /etc/polkit-1/rules.d/50-janus-controller.rules; fi
cat > /etc/polkit-1/rules.d/50-{{.User}}.rules <<'JANUS'
// Janus Controller, hypervisor {{.Name}} (docs/hypervisors.md): what its
// account may do through libvirt once libvirt checks every API call with
// polkit - its own virtual machines, its pool, its networks, and reading
// the host. In a function of its own: every rules file shares one scope.
(function () {
    var JANUS_USER = "{{.User}}";
    var JANUS_PREFIX = "{{.Prefix}}";
    var JANUS_POOL = "{{.Pool}}";
    var JANUS_NETWORKS = {{js .Networks}};
    // Every Janus Controller's account: each one has its own rule.
    var JANUS_GROUP = "{{.Group}}";

    var ALLOWED = {
        "connect": ["getattr", "read", "search-domains", "search-networks", "search-storage-pools"],
        "domain": ["getattr", "read", "write", "save", "delete", "start", "stop", "reset", "open-device"],
        "storage-pool": ["getattr", "read", "refresh", "search-storage-vols"],
        "storage-vol": ["getattr", "read", "create", "delete", "data-read", "data-write"],
        "network": ["getattr", "read"],
        // Starting a machine plugs its interfaces into the networks.
        "network-port": ["getattr", "read", "create", "delete"]
    };

    function denied(action) {
        polkit.log(JANUS_USER + ": denied " + action.id + " domain=" + action.lookup("domain_name") +
            " pool=" + action.lookup("pool_name") + " network=" + action.lookup("network_name"));
        return polkit.Result.NO;
    }

    polkit.addRule(function (action, subject) {
        if (action.id.indexOf("org.libvirt.api.") != 0) {
            return polkit.Result.NOT_HANDLED;
        }
        if (subject.user != JANUS_USER) {
            if (subject.isInGroup(JANUS_GROUP)) {
                return polkit.Result.NOT_HANDLED;
            }
            if (subject.user == "root" || subject.isInGroup("libvirt")) {
                return polkit.Result.YES;
            }
            return polkit.Result.NOT_HANDLED;
        }
        var parts = action.id.substr("org.libvirt.api.".length).split(".");
        var allowed = ALLOWED[parts[0]];
        if (!allowed || allowed.indexOf(parts[1]) < 0) {
            return denied(action);
        }
        switch (parts[0]) {
        case "domain":
            return String(action.lookup("domain_name")).indexOf(JANUS_PREFIX) == 0 ? polkit.Result.YES : denied(action);
        case "storage-pool":
        case "storage-vol":
            return action.lookup("pool_name") == JANUS_POOL ? polkit.Result.YES : denied(action);
        case "network":
        case "network-port":
            return JANUS_NETWORKS.indexOf(action.lookup("network_name")) >= 0 ? polkit.Result.YES : denied(action);
        }
        return polkit.Result.YES;
    });
})();
JANUS
if systemctl is-enabled --quiet libvirtd.service 2>/dev/null || systemctl is-active --quiet libvirtd.service; then
    if ! grep -q '^access_drivers' /etc/libvirt/libvirtd.conf; then
        echo 'access_drivers = [ "polkit" ]' >> /etc/libvirt/libvirtd.conf
        systemctl try-restart libvirtd.service
    fi
    grep -q '^access_drivers.*"polkit"' /etc/libvirt/libvirtd.conf || echo "warning: /etc/libvirt/libvirtd.conf sets access_drivers without polkit" >&2
else
    echo "warning: no libvirtd - with libvirt's modular daemons, add access_drivers = [ \"polkit\" ] to virtqemud.conf, virtstoraged.conf and virtnetworkd.conf, then restart them" >&2
fi
`,
	},
	{
		title: "Check",
		about: "What {{.User}} sees in libvirt - its own machines only - and this host's SSH key: the fingerprint the Controller reads must be this one.",
		script: `echo "{{.User}} sees these domains:"
runuser -u {{.User}} -- virsh -c qemu:///system list --all --name
echo "This host's SSH key:"
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
`,
	},
}
