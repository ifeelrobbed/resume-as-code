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

output "app_storage_account_name" {
  description = "Storage account holding the visitor count blob"
  value       = azurerm_storage_account.app.name
}

output "app_stats_container_name" {
  description = "Container within the app storage account"
  value       = azurerm_storage_container.stats.name
}
