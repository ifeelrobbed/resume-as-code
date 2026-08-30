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
  features {
    storage {
      # Load-bearing, for a reason that is easy to misread as obsolete.
      #
      # By default the provider builds Blob/Queue/Table data-plane clients for
      # every storage account, and does so by calling listKeys. The plan
      # identity holds Reader precisely so a pull request cannot read data, and
      # Reader excludes listKeys - so every `terraform plan` touching a storage
      # account 403s on an account it is only meant to describe.
      #
      # It also skips the post-create data-plane readiness poll. That poll
      # authenticates with a shared key, so it fails outright on an account
      # with shared_access_key_enabled = false - which the app storage account
      # is. Both reasons independently require this flag; removing it broke
      # plan immediately when tried (#75).
      #
      # Nothing here needs data-plane access from Terraform:
      # azurerm_storage_container is addressed by storage_account_id, which
      # goes through Resource Manager, and the app reaches blobs at runtime via
      # workload identity.
      data_plane_available = false
    }
  }
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
#
# Lives here rather than in modules/networking because its role assignment
# below needs the cluster identity, while module.aks already consumes
# networking's subnet - moving the pair in would make networking depend on aks
# and aks on networking. It's a seam between the two modules, which is what a
# composition root is for.

resource "azurerm_public_ip" "ingress" {
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
  scope                = azurerm_public_ip.ingress.id
  role_definition_name = "Network Contributor"
  principal_id         = module.aks.cluster_identity_principal_id
}

