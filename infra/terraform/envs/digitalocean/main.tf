# DigitalOcean droplet (2 vCPU, 4 GB, amd64), paid from GitHub Student Pack
# credit ($200 / 1 year). s-2vcpu-4gb is ~$24/month, so the credit lasts
# ~8 months. Same cloud-init module as Oracle and Hetzner.
# ponytail: credit expires; destroy or move to envs/hetzner before it does,
# or the droplet starts billing the card on file.

terraform {
  required_version = ">= 1.9"
  required_providers {
    digitalocean = {
      source  = "digitalocean/digitalocean"
      version = "~> 2.0"
    }
  }
}

variable "do_token" {
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
variable "region" {
  type    = string
  default = "blr1"
}
variable "size" {
  type    = string
  default = "s-2vcpu-4gb"
}
variable "k3s_version" {
  type    = string
  default = "v1.37.2+k3s1"
}

provider "digitalocean" {
  token = var.do_token
}

module "cloud_init" {
  source      = "../../modules/k3s-cloud-init"
  k3s_version = var.k3s_version
  # DO Ubuntu images have no restrictive host firewall; the cloud firewall below filters.
}

resource "digitalocean_ssh_key" "this" {
  name       = "tollgate"
  public_key = file(var.ssh_public_key_path)
}

resource "digitalocean_droplet" "node" {
  name       = "tollgate"
  image      = "ubuntu-24-04-x64"
  size       = var.size
  region     = var.region
  ssh_keys   = [digitalocean_ssh_key.this.fingerprint]
  user_data  = module.cloud_init.rendered
  monitoring = true
  lifecycle {
    ignore_changes = [user_data, image]
  }
}

resource "digitalocean_firewall" "this" {
  name        = "tollgate"
  droplet_ids = [digitalocean_droplet.node.id]

  inbound_rule {
    protocol         = "tcp"
    port_range       = "22"
    source_addresses = [var.ssh_cidr]
  }
  dynamic "inbound_rule" {
    for_each = ["80", "443"]
    content {
      protocol         = "tcp"
      port_range       = inbound_rule.value
      source_addresses = ["0.0.0.0/0", "::/0"]
    }
  }
  # DO firewalls deny egress unless allowed; k3s needs to pull images and certs.
  dynamic "outbound_rule" {
    for_each = ["tcp", "udp"]
    content {
      protocol              = outbound_rule.value
      port_range            = "1-65535"
      destination_addresses = ["0.0.0.0/0", "::/0"]
    }
  }
  outbound_rule {
    protocol              = "icmp"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }
}

output "public_ip" { value = digitalocean_droplet.node.ipv4_address }
output "ssh" { value = "ssh root@${digitalocean_droplet.node.ipv4_address}" }
