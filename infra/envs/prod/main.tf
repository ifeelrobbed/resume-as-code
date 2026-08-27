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
# cloud-provider auto-create one tied to the Service's lifecycle. The
# ingress-nginx Application's Helm values reference it explicitly by name via
# the azure-pip-name annotation so the cloud-provider treats it as external,
# not ephemeral.
#
# Deliberately in the app resource group, NOT the AKS-managed node resource
# group. The node RG's lifecycle belongs to the cluster: Azure deletes it
# wholesale when the cluster goes, taking anything inside with it. DNS points
# at this address, so having it there meant a cluster rebuild silently took
# the site's DNS target with it - and prevent_destroy is no defence, since it
# only guards destroys that Terraform initiates. See #65.
resource "azurerm_public_ip" "ingress_app_rg" {
  name                = "pip-${var.name_prefix}-ingress"
  resource_group_name = azurerm_resource_group.this.name
  location            = var.location
  allocation_method   = "Static"
  sku                 = "Standard"
  zones               = ["1", "2", "3"]
  tags                = var.tags

  lifecycle {
    # AKS's cloud-provider manages its own tags (k8s-azure-service, etc.) on
    # this resource - don't fight over them.
    ignore_changes = [tags]
  }
}

# AKS grants its own identity Contributor on the node resource group
# automatically, which is why the old IP needed no explicit grant. Anything
# outside that RG does: without this the service-controller can read the
# annotation, fail to access the IP, and report "user supplied IP Address was
# not found" while the LoadBalancer stays pending.
#
# Scoped to the IP itself rather than the whole resource group. Microsoft's
# own guidance (learn.microsoft.com/azure/aks/static-ip) says to scope it to
# the resource group, but the azure-pip-name annotation means the
# cloud-provider resolves this IP by name rather than listing the RG, so the
# narrower scope should be sufficient - and RG scope would hand the cluster
# network rights over the VNet, subnet and NSG as a side effect. If the
# service-controller ever reports it cannot find the IP, widening this scope
# to azurerm_resource_group.this.id is the first thing to try.
resource "azurerm_role_assignment" "ingress_pip_network_contributor" {
  scope                = azurerm_public_ip.ingress_app_rg.id
  role_definition_name = "Network Contributor"
  principal_id         = module.aks.cluster_identity_principal_id
}

# The original IP, still in the node resource group and still serving traffic.
# Kept until the cutover is verified so that reverting the ingress-nginx
# annotation is a complete rollback; removed in a follow-up once the new IP
# is confirmed live. prevent_destroy stays on until then.
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
    ignore_changes  = [tags]
  }
}
