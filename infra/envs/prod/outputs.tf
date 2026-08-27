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
