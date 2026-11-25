# One Ampere A1 (ARM64) VM sized inside the Always Free allowance.
#
# Always Free A1 allowance (per reports from June 2026; Oracle's docs page
# lagged): 1,500 OCPU-hours and 9,000 GB-hours per month = 2 OCPU / 12 GB
# running continuously. Exceeding it on a free tenancy disables and then
# deletes ALL A1 instances, so the caps below are hard validations.

terraform {
  required_providers {
    oci = { source = "oracle/oci" }
  }
}

variable "tenancy_ocid" { type = string }
variable "compartment_id" { type = string }
variable "subnet_id" { type = string }
variable "name" { type = string }
variable "ssh_public_key" { type = string }
variable "user_data" {
  description = "cloud-init (plain text), from the k3s-cloud-init module."
  type        = string
}
variable "availability_domain_index" {
  description = "A1 capacity varies by AD; try another index if creation fails with 'Out of host capacity'."
  type        = number
  default     = 0
}

variable "ocpus" {
  type    = number
  default = 2
  validation {
    condition     = var.ocpus >= 1 && var.ocpus <= 2
    error_message = "Always Free A1 allows at most 2 OCPUs (2026 limits)."
  }
}

variable "memory_gb" {
  type    = number
  default = 12
  validation {
    condition     = var.memory_gb >= 1 && var.memory_gb <= 12
    error_message = "Always Free A1 allows at most 12 GB (2026 limits)."
  }
}

variable "boot_volume_gb" {
  type    = number
  default = 100
  validation {
    condition     = var.boot_volume_gb >= 50 && var.boot_volume_gb <= 200
    error_message = "Always Free block storage totals 200 GB."
  }
}

data "oci_identity_availability_domains" "ads" {
  compartment_id = var.tenancy_ocid
}

data "oci_core_images" "ubuntu" {
  compartment_id           = var.compartment_id
  operating_system         = "Canonical Ubuntu"
  operating_system_version = "24.04"
  shape                    = "VM.Standard.A1.Flex"
  sort_by                  = "TIMECREATED"
  sort_order               = "DESC"
}

resource "oci_core_instance" "node" {
  availability_domain = data.oci_identity_availability_domains.ads.availability_domains[var.availability_domain_index].name
  compartment_id      = var.compartment_id
  display_name        = var.name
  shape               = "VM.Standard.A1.Flex"

  shape_config {
    ocpus         = var.ocpus
    memory_in_gbs = var.memory_gb
  }

  source_details {
    source_type             = "image"
    source_id               = data.oci_core_images.ubuntu.images[0].id
    boot_volume_size_in_gbs = var.boot_volume_gb
  }

  create_vnic_details {
    subnet_id        = var.subnet_id
    assign_public_ip = true
  }

  metadata = {
    ssh_authorized_keys = var.ssh_public_key
    user_data           = base64encode(var.user_data)
  }

  lifecycle {
    # A newer Ubuntu image must not replace (and wipe) the running node.
    ignore_changes = [source_details[0].source_id, metadata["user_data"]]
  }
}

output "public_ip" { value = oci_core_instance.node.public_ip }
