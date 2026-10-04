# Hypervisors: nodes the Controller creates itself

The Controller can be given hypervisors - libvirt/KVM hosts and
Proxmox VE nodes - and create, power and destroy Janus nodes on them
itself. A node it creates
is admitted as soon as it registers, without the approval every other
registration waits for: creating it was the approval.

The Controller only ever acts on the virtual machines it created. Every
one carries an ownership tag - in its libvirt `<metadata>`, in its
Proxmox notes: this Controller's ID (`<data-dir>/controller-id`) and the
machine's. Every operation on a machine checks it first, so a lab
Controller and a production one sharing a host never touch each other's
machines, and neither touches anything else. On the host, the
hypervisor itself enforces the same boundary: a polkit policy for
libvirt, an API token whose rights cover one pool for Proxmox (below).

VMware and Hyper-V are planned. Terraform (and OpenTofu,
Terragrunt) drive it with the Janus provider: [terraform.md](terraform.md).

## What a libvirt host needs

- libvirt with QEMU/KVM and OVMF. Tested with Debian 13: libvirt 11.3,
  QEMU 10.0.
- A storage pool of type `dir`, for the base images and the machines'
  disks: a dedicated one, `janus` below.
- The libvirt networks the machines may be attached to.
- An SSH server the Controller reaches. The Controller logs in as a
  dedicated user and opens libvirt's Unix socket over SSH, in Go: no
  libvirt TCP/TLS listener, nothing to install on the host.
- On the network side:
  - the machines reach the Controller's registration port (`:8443`);
  - the Controller reaches them on `:9505`.

## Preparing a libvirt host

The Controller writes the host's preparation with the hypervisor's own
values - its account, pool, networks and name prefix:
- **Show host preparation**, in the **Add hypervisor** form, once its
  required fields are filled in;
- on the card of a hypervisor not trusted yet - the Controller's key
  included;
