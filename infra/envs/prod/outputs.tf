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
