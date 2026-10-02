# Hypervisors: nodes the Controller creates itself

The Controller can be given hypervisors - libvirt/KVM hosts today - and
create, power and destroy Janus nodes on them itself. A node it creates
is admitted as soon as it registers, without the approval every other
registration waits for: creating it was the approval.

The Controller only ever acts on the virtual machines it created. Every
one carries an ownership tag in its libvirt `<metadata>`: this
Controller's ID (`<data-dir>/controller-id`) and the machine's. Every
operation on a machine checks it first, so a lab Controller and a
production one sharing a host never touch each other's machines, and
neither touches anything else. On the host, a polkit policy makes
libvirt itself enforce the same boundary (below).

Proxmox, VMware and Hyper-V are planned; a Terraform provider driving
this API too.

## What the host needs

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

Everything here runs as root on the host.

### 1. The Controller's account

```sh
useradd -m -s /usr/sbin/nologin -G libvirt janus-ctl
passwd -l janus-ctl
install -d -m 700 -o janus-ctl -g janus-ctl ~janus-ctl/.ssh
install -m 600 -o janus-ctl -g janus-ctl /dev/null ~janus-ctl/.ssh/authorized_keys
```

The `libvirt` group lets the account connect to libvirt's socket
(Debian's polkit rule `60-libvirt.rules`).

The Controller generates its own SSH key per hypervisor. Its public half,
shown once the hypervisor is added, goes in that `authorized_keys`.

### 2. sshd: nothing but the socket

`/etc/ssh/sshd_config.d/50-janus-controller.conf`:

```
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
```

The account gets no shell and no TTY, and nothing listens on its behalf.

`AllowTcpForwarding` has to stay `local`. With `no`, OpenSSH also
refuses to open a Unix socket: libvirt's channel fails with "connect
failed". For the same reason, `PermitOpen` must stay unset.

Since that also allows forwarding TCP, step 3 makes sure no TCP gets
anywhere.

### 3. No network traffic from that account

The account only ever needs libvirt's Unix socket. An nftables table
rejects any IP traffic it would send, so a stolen key can't be used to
reach other machines through the host.

`/etc/nftables.d/janus-ctl.nft`:

```
table inet janus_ctl
delete table inet janus_ctl
table inet janus_ctl {
	chain output {
		type filter hook output priority filter; policy accept;
		meta skuid "janus-ctl" counter reject
	}
}
```

Load it at boot with a oneshot unit,
`/etc/systemd/system/janus-ctl-egress.service`:

```
[Unit]
Description=No IP traffic from the Janus Controller account (janus-ctl)
Before=ssh.service
After=nss-user-lookup.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/nftables.d/janus-ctl.nft
ExecStop=/usr/sbin/nft delete table inet janus_ctl

[Install]
WantedBy=multi-user.target
```

Enable it with `systemctl enable --now janus-ctl-egress`. The Controller's
own SSH connection isn't affected: its socket belongs to sshd.

### 4. The storage pool

```sh
install -d -m 711 /var/lib/libvirt/janus
virsh pool-define-as janus dir --target /var/lib/libvirt/janus
virsh pool-start janus
virsh pool-autostart janus
```

### 5. polkit: libvirt enforces the boundary

Steps 1 to 4 are enough to work. But a member of the `libvirt` group can
do anything libvirt can, which is effectively root on the host.

With libvirt's polkit access driver, every API call is checked:
- the Controller's account only sees and acts on domains named with its
  prefix, its pool and the networks it may use;
- everyone else - root, the rest of the `libvirt` group - keeps every
  right they had.

`/etc/polkit-1/rules.d/50-janus-controller.rules`, with your prefix,
pool and networks at the top:

```js
var JANUS_USER = "janus-ctl";
var JANUS_PREFIX = "janus-";
var JANUS_POOL = "janus";
var JANUS_NETWORKS = ["lan", "dmz"];

var JANUS_ALLOWED = {
    "connect": ["getattr", "read", "search-domains", "search-networks", "search-storage-pools"],
    "domain": ["getattr", "read", "write", "save", "delete", "start", "stop", "reset", "open-device"],
    "storage-pool": ["getattr", "read", "refresh", "search-storage-vols"],
    "storage-vol": ["getattr", "read", "create", "delete", "data-read", "data-write"],
    "network": ["getattr", "read"],
    // Starting a machine plugs its interfaces into the networks.
    "network-port": ["getattr", "read", "create", "delete"]
};

function janusDenied(action) {
    polkit.log("janus-controller: denied " + action.id + " domain=" + action.lookup("domain_name") +
        " pool=" + action.lookup("pool_name") + " network=" + action.lookup("network_name"));
    return polkit.Result.NO;
}

polkit.addRule(function(action, subject) {
    if (action.id.indexOf("org.libvirt.api.") != 0) {
        return polkit.Result.NOT_HANDLED;
    }
    if (subject.user != JANUS_USER) {
        if (subject.user == "root" || subject.isInGroup("libvirt")) {
            return polkit.Result.YES;
        }
        return polkit.Result.NOT_HANDLED;
    }
    var parts = action.id.substr("org.libvirt.api.".length).split(".");
    var allowed = JANUS_ALLOWED[parts[0]];
    if (!allowed || allowed.indexOf(parts[1]) < 0) {
        return janusDenied(action);
    }
    switch (parts[0]) {
    case "domain":
        return String(action.lookup("domain_name")).indexOf(JANUS_PREFIX) == 0 ? polkit.Result.YES : janusDenied(action);
    case "storage-pool":
    case "storage-vol":
        return action.lookup("pool_name") == JANUS_POOL ? polkit.Result.YES : janusDenied(action);
    case "network":
    case "network-port":
        return JANUS_NETWORKS.indexOf(action.lookup("network_name")) >= 0 ? polkit.Result.YES : janusDenied(action);
    }
    return polkit.Result.YES;
});
```

Then turn the access driver on and restart libvirt. The running virtual
machines aren't affected.

```sh
echo 'access_drivers = [ "polkit" ]' >> /etc/libvirt/libvirtd.conf
systemctl restart libvirtd
```

With modular daemons instead of `libvirtd` (not tested here), the setting
goes in each driver daemon's own configuration: `virtqemud.conf`,
`virtstoraged.conf` and `virtnetworkd.conf`.

Check it - root sees everything, the account only its own:

```sh
virsh list --all
sudo -u janus-ctl virsh -c qemu:///system list --all
sudo -u janus-ctl virsh -c qemu:///system dominfo <another VM>    # failed to get domain
```

How to read a refusal:
- libvirtd's journal says `access denied: ...`.
- The rule logs each one with `polkit.log`. Whether polkitd shows those
  lines depends on its log level: it didn't at Debian's `notice`.

The machines' prefix is the hypervisor's **name prefix** in the
Controller. A Controller sharing a host with another one gets its own
account and its own prefix.

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

## Adding the hypervisor

In the Controller: **Hypervisors**, then **Add hypervisor**. You give it:
- the SSH host;
- the user;
- the pool;
- the networks its machines may use;
- optionally, the name prefix (`janus-` by default);
- optionally, the address its machines register at. By default it's the
  Controller's own guess: `-advertise-address` and `-register-addr`.

Then the card walks you through the remaining steps:

1. **Let the Controller in**: add the shown line to the account's
   `authorized_keys`.
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
  never holds a node's admin credential, even passing through. A
  console already open on the host (`virsh console`) keeps it.
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

## The API

Behind the Controller's login, like the rest of its API. The shape is
the one a Terraform provider needs, for the planned one:
- `POST` answers `202` with the resource, whose `phase` is then polled;
- `GET` returns the spec as created, with MAC addresses filled in, plus
  the observed state;
- `DELETE` answers `202`, then the resource is `404`;
- errors are `{"error": "..."}`.

| Method and path | |
|---|---|
| `GET`, `POST /api/hypervisors` | list, add (`{name, kind: "libvirt", controller_address, libvirt: {host, user, socket, pool, networks, name_prefix}}`) |
| `GET`, `PATCH`, `DELETE /api/hypervisors/{id}` | one; change (another host must be trusted again); remove (refused while it has machines) |
| `POST /api/hypervisors/{id}/probe` | the host key the host presents: `{host_key, fingerprint}` |
| `POST /api/hypervisors/{id}/trust` | `{fingerprint}`: pins the host key, if the host presents that one |
| `GET /api/hypervisors/{id}/status` | the host, CPU use, and each of its machines' state |
| `GET`, `POST /api/machines` | list; create (`{name, hypervisor_id, vcpus, memory_mib, version, extensions, image: {url, sha256}, nics: [{network, name, mac, mode, addresses, gateway}], dns, ntp}`) |
| `GET`, `DELETE /api/machines/{id}` | one: `phase` is `pending`, `preparing-image`, `creating`, `waiting-registration`, `ready`, `failed` or `destroying`; destroy (`?forget=true`: records only) |
| `POST /api/machines/{id}/power` | `{action: "start" \| "force-off" \| "reset"}` |
| `POST /api/machines/{id}/retry` | a failed creation, again |
| `GET /api/machines/{id}/console` | Server-Sent Events, one JSON string per chunk; `failure` when it closes |
| `GET /api/catalog` | the newest release and the image factory's extensions |

## Testing

`make controller-libvirt-test` (`hack/controller-libvirt-test.sh`, run
by `image-build.yml`) runs the whole cycle against a real libvirt in a
privileged container (`hack/libvirt-host`), with the Controller inside it:
- adding the hypervisor;
- creating a node from the image under test, admitted on its token;
- the console of its first boot, with no key getting through;
- a reset;
- refusing two forged records pointing at domains the Controller didn't
  create;
- destroying it.

The polkit policy can't run in a container: it's checked on a real host
as shown in step 5.

On a host without `/dev/net/tun`, a machine can't be plugged into a
network, so the test can't boot it. The CI runners are such hosts: an
unprivileged LXC that only passes `/dev/kvm` through. There, the test
checks instead that the failed start leaves nothing behind and that
destroying it is clean, and it says what it skipped in a CI warning.
