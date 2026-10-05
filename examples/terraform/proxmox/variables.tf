variable "hypervisor_name" {
  description = "The hypervisor's name on the Controller."
  type        = string
  default     = "pve01"
}

variable "pve_url" {
  description = "The Proxmox VE API, https://host:8006."
  type        = string
}

variable "pve_node" {
  description = "The Proxmox VE node the machines run on."
  type        = string
}

variable "pve_token_secret" {
  description = "The API token's secret, shown once when it was created."
  type        = string
  sensitive   = true
}

variable "certificate_fingerprint" {
  description = "The API's certificate, as `openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pveproxy-ssl.pem` prints it on the node."
  type        = string
}

variable "storage" {
  description = "The storage for the machines' disks."
  type        = string
  default     = "local-lvm"
}

variable "image_storage" {
  description = "The storage for the images and NoCloud volumes (content: iso, import)."
  type        = string
  default     = "janus-images"
}

variable "network" {
  description = "The bridge - or VLAN on one, vmbr0.20 - the nodes are attached to."
  type        = string
  default     = "vmbr0"
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
