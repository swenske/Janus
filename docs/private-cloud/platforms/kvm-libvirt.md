# libvirt/KVM, end to end

Two Janus load balancers on a libvirt/KVM host, created by a Janus
Controller and configured by Terraform (or OpenTofu): the hypervisor,
the nodes, their network and their HAProxy configuration, all as code -
then a virtual IP between them.

> [!NOTE]
> Proven: `make terraform-provider-test` applies this example as written,
> on every image build: its two nodes created and admitted, serving its
> `haproxy.cfg`, on a real Controller and a real libvirt host.

## What you need

- **A libvirt host**: libvirt with QEMU/KVM and OVMF - tested with
  Debian 13 (libvirt 11.3, QEMU 10.0) - a `dir` storage pool for Janus,
  the libvirt network the nodes go on, and SSH.
- **A host for the Controller**: Docker with Compose. It reaches the
  libvirt host over SSH and the nodes on port 9505; the nodes reach it
  on 8443.
- **OpenTofu or Terraform**, and `janusctl` for the virtual IP
  ([janusctl](../../janusctl.md#installing-janusctl)).
- **Addresses**: two for the nodes, one for the virtual IP, on the
  libvirt network - `192.0.2.21`, `192.0.2.22` and `192.0.2.100` below.

## 1. The Controller

On its host, the Compose setup - the Controller and its updater - from
[`examples/compose/compose.yaml`](../../../examples/compose/compose.yaml):

```sh
sudo mkdir -p /opt/janus-controller && cd /opt/janus-controller
sudo curl -fsSLO https://raw.githubusercontent.com/swenske/Janus/main/examples/compose/compose.yaml
sudo docker compose up -d
```

(Another directory than `/opt/janus-controller`: change it in the file
too - [the Compose setup](../../../dashboard/README.md#the-compose-setup).)

Open `https://<controller>:8080/`, create the first account - an admin,
with a second factor - then **Secure your fleet**: its recovery kit and
passphrase go to your password manager
([securing the fleet](../../../dashboard/README.md#securing-the-fleet)).
From the **Provision new nodes** panel, keep the Controller's
certificate as `controller-ca.crt`.

## 2. The libvirt host

The Controller writes the host's preparation with your values - a
dedicated `janus-ctl` account that can only open libvirt's socket, the
`janus` pool, the networks, a polkit policy that limits it to its own
machines. In the Controller: **Hypervisors › Add hypervisor**, fill in
the host, `janus-ctl`, `janus`, your network, then **Show host
preparation** - and cancel: Terraform adds the hypervisor itself. Run
the preparation as root on the host. What each step does:
[preparing a libvirt host](../../hypervisors.md#preparing-a-libvirt-host).

Note the host's SSH key fingerprint, which the Controller will trust:

```sh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

## 3. Terraform's access

An **API token** of an admin's account (the **API tokens** tab), and
the provider binary from the release that matches your Controller
([setting it up](../../terraform.md#setting-it-up)), then:

```sh
export JANUS_ENDPOINT=https://controller.example.net:8080
export JANUS_CA_CERT=$PWD/controller-ca.crt
export JANUS_TOKEN=janus_...                 # from your secret store
```

## 4. The configuration

[`examples/terraform/libvirt`](../../../examples/terraform/libvirt/):

```hcl title="examples/terraform/libvirt/main.tf"
# Two Janus load balancers on a libvirt/KVM host, created by a Janus
# Controller and serving ../../haproxy/web.cfg - the end-to-end example of
# docs/private-cloud/platforms/kvm-libvirt.md. make terraform-provider-test
# applies it as written, against a real Controller and libvirt host.
#
# The provider reads the Controller from the environment:
#   JANUS_ENDPOINT  https://controller.example.net:8080
#   JANUS_CA_CERT   the Controller's certificate (its Provision panel)
#   JANUS_TOKEN     an API token (an admin's: it creates machines)

terraform {
  required_providers {
    janus = { source = "swenske/janus" }
  }
}

provider "janus" {}

# The host, prepared first (docs/hypervisors.md). Its account authorizes
# the Controller's key - authorized_key, below - before nodes are made:
# apply this resource alone first (-target=janus_hypervisor.kvm).
resource "janus_hypervisor" "kvm" {
  name                 = var.hypervisor_name
  host                 = var.hypervisor_host
  user                 = "janus-ctl"
  pool                 = "janus"
  networks             = [var.network]
  host_key_fingerprint = var.host_key_fingerprint
}

resource "janus_node" "lb" {
  for_each      = var.nodes
  name          = each.key
  hypervisor_id = janus_hypervisor.kvm.id
  vcpus         = 2
  memory_mib    = var.memory_mib
  version       = var.janus_version
  extensions    = var.extensions
  image         = var.image

  interfaces = [{
    network   = var.network
    name      = "front"
    mode      = "static"
    addresses = [each.value]
    gateway   = var.gateway
  }]
  dns    = var.dns
  labels = { role = "edge" }
}

# Each node's haproxy.cfg: HAProxy checks it first - a configuration it
# refuses fails the apply and changes nothing.
resource "janus_haproxy_config" "lb" {
  for_each = janus_node.lb
  node     = each.value.node_id
  config   = file("${path.module}/../../haproxy/web.cfg")
}

output "authorized_key" {
  description = "The line the host's janus-ctl account needs in ~/.ssh/authorized_keys."
  value       = janus_hypervisor.kvm.authorized_key
}

output "nodes" {
  description = "Each node's address on the Controller."
  value       = { for name, node in janus_node.lb : name => node.node_address }
}
```

```hcl title="examples/terraform/libvirt/variables.tf"
variable "hypervisor_name" {
  description = "The hypervisor's name on the Controller."
  type        = string
  default     = "kvm01"
}

variable "hypervisor_host" {
  description = "The libvirt host the Controller reaches over SSH."
  type        = string
}

variable "host_key_fingerprint" {
  description = "The host's SSH key, as `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` prints it on the host: the Controller trusts that key only."
  type        = string
}

variable "network" {
  description = "The libvirt network the nodes are attached to."
  type        = string
  default     = "lan"
}

variable "gateway" {
  description = "The nodes' default gateway."
  type        = string
  default     = "192.0.2.1"
}

variable "nodes" {
  description = "Each node's name and static address (CIDR)."
  type        = map(string)
  default = {
    lb1 = "192.0.2.21/24"
    lb2 = "192.0.2.22/24"
  }
}

variable "memory_mib" {
  description = "Each node's memory: HAProxy's tables grow with maxconn (docs/haproxy-config.md)."
  type        = number
  default     = 2048
}

variable "dns" {
  description = "DNS servers (unset: DHCP's)."
  type        = list(string)
  default     = null
}

variable "janus_version" {
  description = "The release to run (unset: the newest)."
  type        = string
  default     = null
}

variable "extensions" {
  description = "Extensions, built by the image factory - [\"keepalived\"] for a virtual IP."
  type        = set(string)
  default     = null
}

variable "image" {
  description = "Another qcow2 than the release's: a mirror, a development build."
  type = object({
    url    = string
    sha256 = string
  })
  default = null
}
```

The `haproxy.cfg` both nodes get: HTTP on port 80 to two application
servers, and HAProxy's Prometheus exporter on 8405 - what differs from a
distribution's is in [your haproxy.cfg](../../haproxy-config.md):

```haproxy title="examples/haproxy/web.cfg"
# A web load balancer on a Janus node: HTTP for the application on every
# address, two application servers checked every 2 seconds, HAProxy's
# own Prometheus exporter on :8405. Applied by the Terraform examples
# (../terraform/*/main.tf); checked by the node's own HAProxy (make
# examples-test). docs/haproxy-config.md says what differs from a
# distribution's haproxy.cfg.
global
    log stdout format raw local0
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000
    # About 32000 connections and 20 MB of tables: enough for a small node.
    fd-hard-limit 65536

defaults
    mode http
    log global
    option httplog
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend web
    bind :80
    # HTTPS once the certificate is on the node (docs/haproxy-files.md):
    #   bind :443 ssl crt /etc/haproxy/files/certs/app.pem alpn h2,http/1.1
    http-request return status 200 content-type text/plain string "ok\n" if { path /healthz }
    default_backend app

backend app
    balance roundrobin
    option httpchk GET /healthz
    default-server check inter 2s fall 3 rise 2
    server app1 10.0.10.11:8080
    server app2 10.0.10.12:8080

frontend prometheus
    bind :8405
    http-request use-service prometheus-exporter if { path /metrics }
    no log
```

## 5. Apply it

The host must authorize the Controller's key before a node can be made
on it: the hypervisor first, its key, then the rest.

```sh
cat > terraform.tfvars <<'EOF'
hypervisor_host      = "kvm01.example.net"
host_key_fingerprint = "SHA256:..."            # step 2
EOF
tofu apply -target=janus_hypervisor.kvm
tofu output -raw authorized_key | ssh root@kvm01.example.net 'cat >> ~janus-ctl/.ssh/authorized_keys'
tofu apply
```

Each node's creation - the image downloaded and checked (once per
release), its virtual machine made with a NoCloud volume, its first
boot, its registration on a one-time token - takes a few minutes;
`apply` waits until both are admitted and answer, then applies their
HAProxy configuration. The Controller's
**Hypervisors** tab shows each machine's progress and console.

## 6. Check

```sh
curl http://192.0.2.21/healthz                 # ok
janusctl login -controller controller.example.net:8080 -controller-ca controller-ca.crt
janusctl -n lb1,lb2 haproxy show-info
tofu plan                                      # nothing to change
```

## 7. A virtual IP

The nodes need the `keepalived` extension: add it, and Terraform updates
both nodes in place - each one's own A/B update, to the image factory's
build with keepalived (built on first request: allow a few minutes):

```sh
tofu apply -var 'extensions=["keepalived"]'
```

Then the same `keepalived.conf` on both, but for the priority
([VRRP](../../vrrp.md)), with `interface front` and the virtual IP
`192.0.2.100/24`:

```sh
janusctl -n lb1 network vrrp apply keepalived-lb1.conf    # priority 150
janusctl -n lb2 network vrrp apply keepalived-lb2.conf    # priority 100
janusctl -n lb1,lb2 network vrrp status                   # lb1 MASTER, lb2 BACKUP
curl http://192.0.2.100/healthz
```

Stop HAProxy on lb1 (`janusctl -n lb1 system service stop haproxy`):
lb2 takes the virtual IP within a second; start it again and lb1 takes
it back.

## Clean up

```sh
tofu destroy
```

Each node is shut down cleanly first, then its virtual machine and its
disks are deleted. The base images stay in the pool for the next nodes.

## Without Terraform

- **From the Controller**: **Hypervisors › Create node** does the same
  as `janus_node`, with a form ([creating a node](../../hypervisors.md#creating-a-node)).
- **By hand**, without a Controller's hypervisor:
  `virt-install --import --boot uefi` with `janus-kvm.qcow2`, plus a
  `cidata` volume for its first boot ([first boot](../first-boot.md)):

  ```sh
  virt-install --name lb1 --memory 2048 --vcpus 2 --import --boot uefi \
    --disk path=lb1.qcow2,bus=virtio --disk path=cidata.iso,device=cdrom \
    --network network=lan,model=virtio \
    --graphics none --console pty,target_type=serial --noautoconsole
  ```
