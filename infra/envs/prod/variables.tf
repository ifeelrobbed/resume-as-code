variable "name_prefix" {
  description = "Workload-and-environment prefix (<workload>-<environment>); each resource name prepends its own type abbreviation, e.g. vnet-<name_prefix>"
  type        = string
  default     = "resume-site-prod"
}

variable "location" {
  description = "Azure region"
  type        = string
  default     = "westus2"
}

variable "resource_group_name" {
  description = "Resource group for all app resources (separate from the tfstate RG)"
  type        = string
  default     = "rg-resume-site-prod"
}

variable "kubernetes_version" {
  description = "Kubernetes version - leave null to track Azure's current default"
  type        = string
  default     = null
}

# 2 vCPU / 8 GiB. Sized by memory, not CPU: 4 GiB could not hold Prometheus,
# Grafana and the Argo CD application-controller together, which between them
# account for most of the ~4.8 GiB in use. CPU is nearly idle by comparison -
# about 10% - though requests reserve 84% of it, mostly chart defaults nobody
# chose, so a smaller SKU would be refused on requests long before it ran out
# of real capacity.
#
# This is the largest line on the bill at $61.61/month against roughly $88
# total, and whether it needs to be this size is the subject of #118. Trimming
# those requests and Prometheus retention comes first; re-pricing is only
# meaningful afterwards, and only against real traffic.
variable "node_vm_size" {
  description = "VM size for the single node pool"
  type        = string
  default     = "Standard_D2as_v7"
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
