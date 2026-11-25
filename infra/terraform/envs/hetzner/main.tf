# Fallback host: Hetzner CAX11 (2 vCPU ARM64, 4 GB). NOT free (~€4/month).
# Use when Oracle A1 capacity is unavailable or the free tier changes again.
# Same cloud-init module, so the node and everything on it is identical.

terraform {
  required_version = ">= 1.9"
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.70"
    }
  }
}

variable "hcloud_token" {
  type      = string
  sensitive = true
}
variable "ssh_public_key_path" { type = string }
variable "ssh_cidr" {
  type = string
  validation {
    condition     = var.ssh_cidr != "0.0.0.0/0"
    error_message = "Set ssh_cidr to your IP/32."
  }
}
variable "location" {
  type    = string
  default = "fsn1"
}
variable "k3s_version" {
  type    = string
  default = "v1.37.2+k3s1"
}

provider "hcloud" {
  token = var.hcloud_token
}

module "cloud_init" {
  source      = "../../modules/k3s-cloud-init"
  k3s_version = var.k3s_version
  # Hetzner images have no restrictive host firewall; hcloud_firewall below filters.
}

resource "hcloud_ssh_key" "this" {
  name       = "tollgate"
  public_key = file(var.ssh_public_key_path)
}

resource "hcloud_firewall" "this" {
  name = "tollgate"
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = [var.ssh_cidr]
  }
  dynamic "rule" {
    for_each = ["80", "443"]
    content {
      direction  = "in"
      protocol   = "tcp"
      port       = rule.value
      source_ips = ["0.0.0.0/0", "::/0"]
    }
  }
}

resource "hcloud_server" "node" {
  name         = "tollgate"
  server_type  = "cax11"
  image        = "ubuntu-24.04"
  location     = var.location
  ssh_keys     = [hcloud_ssh_key.this.id]
  firewall_ids = [hcloud_firewall.this.id]
  user_data    = module.cloud_init.rendered
  lifecycle {
    ignore_changes = [user_data, image]
  }
}

output "public_ip" { value = hcloud_server.node.ipv4_address }
output "ssh" { value = "ssh root@${hcloud_server.node.ipv4_address}" }
