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

    # Azure sets these defaults on the node pool whether or not they're
    # declared, so leaving the block out meant every plan carried an
    # unrelated "remove upgrade_settings" diff waiting for the next apply -
    # which would have quietly reverted max_surge to the AKS default.
    #
    # Worth declaring rather than just silencing: max_surge governs how many
    # buffer nodes AKS adds before recycling an existing one, so it decides
    # how disruptive a node reimage is. That's not academic here - enabling a
    # network policy engine (#58) reimages the pool, and with node_count = 1
    # this is the difference between the site moving to a new node first and
    # the only node going away underneath it. 10% rounds up to one surge node.
    upgrade_settings {
      max_surge = "10%"
    }
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin    = "kubenet" # cheaper/simpler than Azure CNI at this scale
    load_balancer_sku = "standard"

    # Calico is the only policy engine kubenet supports - Azure NPM and Cilium
    # both require Azure CNI. Without it, NetworkPolicy objects are accepted by
    # the API server and enforced by nobody, which is how ARCHITECTURE.md came
    # to describe a control that did not exist (#58), and how six policies
    # shipped by the Argo CD chart sat inert for weeks.
    #
    # Enabled out of band with `az aks update --network-policy calico` before
    # this line was added, deliberately. Changing network_policy from "none"
    # forces a new cluster; changing it from nothing to a value that already
    # matches reality is a no-op. See BOOTSTRAP.md.
    network_policy = "calico"
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
