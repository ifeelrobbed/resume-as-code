variable "name_prefix" {
  description = "Short prefix used to name all resources"
  type        = string
  default     = "resumesite"
}

variable "location" {
  description = "Azure region"
  type        = string
  default     = "eastus2"
}

variable "resource_group_name" {
  description = "Resource group for all app resources (separate from the tfstate RG)"
  type        = string
  default     = "resume-site-prod-rg"
}

variable "kubernetes_version" {
  description = "Kubernetes version - leave null to track Azure's current default"
  type        = string
  default     = null
}

variable "node_vm_size" {
  description = "VM size for the single node pool"
  type        = string
  default     = "Standard_B2s"
}

variable "use_spot" {
  description = "Run the node pool on Spot pricing"
  type        = bool
  default     = true
}

variable "tags" {
  description = "Tags applied to all resources"
  type        = map(string)
  default = {
    project     = "resume-site"
    environment = "prod"
    managed_by  = "terraform"
  }
}
