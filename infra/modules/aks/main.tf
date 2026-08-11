# Single system node pool doubles as the workload pool - there is no
# separate user node pool for MVP. only_critical_addons_enabled stays
# false so the resume site, Prometheus, Grafana, etc. can all schedule here.
#
# sku_tier = "Free" avoids the paid Standard control-plane tier, which
# only buys an SLA that doesn't matter for a portfolio project.
#
# No Spot pricing here: Azure doesn't allow the default/system node pool
# to run at Spot priority (only a secondary node pool can), and this
# module deliberately has just the one pool.

resource "azurerm_kubernetes_cluster" "this" {
  name                = "aks-${var.name_prefix}"
  location            = var.location
  resource_group_name = var.resource_group_name
  node_resource_group = "${var.resource_group_name}-aks-nodes"
  dns_prefix          = "aks-${var.name_prefix}"
  kubernetes_version  = var.kubernetes_version
  sku_tier            = "Free"

  default_node_pool {
    name                        = "system"
    temporary_name_for_rotation = "tmpsystem"
    vm_size                     = var.node_vm_size
    node_count                  = var.node_count
    vnet_subnet_id              = var.subnet_id
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin    = "kubenet" # cheaper/simpler than Azure CNI at this scale
    load_balancer_sku = "standard"
  }

  # Enabled now, unused until phase 2's Key Vault CSI driver / workload
  # identity work - free to turn on, so no reason to wait and re-provision.
  oidc_issuer_enabled       = true
  workload_identity_enabled = true

  tags = var.tags

  lifecycle {
    # Avoid unplanned diffs when Azure deprecates a minor version out
    # from under an unpinned cluster; bump this deliberately via PR instead.
    ignore_changes = [kubernetes_version]
  }
}
