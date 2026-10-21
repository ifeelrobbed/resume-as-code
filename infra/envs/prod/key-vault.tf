# Key Vault for the in-cluster secrets that cannot be derived from anything in
# this repo - third-party bearer URLs, starting with Alertmanager's two (#145).
#
# The point is disaster recovery, not tidiness. Hand-created Kubernetes Secrets
# die with the cluster, and recreating them was the one rebuild step nothing
# verified. This vault lives in the app resource group, which Azure does not
# delete with the cluster, so a rebuild gets its secrets back the same way it
# gets the ingress IP and the visitor-count blob back: by never losing them.
#
# Terraform declares the vault and who may read it, never the values. An
# azurerm_key_vault_secret would put each value in state and in plan output on
# a public repo's CI logs. Values are set by hand with `az keyvault secret set`
# - see BOOTSTRAP.md section 6.
#
# Delivery into the cluster is the Secrets Store CSI driver
# (key_vault_secrets_provider in modules/aks), through a dedicated sync pod in
# the monitoring namespace rather than a volume on Alertmanager itself. The
# operator will not create the Alertmanager StatefulSet until every Secret its
# config references exists, and the CSI driver only creates a synced Secret
# once a pod mounts the volume - so mounting it on Alertmanager deadlocks on
# any fresh install or rebuild. Confirmed in a throwaway namespace before
# committing to this: no StatefulSet for as long as the Secret was missing,
# then a Ready pod within ~12s of it appearing.

data "azurerm_client_config" "current" {}

resource "azurerm_key_vault" "this" {
  # Globally unique, 3-24 characters. Checked available before first apply.
  name                = "kv-${var.name_prefix}"
  resource_group_name = azurerm_resource_group.this.name
  location            = var.location
  tenant_id           = data.azurerm_client_config.current.tenant_id
  sku_name            = "standard"
  tags                = var.tags

  # Azure RBAC rather than access policies, so access is granted with the same
  # role assignments as everything else here - scoped, visible in one place,
  # and covered by the apply identity's ABAC condition. Access policies are a
  # separate permission system that a Contributor can edit directly.
  rbac_authorization_enabled = true

  # 90 is Azure's default, declared because it cannot be changed after
  # creation. The long window does not trap the name: with purge protection
  # off, a soft-deleted vault can be recovered or purged at any time.
  #
  # Not automatically, though. The provider's recover-on-create looks for
  # soft-deleted vaults at subscription scope, and treats a 403 there as "none
  # exists" (key_vault_resource.go, v4.81.0). The apply identity is scoped to
  # the resource group, so it always gets that 403, and a create over a
  # soft-deleted vault fails with a name conflict instead of recovering it.
  # The fix is by hand: `az keyvault recover`, then `terraform import` - see
  # BOOTSTRAP.md's recovery section.
  soft_delete_retention_days = 90

  # Off, on reasoning rather than cost. Purge protection cannot be disabled
  # once enabled, and what it adds over soft delete is defence against a
  # deliberate purge by someone already holding purge rights. The realistic
  # failure is an accidental destroy, which soft delete plus
  # purge_soft_delete_on_destroy = false (main.tf) already makes recoverable
  # for 90 days. And everything stored here can be reissued at its source - a new
  # Discord webhook, a new healthchecks.io ping URL - so the worst case is an
  # inconvenience, not a loss.
  purge_protection_enabled = false

  # Public endpoint, RBAC-gated. The CSI provider reaches the vault from the
  # node, and a private endpoint would add ~$7/month plus a private DNS zone
  # to protect two webhook URLs whose read access is already limited to one
  # federated identity.
  public_network_access_enabled = true
}

# The identity the sync pod runs as. Separate from the app's identity: that
# one can write the visitor-count blob, and nothing in monitoring should be
# able to. Alertmanager itself gets no Azure identity at all - it only ever
# sees the synced Kubernetes Secret.
resource "azurerm_user_assigned_identity" "keyvault_sync" {
  name                = "id-${var.name_prefix}-keyvault-sync"
  resource_group_name = azurerm_resource_group.this.name
  location            = var.location
  tags                = var.tags
}

# Same mechanism as azurerm_federated_identity_credential.app, and the same
# rules apply: the subject must match the sync pod's ServiceAccount exactly,
# and the issuer comes from the aks module so a rebuild regenerates it rather
# than leaving a credential that silently no longer matches.
resource "azurerm_federated_identity_credential" "keyvault_sync" {
  name                      = "monitoring-keyvault-sync-serviceaccount"
  user_assigned_identity_id = azurerm_user_assigned_identity.keyvault_sync.id
  audience                  = ["api://AzureADTokenExchange"]
  issuer                    = module.aks.oidc_issuer_url
  subject                   = "system:serviceaccount:monitoring:keyvault-sync"
}

# Vault scope rather than per secret. Per-secret scopes would need each secret
# to exist as a Terraform resource, which is exactly what is avoided above.
# The vault holds only what this identity exists to read, so the difference is
# nil in practice.
#
# Creating this needs Key Vault Secrets User in the apply identity's RBAC
# Administrator condition - see BOOTSTRAP.md section 3.
resource "azurerm_role_assignment" "keyvault_sync_secrets_user" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.keyvault_sync.principal_id
}
