# Azure VM paid from "Azure for Students" credit (GitHub Student Pack: $100,
# no card). Standard_B2pls_v2 (2 vCPU, 4 GB, ARM64) is ~$25/month, so the
# credit lasts ~4 months; it renews yearly while enrolled. B2s and B2s_v2 are
# not offered to student subscriptions (az vm list-skus). Burstable: fine for
# portfolio traffic, throttles under sustained load once CPU credits run out.
# Same cloud-init module as Oracle and Hetzner.
# ponytail: when the credit runs out the subscription is disabled (no card on
# file, so no surprise bill); move to envs/hetzner before that.

terraform {
  required_version = ">= 1.9"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

variable "subscription_id" { type = string }
variable "ssh_public_key_path" { type = string }
variable "ssh_cidr" {
  type = string
  validation {
    condition     = var.ssh_cidr != "0.0.0.0/0"
    error_message = "Set ssh_cidr to your IP/32."
  }
}
variable "location" {
  # Azure for Students only allows some regions (az policy assignment list
  # shows them): westus, westus2, canadacentral, francecentral, belgiumcentral.
  type    = string
  default = "westus2"
}
variable "size" {
  type    = string
  default = "Standard_B2pls_v2"
}
variable "k3s_version" {
  type    = string
  default = "v1.37.2+k3s1"
}

provider "azurerm" {
  features {}
  subscription_id = var.subscription_id
}

module "cloud_init" {
  source      = "../../modules/k3s-cloud-init"
  k3s_version = var.k3s_version
  # Azure Ubuntu images have no restrictive host firewall; the NSG below filters.
}

resource "azurerm_resource_group" "this" {
  name     = "tollgate"
  location = var.location
}

resource "azurerm_virtual_network" "this" {
  name                = "tollgate"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  address_space       = ["10.20.0.0/16"]
}

resource "azurerm_subnet" "this" {
  name                 = "nodes"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = ["10.20.1.0/24"]
}

resource "azurerm_network_security_group" "this" {
  name                = "tollgate"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location

  security_rule {
    name                       = "ssh"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefix      = var.ssh_cidr
    destination_address_prefix = "*"
  }
  security_rule {
    name                       = "web"
    priority                   = 110
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_ranges    = ["80", "443"]
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
  # Everything else inbound hits Azure's default DenyAllInBound; egress is open by default.
}

resource "azurerm_subnet_network_security_group_association" "this" {
  subnet_id                 = azurerm_subnet.this.id
  network_security_group_id = azurerm_network_security_group.this.id
}

resource "azurerm_public_ip" "this" {
  name                = "tollgate"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  allocation_method   = "Static" # DNS (DuckDNS) points here; must survive reboots
  sku                 = "Standard"
}

resource "azurerm_network_interface" "this" {
  name                = "tollgate"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  ip_configuration {
    name                          = "primary"
    subnet_id                     = azurerm_subnet.this.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.this.id
  }
}

resource "azurerm_linux_virtual_machine" "node" {
  name                  = "tollgate"
  resource_group_name   = azurerm_resource_group.this.name
  location              = azurerm_resource_group.this.location
  size                  = var.size
  admin_username        = "ubuntu"
  network_interface_ids = [azurerm_network_interface.this.id]
  custom_data           = base64encode(module.cloud_init.rendered)

  admin_ssh_key {
    username   = "ubuntu"
    public_key = file(var.ssh_public_key_path)
  }
  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
    disk_size_gb         = 64
  }
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server-arm64"
    version   = "latest"
  }
  lifecycle {
    ignore_changes = [custom_data, source_image_reference]
  }
}

output "public_ip" { value = azurerm_public_ip.this.ip_address }
output "ssh" { value = "ssh ubuntu@${azurerm_public_ip.this.ip_address}" }
