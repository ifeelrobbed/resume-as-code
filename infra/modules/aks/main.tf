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

  # Both are load-bearing: the app authenticates to Blob Storage for the
  # visitor count with a federated ServiceAccount token, so turning either off
  # breaks it (see infra/envs/prod/app-identity.tf and ARCHITECTURE.md's
  # Secrets section).
  #
  # Turning them on at provisioning time is what made that possible - changing
  # either later forces a new cluster, so the cost of enabling them
  # speculatively was zero and the cost of not having them would have been a
  # rebuild.
  oidc_issuer_enabled       = true
  workload_identity_enabled = true

  # Both channels below were previously undeclared, which meant Azure's
  # defaults decided when this cluster got reimaged. That is how the node
  # reimage at 01:37 on 2026-09-10 happened with nothing in this repo
  # describing it (#140) - node_os_upgrade_channel defaults to "NodeImage"
  # whether or not it appears here. The upgrade_settings block below already
  # documents how disruptive a reimage is; leaving out what *triggers* one
  # declared only half of it.
  #
  # NodeImage keeps the existing behaviour - weekly-ish node image updates
  # carrying OS security patches. The alternatives are SecurityPatch (patch
  # in place, no image swap), None (nothing, and the node drifts), and
  # Unmanaged (Ubuntu's own unattended-upgrades). NodeImage stays because
  # patched images are worth taking and the disruption is now bounded by a
  # window rather than arbitrary.
  node_os_upgrade_channel = "NodeImage"

  # "patch" tracks the latest patch release of the current minor - security
  # fixes, without ever crossing a minor boundary. That boundary is the whole
  # risk: patch releases do not remove APIs, so nothing running here (Calico,
  # Argo CD, cert-manager, the kube-prometheus-stack CRDs) can break on an
  # API removal. "stable" and "rapid" do advance minors, and with no staging
  # environment prod would be where that got discovered.
  #
  # Minor upgrades therefore stay a deliberate decision. This does not make
  # the cluster maintenance-free - it makes it patched.
  automatic_upgrade_channel = "patch"

  # Two blocks, not one, and this is easy to get wrong: maintenance_window_
  # node_os bounds node image upgrades, maintenance_window_auto_upgrade
  # bounds Kubernetes version upgrades. Setting a channel without its
  # matching window leaves that channel firing whenever Azure likes, which
  # is the exact problem #140 exists to fix.
  #
  # Sunday 21:00-01:00 UTC is 16:00-20:00 CDT - awake and able to react - and
  # runs from late evening into the small hours across Europe (roughly
  # 21:00-03:00 local depending on the zone and the season), where a good
  # part of this site's audience is. 4h is the minimum the API accepts, so a
  # tighter window is not available. utc_offset stays +00:00 rather than
  # tracking a local zone, since Azure does not follow DST and a fixed offset
  # would silently shift the real local time twice a year.
  #
  # Both windows are the same slot deliberately: every disruption this
  # cluster takes on purpose lands in one predictable four hours a week.
  maintenance_window_node_os {
    frequency   = "Weekly"
    interval    = 1
    day_of_week = "Sunday"
    start_time  = "21:00"
    utc_offset  = "+00:00"
    duration    = 4
  }

  maintenance_window_auto_upgrade {
    frequency   = "Weekly"
    interval    = 1
    day_of_week = "Sunday"
    start_time  = "21:00"
    utc_offset  = "+00:00"
    duration    = 4
  }

  tags = var.tags

  lifecycle {
    # Originally here to avoid unplanned diffs when Azure deprecates a minor
    # version out from under an unpinned cluster, on the understanding that
    # version bumps would be made deliberately via PR. With
    # automatic_upgrade_channel = "patch" that is no longer what happens:
    # Azure moves the patch version on its own, inside the maintenance
    # window, and this is what stops Terraform trying to drag it back on the
    # next apply.
    #
    # So var.kubernetes_version is a floor, not a pin - it decides what the
    # cluster is built with, and nothing after that. Reading it as the
    # running version will be wrong; `az aks show` is the source of truth.
    # Pinning again means removing this and setting the channel to "none",
    # which has to be both or neither.
    ignore_changes = [kubernetes_version]
  }
}
