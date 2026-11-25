# Oracle Cloud Always Free: single-node k3s on one A1 (ARM64) VM. ~$0/month.
#
#   cp terraform.tfvars.example terraform.tfvars   # fill in; it is gitignored
#   terraform init && terraform apply

terraform {
  required_version = ">= 1.9"
  required_providers {
    oci = {
      source  = "oracle/oci"
      version = "~> 9.15"
    }
  }
}

variable "tenancy_ocid" { type = string }
variable "user_ocid" { type = string }
variable "fingerprint" { type = string }
variable "private_key_path" { type = string }
variable "region" { type = string }
variable "compartment_ocid" {
  description = "Compartment for all resources; the tenancy OCID uses the root compartment."
  type        = string
}
variable "ssh_public_key_path" { type = string }
variable "ssh_cidr" { type = string }
variable "k3s_version" {
  type    = string
  default = "v1.37.2+k3s1"
}
variable "availability_domain_index" {
  type    = number
  default = 0
}

provider "oci" {
  tenancy_ocid     = var.tenancy_ocid
  user_ocid        = var.user_ocid
  fingerprint      = var.fingerprint
  private_key_path = var.private_key_path
  region           = var.region
}

module "cloud_init" {
  source             = "../../modules/k3s-cloud-init"
  k3s_version        = var.k3s_version
  open_host_firewall = true
}

module "network" {
  source         = "../../modules/oci-network"
  compartment_id = var.compartment_ocid
  name           = "tollgate"
  ssh_cidr       = var.ssh_cidr
}

module "node" {
  source                    = "../../modules/oci-k3s-node"
  tenancy_ocid              = var.tenancy_ocid
  compartment_id            = var.compartment_ocid
  subnet_id                 = module.network.subnet_id
  name                      = "tollgate"
  ssh_public_key            = file(var.ssh_public_key_path)
  user_data                 = module.cloud_init.rendered
  availability_domain_index = var.availability_domain_index
}

output "public_ip" { value = module.node.public_ip }
output "ssh" { value = "ssh ubuntu@${module.node.public_ip}" }
