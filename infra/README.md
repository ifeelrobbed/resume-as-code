# infra/

Terraform for the AKS cluster and its supporting Azure resources. Anything
running *inside* the cluster (ingress, the resume site, Prometheus, Argo CD
itself) is managed by Argo CD from `manifests/` instead - Terraform's job
stops at the cluster boundary.

## Layout

- `modules/networking` - VNet, one subnet, one NSG. No firewall, no hub-spoke.
- `modules/aks` - the cluster itself, one small node pool.
- `envs/prod` - the only environment for now; wires the modules together
  with real values.

## One-time setup (not managed by this Terraform)

Remote state needs somewhere to live before `terraform init` will work:

```
az group create -n rg-resume-site-tfstate -l eastus2
az storage account create -n resumesitetfstate -g rg-resume-site-tfstate \
  -l eastus2 --sku Standard_LRS --min-tls-version TLS1_2
az storage container create -n tfstate --account-name resumesitetfstate
```

Storage account names are globally unique across all of Azure - rename
`resumesitetfstate` in `envs/prod/backend.tf` if it's taken.

## Usage

```
cd envs/prod
terraform init
terraform plan
terraform apply
az aks get-credentials -g "$(terraform output -raw resource_group_name)" \
  -n "$(terraform output -raw cluster_name)"
```

In CI, `terraform-plan.yml` runs `plan` on every PR touching `infra/`, and
`terraform-apply.yml` runs `apply` on merge to main - same review-then-merge
flow as any other change in the repo.

## Cost notes

Deliberately excluded to avoid the resources that made a prior landing-zone
attempt expensive: Azure Firewall, Application Gateway, Azure Monitor
managed Prometheus/Grafana, a Bastion host. Everything those would have
done is instead handled by NSGs + NetworkPolicies, ingress-nginx +
cert-manager, self-hosted Prometheus/Grafana, and `az aks command invoke`
for admin access.

Rough floor at these defaults (1x Standard_B2s on Spot, Free-tier control
plane, Standard LB, one public IP): well under $50/month. The single
biggest lever if this needs to shrink further is dropping to a smaller
VM size; the single biggest lever if it needs to grow is turning off
`use_spot` before anything that must not be interrupted mid-demo.
