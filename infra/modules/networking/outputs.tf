output "vnet_id" {
  description = "ID of the VNet"
  value       = azurerm_virtual_network.this.id
}

output "vnet_name" {
  description = "Name of the VNet"
  value       = azurerm_virtual_network.this.name
}

output "subnet_id" {
  description = "ID of the AKS node subnet"
  value       = azurerm_subnet.aks.id
}
