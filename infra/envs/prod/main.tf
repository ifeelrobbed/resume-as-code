terraform {
  required_version = ">= 1.7.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

provider "azurerm" {
  features {}
}

resource "azurerm_resource_group" "this" {
  name     = var.resource_group_name
  location = var.location
  tags     = var.tags
}

module "networking" {
  source = "../../modules/networking"

  name_prefix         = var.name_prefix
  location            = var.location
  resource_group_name = azurerm_resource_group.this.name
  tags                = var.tags
}

module "aks" {
  source = "../../modules/aks"

  name_prefix         = var.name_prefix
  location            = var.location
  resource_group_name = azurerm_resource_group.this.name
  subnet_id           = module.networking.subnet_id
  kubernetes_version  = var.kubernetes_version
  node_vm_size        = var.node_vm_size
  tags                = var.tags
}

# ingress-nginx's public IP, managed here instead of letting AKS's
# cloud-provider auto-create one tied to the Service's lifecycle. DNS
# (robertjcameron.com) points at this address, and losing it on a future
# ingress-nginx redeploy would break that; the ingress-nginx Application's
# Helm values reference it explicitly by name via the azure-pip-name
# annotation so the cloud-provider treats it as external, not ephemeral.
resource "azurerm_public_ip" "ingress" {
  name                = "pip-${var.name_prefix}-ingress"
  resource_group_name = module.aks.node_resource_group
  location            = var.location
  allocation_method   = "Static"
  sku                 = "Standard"
  zones               = ["1", "2", "3"]
  tags                = var.tags

  lifecycle {
    prevent_destroy = true
    # AKS's cloud-provider manages its own tags (k8s-azure-service, etc.) on
    # this resource - don't fight over them.
    ignore_changes = [tags]
  }
}
