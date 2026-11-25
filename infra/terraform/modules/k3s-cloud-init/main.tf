# Cloud-agnostic: renders cloud-init user data that turns a fresh Ubuntu
# 24.04 VM into a single-node k3s cluster. Every environment (Oracle,
# Hetzner, a future AWS variant) passes this output to its own VM resource,
# so the node is identical regardless of cloud.

variable "k3s_version" {
  description = "k3s release to install, e.g. v1.37.2+k3s1. Pinned so rebuilds are reproducible."
  type        = string
}

variable "open_host_firewall" {
  description = "Open 80/443 in the host iptables. Oracle's Ubuntu images ship an INPUT chain that REJECTs everything but SSH, so the VCN security list alone is not enough there."
  type        = bool
  default     = false
}

output "rendered" {
  description = "cloud-init user data (plain text; providers that need base64 encode it themselves)."
  value = templatefile("${path.module}/cloud-init.yaml.tftpl", {
    k3s_version        = var.k3s_version
    open_host_firewall = var.open_host_firewall
  })
}
