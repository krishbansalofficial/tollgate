# Public subnet for a single node: VCN, internet gateway, route table and a
# security list that admits SSH (from one CIDR), HTTP and HTTPS only.
# The Kubernetes API (6443) is deliberately not opened: use an SSH tunnel.

terraform {
  required_providers {
    oci = { source = "oracle/oci" }
  }
}

variable "compartment_id" { type = string }
variable "name" { type = string }
variable "vcn_cidr" {
  type    = string
  default = "10.0.0.0/16"
}
variable "subnet_cidr" {
  type    = string
  default = "10.0.1.0/24"
}
variable "ssh_cidr" {
  description = "CIDR allowed to SSH, e.g. your home IP/32. Never 0.0.0.0/0."
  type        = string
  validation {
    condition     = var.ssh_cidr != "0.0.0.0/0"
    error_message = "Refusing to open SSH to the whole internet; set ssh_cidr to your IP/32."
  }
}

resource "oci_core_vcn" "this" {
  compartment_id = var.compartment_id
  cidr_blocks    = [var.vcn_cidr]
  display_name   = var.name
  dns_label      = "tollgate"
}

resource "oci_core_internet_gateway" "this" {
  compartment_id = var.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-igw"
}

resource "oci_core_route_table" "this" {
  compartment_id = var.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-rt"
  route_rules {
    destination       = "0.0.0.0/0"
    network_entity_id = oci_core_internet_gateway.this.id
  }
}

resource "oci_core_security_list" "this" {
  compartment_id = var.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-sl"

  egress_security_rules {
    destination = "0.0.0.0/0"
    protocol    = "all"
  }

  ingress_security_rules {
    source   = var.ssh_cidr
    protocol = "6" # TCP
    tcp_options {
      min = 22
      max = 22
    }
  }

  dynamic "ingress_security_rules" {
    for_each = [80, 443]
    content {
      source   = "0.0.0.0/0"
      protocol = "6"
      tcp_options {
        min = ingress_security_rules.value
        max = ingress_security_rules.value
      }
    }
  }

  # ICMP "fragmentation needed": without it, path MTU discovery breaks and
  # large TLS responses can stall.
  ingress_security_rules {
    source   = "0.0.0.0/0"
    protocol = "1"
    icmp_options {
      type = 3
      code = 4
    }
  }
}

resource "oci_core_subnet" "this" {
  compartment_id    = var.compartment_id
  vcn_id            = oci_core_vcn.this.id
  cidr_block        = var.subnet_cidr
  display_name      = "${var.name}-public"
  dns_label         = "public"
  route_table_id    = oci_core_route_table.this.id
  security_list_ids = [oci_core_security_list.this.id]
}

output "subnet_id" { value = oci_core_subnet.this.id }
