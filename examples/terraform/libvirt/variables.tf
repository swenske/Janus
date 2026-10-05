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
