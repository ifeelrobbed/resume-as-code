# This storage account is the one piece of infra NOT managed by this
# Terraform config - same chicken-and-egg reasoning as the Argo CD
# bootstrap script. Create it once, by hand or via bootstrap/, before
# running terraform init here:
#
#   az group create -n rg-resume-site-tfstate -l eastus2
#   az storage account create -n resumesitetfstate -g rg-resume-site-tfstate \
#     -l eastus2 --sku Standard_LRS --min-tls-version TLS1_2
#   az storage container create -n tfstate --account-name resumesitetfstate

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
