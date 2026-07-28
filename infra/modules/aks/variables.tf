variable "name_prefix" {
  description = "Workload-and-environment prefix (<workload>-<environment>); combined with the aks- type prefix to name the cluster"
  type        = string
}

variable "location" {
  description = "Azure region"
  type        = string
}

variable "resource_group_name" {
  description = "Resource group to create the cluster in"
  type        = string
}

variable "subnet_id" {
  description = "Subnet the node pool's VMs are placed in"
  type        = string
}

variable "kubernetes_version" {
  description = "Kubernetes version for the control plane and node pool"
  type        = string
  default     = null # null lets Azure pick the current default version
}

variable "node_vm_size" {
  description = "VM size for the single node pool"
  type        = string
  default     = "Standard_B2s" # burstable, cheap - fine for a low-traffic site
}

variable "node_count" {
  description = "Fixed node count (no autoscaling for MVP - keeps cost predictable)"
  type        = number
  default     = 1
}

variable "use_spot" {
  description = "Run the node pool on Spot pricing. Cheaper, but nodes can be evicted."
  type        = bool
  default     = true
}

variable "spot_max_price" {
  description = "Max hourly price willing to pay for Spot nodes. -1 means pay up to the on-demand price."
  type        = number
  default     = -1
}

variable "tags" {
  description = "Tags applied to the cluster"
  type        = map(string)
  default     = {}
}
