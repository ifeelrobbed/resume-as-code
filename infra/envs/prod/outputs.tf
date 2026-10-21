output "cluster_name" {
  description = "Run: az aks get-credentials -g <resource_group_name> -n <cluster_name>"
  value       = module.aks.cluster_name
}

output "resource_group_name" {
  value = azurerm_resource_group.this.name
}

output "node_resource_group" {
  description = "Where the actual VMs, disks, and node-level LB/public IP live"
  value       = module.aks.node_resource_group
}

output "oidc_issuer_url" {
  value = module.aks.oidc_issuer_url
}

# The address DNS points at. Exposed so anything needing it reads it from
# Terraform rather than from a copy-pasted value that can go stale - see
# BOOTSTRAP.md's DNS section.
output "ingress_public_ip" {
  description = "Static public IP for ingress-nginx, in the app resource group"
  value       = azurerm_public_ip.ingress.ip_address
}

# Consumed by the resume-site ServiceAccount's
# azure.workload.identity/client-id annotation. Read it from here rather than
# copying the GUID around - a cluster rebuild or a re-created identity changes
# it, and a stale value fails as an opaque token-exchange error.
output "app_identity_client_id" {
  description = "Client ID of the app's user-assigned identity, for the ServiceAccount annotation"
  value       = azurerm_user_assigned_identity.app.client_id
}

# The three values the monitoring namespace's SecretProviderClass is
# parameterised with (keyvaultName, clientID, tenantId). Hardcoded there for
# the same reason as the app's client ID - a plain-directory Argo CD source has
# no templating - so read them from here rather than copying them around.
output "key_vault_name" {
  description = "Key Vault holding in-cluster third-party secrets"
  value       = azurerm_key_vault.this.name
}

output "keyvault_sync_identity_client_id" {
  description = "Client ID of the identity the monitoring keyvault-sync pod runs as"
  value       = azurerm_user_assigned_identity.keyvault_sync.client_id
}

output "tenant_id" {
  description = "Entra ID tenant the vault and identities belong to"
  value       = data.azurerm_client_config.current.tenant_id
}

output "app_storage_account_name" {
  description = "Storage account holding the visitor count blob"
  value       = azurerm_storage_account.app.name
}

output "app_stats_container_name" {
  description = "Container within the app storage account"
  value       = azurerm_storage_container.stats.name
}
