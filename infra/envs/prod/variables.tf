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
# account for most of what is in use. Node memory working set runs in the
# mid-70s percent of the 5789Mi allocatable, peaking around 80% (Azure's
# node_memory_working_set_percentage, which agrees with `kubectl top node`).
#
# CPU is nearly idle by comparison - about 10% - yet requests reserve ~1346m
# of it (70%). #160 trimmed the repo-controlled requests from 600m to 235m;
# the remaining ~1111m belongs to AKS-managed components (metrics-server,
# coredns, the workload-identity webhook, kube-proxy and others) and cannot be
# tuned from this repo. A SKU with less CPU would still be refused on requests
# long before it ran out of real capacity.
#
# This is the largest line on the bill at $61.61/month against roughly $88
# total, and whether it needs to be this size is the subject of #118. With
# requests trimmed, Prometheus memory is the next lever, then re-pricing
# against the measured footprint. The site is linked from job applications,
# so those measurements include real visits: low-volume and bursty by design,
# individual reads rather than sustained load, which a SKU can be sized
# against with margin.
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
