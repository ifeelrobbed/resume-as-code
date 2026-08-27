output "cluster_name" {
  description = "Name of the AKS cluster"
  value       = azurerm_kubernetes_cluster.this.name
}

output "cluster_id" {
  description = "Resource ID of the AKS cluster"
  value       = azurerm_kubernetes_cluster.this.id
}

output "oidc_issuer_url" {
  description = "OIDC issuer URL - needed later for workload identity federation"
  value       = azurerm_kubernetes_cluster.this.oidc_issuer_url
}

output "node_resource_group" {
  description = "Auto-created resource group holding the VMs, disks, and node-level LB/public IP"
  value       = azurerm_kubernetes_cluster.this.node_resource_group
}

# Needed to grant the cluster identity rights over resources AKS doesn't own,
# specifically the ingress public IP now that it lives outside the node
# resource group. AKS grants itself Contributor on the node RG automatically;
# anything outside that has to be assigned explicitly.
output "cluster_identity_principal_id" {
  description = "Principal ID of the cluster's system-assigned managed identity"
  value       = azurerm_kubernetes_cluster.this.identity[0].principal_id
}

# Deliberately no kube_config output here. Fetch credentials with
# `az aks get-credentials` instead of piping a Terraform-managed
# kubeconfig (including its client cert/key) into state and CI logs.
