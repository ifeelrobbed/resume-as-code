# Durable storage for the homepage visitor count, plus the workload identity
# the app uses to reach it (#75).
#
# The count currently comes from Prometheus, which makes it a rolling 15-day
# window that resets to zero whenever the TSDB is lost. Keeping it in a blob
# outside the cluster makes it an all-time number that survives a rebuild - and
# gives oidc_issuer_enabled / workload_identity_enabled on the cluster their
# first actual user, rather than being switched on and unused.
#
# Lives in this root module rather than modules/, for the same reason as the
# ingress IP: the federated credential binds the aks module's OIDC issuer to a
# service account in a namespace Argo CD owns. It's a seam between the cluster
# and the workload, which is what a composition root is for.

# Separate from the tfstate account deliberately. Reusing that one would put an
# internet-facing app's write credential in the same account as Terraform state;
# a second Standard LRS account costs essentially nothing.
resource "azurerm_storage_account" "app" {
  name                     = "resumesiteappdata"
  resource_group_name      = azurerm_resource_group.this.name
  location                 = var.location
  account_tier             = "Standard"
  account_replication_type = "LRS"
  tags                     = var.tags

  min_tls_version                 = "TLS1_2"
  https_traffic_only_enabled      = true
  allow_nested_items_to_be_public = false

  # Keys are enabled, reluctantly. shared_access_key_enabled = false is the
  # better posture and was tried first; it is incompatible with azurerm 4.81.0
  # (the latest 4.x) in a way no flag works around.
  #
  # Azure does not expose the fileServices/default sub-resource on an account
  # with shared key auth disabled - confirmed directly: it 404s with keys off
  # and appears the moment they are on. The provider reads share_properties for
  # every storage account regardless of whether Files is used, so the apply
  # fails on a sub-resource this account will never have.
  #
  # What that costs: account keys exist, so anyone able to call listKeys - which
  # Contributor on this resource group includes - can read the blob without a
  # role assignment. In practice that is the CI apply identity, which can
  # already create and delete every resource here.
  #
  # What it does NOT cost, and this is the part that matters: the app still
  # authenticates with a federated token exchanged at runtime. No key is stored
  # in the image, a Secret, or this repo. Enabling keys does not mean anything
  # uses them.
  #
  # Revisit when the provider stops reading share_properties unconditionally,
  # or on a v5 bump.
  shared_access_key_enabled = true

  blob_properties {
    # The blob this account exists to hold IS the visitor count. Losing it
    # resets the number to zero, which is the exact failure this feature is
    # meant to prevent - so a deleted or overwritten blob should be
    # recoverable. Pennies at this volume.
    versioning_enabled = true

    delete_retention_policy {
      days = 30
    }
  }
}

# Addressed by storage_account_id rather than storage_account_name, so the
# provider goes through Resource Manager instead of the Blob data plane. Kept
# that way even though keys are now enabled: it is one less thing depending on
# data-plane reachability from a CI runner.
resource "azurerm_storage_container" "stats" {
  name                  = "stats"
  storage_account_id    = azurerm_storage_account.app.id
  container_access_type = "private"
}

resource "azurerm_user_assigned_identity" "app" {
  name                = "id-${var.name_prefix}-app"
  resource_group_name = azurerm_resource_group.this.name
  location            = var.location
  tags                = var.tags
}

# The whole trick. AKS projects a service account token signed by the cluster's
# OIDC issuer; Entra ID trusts that issuer for this exact subject and exchanges
# the token for an Azure access token. No secret exists at any point.
#
# The subject must match the pod's service account exactly - namespace and name
# both - or the exchange fails with a mismatch that reads like a permissions
# problem. resume-site/resume-site is created in
# manifests/apps/resume-site/deploy/.
#
# issuer comes from the aks module rather than being hardcoded, so a cluster
# rebuild regenerates this with the new issuer URL instead of leaving a
# credential that silently no longer matches.
resource "azurerm_federated_identity_credential" "app" {
  name = "resume-site-serviceaccount"
  # Neither resource_group_name nor parent_id: the provider deprecated both and
  # removes them in v5. resource_group_name is "no longer used" outright, and
  # parent_id was renamed to user_assigned_identity_id. Both surfaced as plan
  # warnings rather than errors, so they would have gone unnoticed until a
  # major-version bump broke the apply.
  user_assigned_identity_id = azurerm_user_assigned_identity.app.id
  audience                  = ["api://AzureADTokenExchange"]
  issuer                    = module.aks.oidc_issuer_url
  subject                   = "system:serviceaccount:resume-site:resume-site"
}

# Scoped to the container, not the account - the app has no business reading or
# writing anything else that lands here later. Same least-privilege reasoning as
# the ingress IP's Network Contributor grant.
#
# Creating this needs the apply identity's RBAC Administrator condition to allow
# Storage Blob Data Contributor; it was widened for exactly this (see
# BOOTSTRAP.md section 3).
# .id, not .resource_manager_id: because the container is addressed by
# storage_account_id above, its id already IS the Resource Manager ID. The
# separate attribute is deprecated and goes away in provider v5.
resource "azurerm_role_assignment" "app_stats_blob" {
  scope                = azurerm_storage_container.stats.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.app.principal_id
}