- `POST /api/hypervisors/preparation` ([The API](#the-api)).

Run it as root on the host: `sh janus-<name>-host.sh`, or paste it
into `sudo sh`. Every step can also run alone, and running it again is
harmless - it's also how a host follows a change to those values.

The Controller's SSH key is made when the hypervisor is added: a
preparation shown before has no key. The hypervisor's card then gives
the one command that authorizes it.

Its steps, here for the hypervisor `kvm01`, account `janus-ctl`, pool
`janus`, networks `lan` and `dmz`, and the default prefix `janus-`:

### 1. The Controller's account

A dedicated account with no password and no shell:
- the `libvirt` group lets it connect to libvirt's socket (Debian's
  polkit rule `60-libvirt.rules`);
- `janus-controllers` is the group of every Janus Controller's account
  on the host (step 5).

```sh
getent group libvirt >/dev/null || { echo "There's no libvirt group: is libvirt installed?" >&2; exit 1; }
getent group janus-controllers >/dev/null || groupadd --system janus-controllers
id -u janus-ctl >/dev/null 2>&1 || useradd --create-home --shell /usr/sbin/nologin janus-ctl
usermod --append --groups libvirt,janus-controllers janus-ctl
passwd --lock janus-ctl >/dev/null
install -d -m 700 -o janus-ctl -g "$(id -gn janus-ctl)" ~janus-ctl/.ssh
[ -e ~janus-ctl/.ssh/authorized_keys ] || install -m 600 -o janus-ctl -g "$(id -gn janus-ctl)" /dev/null ~janus-ctl/.ssh/authorized_keys
# The Controller's key for this hypervisor goes in ~janus-ctl/.ssh/authorized_keys:
# it's made when the hypervisor is added - its card then gives the command.
```

### 2. sshd: nothing but the socket

The account gets no shell and no TTY, and nothing listens on its behalf.

```sh
grep -qi '^Include /etc/ssh/sshd_config.d/' /etc/ssh/sshd_config || echo "warning: /etc/ssh/sshd_config doesn't include sshd_config.d" >&2
# Its earlier name, from a docs/hypervisors.md that named no account.
if grep -qx 'Match User janus-ctl' /etc/ssh/sshd_config.d/50-janus-controller.conf 2>/dev/null; then rm /etc/ssh/sshd_config.d/50-janus-controller.conf; fi
cat > /etc/ssh/sshd_config.d/50-janus-ctl.conf <<'JANUS'
# Janus Controller, hypervisor kvm01: its account only opens libvirt's socket.
Match User janus-ctl
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
sshd -T -C user=janus-ctl,host=localhost,addr=127.0.0.1 | grep -qx 'allowstreamlocalforwarding local' ||
    echo "warning: another sshd setting wins over 50-janus-ctl.conf for janus-ctl: check sshd -T -C user=janus-ctl" >&2
```

`AllowTcpForwarding` has to stay `local`. With `no`, OpenSSH also
refuses to open a Unix socket: libvirt's channel fails with "connect
failed". For the same reason, `PermitOpen` must stay unset.

Since that also allows forwarding TCP, step 3 makes sure no TCP gets
anywhere. The last check catches another sshd setting winning over this
file - sshd keeps the first value it reads.

### 3. No network traffic from that account

The account only ever needs libvirt's Unix socket. An nftables table,
loaded at boot by a oneshot unit, rejects any IP traffic it would send,
so a stolen key can't be used to reach other machines through the host.
The Controller's own SSH connection isn't affected: its socket belongs
to sshd.

```sh
mkdir -p /etc/nftables.d
cat > /etc/nftables.d/janus-ctl.nft <<'JANUS'
table inet janus_ctl
delete table inet janus_ctl
table inet janus_ctl {
	chain output {
		type filter hook output priority filter; policy accept;
		meta skuid "janus-ctl" counter reject
	}
}
JANUS
cat > /etc/systemd/system/janus-ctl-egress.service <<'JANUS'
[Unit]
Description=No IP traffic from the Janus Controller account janus-ctl
Before=ssh.service
After=nss-user-lookup.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/nftables.d/janus-ctl.nft
ExecStop=/usr/sbin/nft delete table inet janus_ctl

[Install]
WantedBy=multi-user.target
JANUS
systemctl daemon-reload
systemctl enable --quiet janus-ctl-egress
systemctl restart janus-ctl-egress
```

### 4. The storage pool, and the networks

A pool of type `dir`, for the base images and the machines' disks. The
networks must already be in libvirt: they're only checked.

```sh
install -d -m 711 /var/lib/libvirt/janus
virsh -q pool-info janus >/dev/null 2>&1 || virsh -q pool-define-as janus dir --target /var/lib/libvirt/janus
virsh -q pool-info janus | grep -q '^State: *running' || virsh -q pool-start janus
virsh -q pool-autostart janus
for net in lan dmz; do
    virsh -q net-info "$net" >/dev/null 2>&1 || echo "warning: libvirt has no network $net" >&2
done
```

### 5. polkit: libvirt enforces the boundary

Steps 1 to 4 are enough to work. But a member of the `libvirt` group can
do anything libvirt can, which is effectively root on the host.

With libvirt's polkit access driver, every API call is checked:
- the Controller's account only sees and acts on domains named with its
  prefix, its pool and the networks it may use;
- root and the rest of the `libvirt` group keep every right they had;
- another Controller's account (`janus-controllers`) is left to its own
  rule.

Turning the driver on restarts libvirtd once; the running virtual
machines aren't affected.

```sh
# Its earlier name, from a docs/hypervisors.md that named no account.
if grep -q 'JANUS_USER = "janus-ctl";' /etc/polkit-1/rules.d/50-janus-controller.rules 2>/dev/null; then rm /etc/polkit-1/rules.d/50-janus-controller.rules; fi
cat > /etc/polkit-1/rules.d/50-janus-ctl.rules <<'JANUS'
// Janus Controller, hypervisor kvm01 (docs/hypervisors.md): what its
// account may do through libvirt once libvirt checks every API call with
// polkit - its own virtual machines, its pool, its networks, and reading
// the host. In a function of its own: every rules file shares one scope.
(function () {
    var JANUS_USER = "janus-ctl";
    var JANUS_PREFIX = "janus-";
    var JANUS_POOL = "janus";
    var JANUS_NETWORKS = ["lan", "dmz"];
    // Every Janus Controller's account: each one has its own rule.
    var JANUS_GROUP = "janus-controllers";

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
        if (allowed === undefined || allowed.indexOf(parts[1]) < 0) {
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
```

The rule lives in a function of its own: polkit runs every rules file
in one JavaScript scope, so two Controllers' top-level variables would
overwrite each other.

With modular daemons instead of `libvirtd` (not tested here), the
setting goes in each driver daemon's own configuration: `virtqemud.conf`,
`virtstoraged.conf` and `virtnetworkd.conf`. The script says so instead
of guessing.

How to read a refusal:
- libvirtd's journal says `access denied: ...`.
- The rule logs each one with `polkit.log`. Whether polkitd shows those
  lines depends on its log level: it didn't at Debian's `notice`.

### 6. Check

The account sees its own machines only, and the host key's fingerprint
is the one to compare with what the Controller reads when it's trusted.

```sh
echo "janus-ctl sees these domains:"
runuser -u janus-ctl -- virsh -c qemu:///system list --all --name
echo "This host's SSH key:"
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

Other accounts keep their view: `virsh list --all` as root still lists
every machine, and `runuser -u janus-ctl -- virsh -c qemu:///system
dominfo <another VM>` fails to get the domain.

### Several Controllers on one host

Each gets its own account, prefix and pool - its own hypervisor
settings, its own preparation. Every account is in `janus-controllers`,
so each account's polkit rule leaves the others to theirs.

Up to v2026.10.03, these files didn't carry the account's name
(`50-janus-controller.conf`, `50-janus-controller.rules`), and the rule
granted any other `libvirt` group member everything - a second
Controller's account included. Run the preparation again: it replaces
those files with the account's own.

### Limits worth knowing

- **Disk paths aren't checked.** polkit restricts libvirt's objects, not
  the files a domain definition points at. A stolen key could still
  define a `janus-` domain using another VM's disk file.
- **The key is a secret.** It's kept in the Controller's data directory
  (`hypervisors/<id>/ssh.key`, 0600), like the nodes' service
  credentials: the data directory is what to protect.
- **The host key is pinned.** The Controller only logs in to the host
  whose SSH key the operator confirmed, never to whatever answers.
  Moving the hypervisor to another host means confirming its key again.

## Preparing a Proxmox VE node

The Controller talks to Proxmox VE's API with an API token. The token's
rights cover:
- one **resource pool**: its machines go in it, and the token sees no
  other virtual machine at all - not even in a list;
- the **storage** for the machines' disks, and a **directory storage**
  of its own for the Janus images and the machines' NoCloud volumes (it
  may delete its files there);
- the **networks** it may use - Proxmox refuses the token any other:
  - a bridge, untagged: `vmbr0`;
  - a VLAN on one: `vmbr0.20`;
  - a range of VLANs, at most 256: `vmbr0.100-199`;
  - any VLAN on it: `vmbr0.*`.

  With a range or `*`, each interface of a node picks its VLAN: the
  **Create node** and **Edit** forms show the bridge with a VLAN tag
  field, as Proxmox's own forms do - optional when the bridge untagged
  is allowed too (`vmbr0, vmbr0.*`: one choice, `vmbr0 · VLAN tag`). The
  API and Terraform name the result (`vmbr0`, `vmbr0.150`).

  A VLAN-aware `vmbr0` carrying every VLAN, each VM choosing its tag, is
  `vmbr0.*` - with `vmbr0` for untagged interfaces;
- reading the node's state.

Like for libvirt, the Controller writes the preparation with the
hypervisor's values: **Add hypervisor**, kind **Proxmox VE**, then
**Show host preparation** once the required fields are filled in. Run
it as root on the node. Step 4 shows the token's secret, once: paste it
in the form as the token secret. The Controller keeps it in its data
directory (`hypervisors/<id>/token`, 0600) and never shows it again.

Its steps, here for the hypervisor `pve01`: node `pve01`, token
`janus-ctl@pve!controller`, pool `janus`, disks on `local-lvm`, images
on `janus-images`, networks `vmbr0.10` and `vmbr0.100-109` (VLAN 10,
and VLANs 100 to 109):

### 1. The pool, and the image storage

```sh
pvesh get /pools/janus >/dev/null 2>&1 || pvesh create /pools --poolid janus --comment "Janus Controller: its machines"
if ! pvesh get /storage/janus-images >/dev/null 2>&1; then
    mkdir -p /var/lib/janus-images
    pvesm add dir janus-images --path /var/lib/janus-images --content import,iso --nodes pve01
fi
pvesh get /storage/local-lvm >/dev/null 2>&1 || echo "warning: there's no storage local-lvm for the machines' disks" >&2
```

### 2. Roles: what the Controller may do

One role per kind of object, each with only what the Controller uses -
the least Proxmox VE 9 accepted, every privilege added after Proxmox
refused without it:

```sh
role() { pveum role add "$1" --privs "$2" 2>/dev/null || pveum role modify "$1" --privs "$2"; }
role JanusVM "VM.Allocate,VM.Audit,VM.Config.CDROM,VM.Config.CPU,VM.Config.Disk,VM.Config.HWType,VM.Config.Memory,VM.Config.Network,VM.Config.Options,VM.Console,VM.PowerMgmt"
role JanusDisks "Datastore.AllocateSpace,Datastore.Audit"
role JanusImages "Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit"
role JanusNetwork "SDN.Use"
role JanusNode "Sys.Audit"
```

- `VM.Config.Options` covers the machine's notes, where its ownership
  tag is. Proxmox checks a machine's tags against the machine's own
  rights, not its pool's, so the Controller sets the `janus` tag just
  after creating it.
- `Datastore.Allocate` on the image storage is what deleting a file
  there takes. It also lets the token change that storage's settings:
  it's the Controller's own.

### 3. The Controller's user and its rights

```sh
pvesh get /access/users/janus-ctl@pve >/dev/null 2>&1 || pveum user add janus-ctl@pve --comment "Janus Controller"
pveum acl modify /pool/janus --users janus-ctl@pve --roles JanusVM
pveum acl modify /storage/local-lvm --users janus-ctl@pve --roles JanusDisks
pveum acl modify /storage/janus-images --users janus-ctl@pve --roles JanusImages
pveum acl modify /sdn/zones/localnetwork/vmbr0/10 --users janus-ctl@pve --roles JanusNetwork
for vlan in $(seq 100 109); do pveum acl modify /sdn/zones/localnetwork/vmbr0/$vlan --users janus-ctl@pve --roles JanusNetwork; done
pveum acl modify /nodes/pve01 --users janus-ctl@pve --roles JanusNode
```

An untagged bridge gets `--propagate 0`: the bridge itself, not every
VLAN on it. A range gives each of its VLANs its own right. `vmbr0.*` is
the bridge's right, propagated: every VLAN on it - and, for Proxmox,
its untagged traffic too; the Controller itself still only puts an
interface on a VLAN then.

### 4. The API token

```sh
if pvesh get /access/users/janus-ctl@pve/token/controller >/dev/null 2>&1; then
    echo 'The token janus-ctl@pve!controller exists: its secret was shown when it was made. For a new one: pveum user token remove janus-ctl@pve controller, then this step again.'
else
    pveum user token add janus-ctl@pve controller --privsep 0 --comment "Janus Controller"
fi
```

`--privsep 0`: the token has its user's rights, the user nothing else.

### 5. Check

```sh
pveum user permissions janus-ctl@pve
f=/etc/pve/local/pveproxy-ssl.pem
[ -e "$f" ] || f=/etc/pve/local/pve-ssl.pem
openssl x509 -in "$f" -noout -fingerprint -sha256
```

### Trusting the API

The Controller only talks to the API whose certificate the operator
vouched for, never to whatever answers:
- **its fingerprint**: once the hypervisor is added, its card reads the
  certificate the API presents. Compare it with the node's own - step 5
  prints it - and trust it. Another certificate later (a renewal, a
  move) has to be confirmed again;
- **or its CA**: paste the CA certificate that signs the API's in the
  form. A certificate renewed by that CA stays trusted, and the address
  must be one it names.

### What the token can't do, and limits

- It can't see, change or stop any virtual machine outside its pool.
  The Controller reads one outside it as gone.
- It can't put a machine on a VLAN or bridge it wasn't given.
- **VM IDs**: by default, the cluster's next free one. **VM IDs** in the
  form (`9000-9099`) keeps the Controller's machines in a range of their
  own.
- **The serial console** has one reader at a time, like on libvirt:
  while the Controller watches a node register, Proxmox's own serial
  console waits.
- **SDN VNets** aren't supported yet: networks are bridges of the
  `localnetwork` zone, with or without a VLAN.

## Adding the hypervisor

In the Controller: **Hypervisors**, then **Add hypervisor**, and its
kind. A Proxmox VE node takes its API URL, the node, the token (ID and
secret), the pool, the two storages, the networks, and optionally a
VM ID range and the API's CA; its card then has one step left, trusting
the API's certificate ([Trusting the API](#trusting-the-api)). A
libvirt host takes:
- the SSH host;
- the user;
- the pool;
- the networks its machines may use;
- optionally, the name prefix (`janus-` by default);
- optionally, the address its machines register at. By default it's the
  Controller's own guess: `-advertise-address` and `-register-addr`.

Once the required fields are filled in, **Show host preparation** gives
what to run on the host with those values
([Preparing a libvirt host](#preparing-a-libvirt-host)). Changing the
account, pool, networks or prefix later means running it again: the
edit form says so.

Then the card walks you through the remaining steps:

1. **Let the Controller in**: the command to run as root on the host
   adds the Controller's key to the account's `authorized_keys`. A host
   not prepared yet gets the whole preparation, key included, from
   **Show host preparation** there.
2. **Confirm the host**: **Read the host key** shows the fingerprint the
   host presents. Compare it with the host's own,
   `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`, then click **It
   matches - trust this host**. The trust request carries the fingerprint
   you confirmed, and the Controller refuses it if the host presents
   another key by then.

The card then shows the host:
- hostname, QEMU and libvirt versions;
- CPU (and its use), memory;
- the pool's space and the networks' state;
- the machines.

## Creating a node

**Create node** asks for:
- the hypervisor, the name (the node's hostname), vCPUs and memory;
- the network interfaces:
  - each on one of the allowed networks;
  - named on the node (`mgmt`, `front`...) and matched by a MAC address
    the Controller chooses;
  - static, DHCP, or up without an address;
- DNS and NTP servers;
- the Janus version (the newest by default);
- the image factory's extensions.

Prefer static addresses: the kernel's DHCP lease is taken once at boot
and never renewed.

What happens then, followed on the machine's card:

1. **The image.**
   - Without extensions: the release's `janus-kvm.qcow2`, checked
     against the SHA-256 GitHub computed for it.
   - With extensions: the image factory's build of that schematic,
     waited for if it's being built.
   - Or any image given by URL and SHA-256 (a mirror, a development
     build).

   The Controller downloads it into its data directory and checks it,
   so nothing unchecked reaches the host. It then uploads it once per
   schematic and version as a base image, `janus-base-...` in the pool.
2. **The virtual machine.**
   - **Disk:** a full copy of the base image.
   - **NoCloud CD-ROM** (`cidata`), carrying:
     - the Controller's address and CA;
     - the node's network: hostname, interfaces;
     - a one-time registration token.
   - **Firmware:** UEFI without Secure Boot. The VM images' boot entries
     aren't signed; the release bundles are.
   - **Console:** a serial console on a pty. Never a log file on the
     host: a node's first boot prints its admin credential there.
   - **Autostart.**
3. **The node registers.** It presents its token and is admitted at
   once: no approval, and the node is linked to its machine.
   - An image released before registration tokens (before the version
     that introduced them) registers like any node and waits in
     **Waiting for approval**, marked as a machine this Controller
     created. Approving it links it to the machine.

**While it waits, the card says what's wrong.** The Controller reads
the node's console, and a node that can't register says why there: the
card shows it, with the likely cause:
- no route to the Controller - often a DHCP server that hands out no
  gateway: give an interface a static address and a gateway;
- no DNS for the Controller's name - give the node a DNS server, or set
  the hypervisor's Controller address to an IP address;
- nothing listening, a certificate that doesn't name the address, no
  answer at all (a firewall).

The node keeps trying, waiting a little longer each time (up to two
minutes): once the network is fixed, it registers without a reboot. An
image up to v2026.10.03-2 tries once a boot only: the card says so -
reset it once the network is fixed.

A machine that hasn't registered within 15 minutes is marked failed.
The virtual machine is left running so its console can say why, and its
token stays valid for a day: a node that comes up later is still
admitted. **Retry** starts a failed creation over: it removes whatever
the failed attempt left, then creates the machine again with a fresh
token.

Creating a machine runs in the background. A restarted Controller
reports what it was in the middle of as interrupted (retry or destroy)
and keeps waiting for nodes that hadn't registered yet.

## Running them

On the machine's card, and on its node's card:

- **Reset**, **Force off**, **Start**: the hypervisor's own actions, for
  a node that doesn't answer. A clean reboot or shutdown is on the
  node's own page (Power), through its API.
- **Console**: the serial console, read-only (a node has no shell),
  from the moment it's opened. Private keys are hidden: the Controller
  never holds a node's admin credential, even passing through. Every
  page showing it - and the Controller, while a node registers - shares
  one console. A console already open on the host (`virsh console`)
  keeps it.
- **Destroy** (type the name):
  1. the node is shut down cleanly first, so HAProxy stops and VRRP or
     BGP peers see it go;
  2. then the virtual machine, its NVRAM and its two volumes are
     deleted;
  3. then the Controller forgets the node.

  A machine's node can't be removed on its own: its virtual machine
  would keep running, owned by nobody.

Base images stay in the pool for the next machine. The Controller never
removes them: `virsh vol-delete --pool janus janus-base-...` once no
machine needs that version.

If a hypervisor is gone for good, `DELETE /api/machines/{id}?forget=true`
drops the Controller's records of a machine without touching anything
else.

## Changing a node

What a machine is made of has three homes, each with one way to change
it:

| What | Where it's changed |
|---|---|
| Hardware: vCPUs, memory, network interfaces (added, removed, moved to another network) | **Edit** on the machine's card, the API, or Terraform. The node shuts down cleanly, its virtual machine is reconfigured and started again. |
| The node's own settings: interfaces' addresses, DNS, NTP, VLANs, its version and extensions, HAProxy... | Its own page (**Open**): System › Network, System › Update... - or the API, or Terraform. |
| Its name, hypervisor, image | Nowhere: that's a new machine. |

The page doesn't repeat what the node's page does. An interface added or
moved gets its addresses in **Edit**, since it's of no use without them.

Two constraints from the node:
- **The interface the Controller reaches the node through** can't be
  removed or moved: the node would be lost.
- **DHCP** only works on the interface the node booted with, so an
  added one gets static addresses or none.

The order is the node's too. A removed interface leaves the node's
configuration first, then it's unplugged: a node whose configuration
names an interface it hasn't got keeps none of it. An added one is
plugged in first, then configured.

**The record follows the node.** The machine's record is read again
from its node and its hypervisor:
- after every change the Controller makes or relays;
- every minute;
- when asked (`?refresh=true`).

So a change made on the node's page, with `janusctl` or on the hypervisor
shows up on the card. It's in its history ("changed outside the
Controller") and in Terraform's next plan.

The name isn't read back: a hostname changed on the node's page is shown
beside it. A network change through the API or Terraform is merged into
the node's own configuration: what only its page manages - VLANs, MTUs,
search domains, route metrics - stays.

### Managed as code, and locked

A node Terraform manages is marked so (`managed_by`) and, by default,
**locked** (`lock_ui`, [terraform.md](terraform.md)).

- **What its pages refuse** (both the Controller's and the node's own):
  what Terraform manages - its hardware, network, version and
  extensions, and destroying it.
- **What they still do**: show it, its console and logs, power and
  restarts, and everything Terraform doesn't manage yet (HAProxy's
  configuration, certificates, the firewall...).
- **The lock is only on the pages**: Terraform's API token isn't held by
  it.

**Release**, on the card, lifts the lock for a change by hand. Terraform
then shows that change in its next plan and undoes it on `apply`, and
locks the node again. The lock is a guardrail in the Controller, not a
barrier: the node's own admin credential (`janusctl`) goes around it.
The record shows what was done that way.

`PATCH /api/machines/{id}` changes a ready node in place, in the
background: phase `updating`, then back to `ready`. If a step failed,
the error is on the machine, and its record is read back from the node,
so it says what was really done.

## The API

Behind the Controller's login, like the rest of its API, or an API token
(`Authorization: Bearer`, the **API tokens** tab) for a program. Reads
need a reader, power and console an operator, the rest an admin. The shape is
the one a Terraform provider needs, for the planned one:
- `POST` answers `202` with the resource, whose `phase` is then polled;
- `GET` returns the spec as created, with MAC addresses filled in, plus
  the observed state;
- `DELETE` answers `202`, then the resource is `404`;
- errors are `{"error": "..."}`.

| Method and path | |
|---|---|
| `GET`, `POST /api/hypervisors` | list, add (`{name, kind: "libvirt", controller_address, libvirt: {host, user, socket, pool, networks, name_prefix}}`, or `{name, kind: "proxmox", controller_address, token_secret, proxmox: {url, node, token_id, pool, storage, image_storage, networks, name_prefix, vmids, ca_cert}}` - Proxmox networks: `vmbr0`, `vmbr0.20`, `vmbr0.100-199`, `vmbr0.*`; a machine's interface names one bridge or VLAN, `vmbr0.150`) - the token secret is never in an answer (`has_token_secret`) |
| `GET`, `PATCH`, `DELETE /api/hypervisors/{id}` | one; change (another host or API address must be trusted again; a Proxmox token secret is kept unless one is given); remove (refused while it has machines) |
| `POST /api/hypervisors/preparation` | what to run on the host for these settings (the add form's body, plus `id` for one already added: its key goes in): `{steps: [{title, about, script}], script, has_key}` |
| `POST /api/hypervisors/{id}/probe` | what the host presents: `{host_key, fingerprint}` (libvirt), `{fingerprint, subject, issuer}` (the API's certificate, Proxmox) |
| `POST /api/hypervisors/{id}/trust` | `{fingerprint}`: pins the host key or certificate, if the host presents that one |
| `GET /api/hypervisors/{id}/status` | the host, CPU use, and each of its machines' state |
| `GET`, `POST /api/machines` | list; create (`{name, hypervisor_id, vcpus, memory_mib, version, extensions, image: {url, sha256}, nics: [{network, name, mac, mode, addresses, gateway}], dns, ntp}`) |
| `GET`, `DELETE /api/machines/{id}` | one (`?refresh=true`: read from its node and hypervisor first): `phase` is `pending`, `preparing-image`, `creating`, `waiting-registration`, `ready`, `updating`, `failed` or `destroying`; destroy (`?forget=true`: records only) |
| `PATCH /api/machines/{id}` | change it in place (`{vcpus, memory_mib, version, extensions, nics, dns, ntp, managed_by, locked}`, those given; `nics` is the whole new set, by MAC). A locked machine refuses its pages (`423`) except `{"locked": false}` |
| `POST /api/machines/{id}/power` | `{action: "start" \| "force-off" \| "reset"}` |
| `POST /api/machines/{id}/retry` | a failed creation, again |
| `GET /api/machines/{id}/console` | Server-Sent Events, one JSON string per chunk; `failure` when it closes |
| `GET /api/catalog` | the newest release and the image factory's extensions |
| `GET`, `POST /api/tokens`, `DELETE /api/tokens/{id}` | API tokens (`{name, expires_in_days, role}`; the token is in the answer, once) - each account's own, from its session only, never a token; an admin sees everyone's |

## Testing

`make controller-libvirt-test` (`hack/controller-libvirt-test.sh`, run
by `image-build.yml`) runs the whole cycle against a real libvirt in a
privileged container (`hack/libvirt-host`), with the Controller inside it:
- adding the hypervisor;
- creating a node from the image under test, admitted on its token;
- the console of its first boot, with no key getting through;
- a reset;
- a node that can't reach the Controller: the machine says why, while a
  page reads the same console, and the node registers on its own once
  it can;
- refusing two forged records pointing at domains the Controller didn't
  create;
- destroying it.

The polkit policy can't run in a container: it's checked on a real host
as shown in step 5.

Proxmox VE can't run in CI either. Its driver has unit tests, and a test
against a real node, skipped unless its environment is set - a token
prepared as above, and a VM ID outside its pool, only ever read, to
check the token can't see it:

```sh
JANUS_PVE_URL=https://pve01:8006 JANUS_PVE_NODE=pve01 \
JANUS_PVE_TOKEN_ID='janus-ctl@pve!controller' JANUS_PVE_TOKEN_SECRET=... \
JANUS_PVE_FINGERPRINT=AA:BB:... JANUS_PVE_POOL=janus JANUS_PVE_STORAGE=local-lvm \
JANUS_PVE_IMAGE_STORAGE=janus-images JANUS_PVE_NETWORK=vmbr0.10 \
JANUS_PVE_VMIDS=9100-9189 JANUS_PVE_IMAGE=janus.qcow2 JANUS_PVE_FOREIGN_VMID=100 \
go test -run TestLive -v ./dashboard/backend/internal/hypervisor/proxmox/
```

It uploads the image, creates a machine, reads its console through a
reset, changes its hardware (an interface added, then removed), checks
another Controller's record and a machine outside the pool are refused,
and destroys it.

On a host without `/dev/net/tun`, a machine can't be plugged into a
network, so the test can't boot it. The CI runners are such hosts: an
unprivileged LXC that only passes `/dev/kvm` through. There, the test
checks instead that the failed start leaves nothing behind and that
destroying it is clean, and it says what it skipped in a CI warning.
