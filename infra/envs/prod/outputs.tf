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

# The address DNS should point at once the cutover in #65 completes. Exposed
# so the runbook reads it from Terraform rather than from a copy-pasted value
# that can go stale.
output "ingress_public_ip" {
  description = "Static public IP for ingress-nginx, in the app resource group"
  value       = azurerm_public_ip.ingress_app_rg.ip_address
}

# The address DNS points at today. Removed once the old IP is torn down.
output "ingress_public_ip_legacy" {
  description = "Previous ingress IP, still in the AKS node resource group"
  value       = azurerm_public_ip.ingress.ip_address
}
