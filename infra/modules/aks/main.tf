# Single system node pool doubles as the workload pool - there is no
# separate user node pool for MVP. only_critical_addons_enabled stays
# false so the resume site, Prometheus, Grafana, etc. can all schedule here.
#
# sku_tier = "Free" avoids the paid Standard control-plane tier, which
# only buys an SLA that doesn't matter for a portfolio project.
#
# use_spot defaults to true for cost, but with node_count = 1 an eviction
# takes the whole site down until Azure reschedules it. That's an
# acceptable tradeoff for a demo site; set use_spot = false once this
# needs to stay up during, say, a live interview walkthrough.

resource "azurerm_kubernetes_cluster" "this" {
  name                = "${var.name_prefix}-aks"
  location            = var.location
  resource_group_name = var.resource_group_name
  dns_prefix          = "${var.name_prefix}-aks"
  kubernetes_version  = var.kubernetes_version
  sku_tier            = "Free"

  default_node_pool {
    name           = "system"
    vm_size        = var.node_vm_size
    node_count     = var.node_count
    vnet_subnet_id = var.subnet_id
    priority       = var.use_spot ? "Spot" : "Regular"
    eviction_policy = var.use_spot ? "Delete" : null
    spot_max_price  = var.use_spot ? var.spot_max_price : null
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
