variable "name_prefix" {
  description = "Short prefix used to name all networking resources"
  type        = string
}

variable "location" {
  description = "Azure region"
  type        = string
}

variable "resource_group_name" {
  description = "Resource group to create networking resources in"
  type        = string
}

variable "vnet_cidr" {
  description = "Address space for the VNet"
  type        = string
  default     = "10.10.0.0/16"
}

variable "aks_subnet_cidr" {
  description = "Address prefix for the AKS node subnet"
  type        = string
  default     = "10.10.1.0/24"
}

variable "tags" {
  description = "Tags applied to all networking resources"
  type        = map(string)
  default     = {}
}
