# This storage account is NOT managed by this Terraform config - same
# chicken-and-egg reasoning as the Argo CD bootstrap script: Terraform can't
# keep its state in a resource it hasn't created yet. Create it once before
# running terraform init here; the commands live in BOOTSTRAP.md at the repo
# root, section 1, rather than being repeated in a comment that would go
# stale the first time they changed.

terraform {
  backend "azurerm" {
    resource_group_name  = "rg-resume-site-tfstate"
    storage_account_name = "resumesitetfstate" # must be globally unique - rename before first use
    container_name       = "tfstate"
    key                  = "prod.tfstate"
    # AAD RBAC instead of the storage account key - the CI plan identity
    # only has Reader on this RG, which doesn't include listKeys.
    use_azuread_auth = true
  }
}
