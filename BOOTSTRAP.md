# Bootstrap

Everything this project needs that is **not** created by Terraform, Argo CD, or
CI. If the cluster and its subscription were rebuilt from nothing, this is the
list that has to be worked through by hand before the automated parts can run.

Each entry says why it can't be in code. That reasoning matters more than the
commands: the goal is for this list to stay short, and every addition to it
should have to justify itself. Two of these exist purely because of
chicken-and-egg (Terraform's own state, and the identity Terraform runs as),
which is a good reason. "It was quicker at the time" is not.

Identifiers below are placeholders. The real values live in the GitHub repo
variables and in the Azure portal — this file is public.

## Order

Roughly dependency order; 1–4 are prerequisites for any `terraform apply`,
5–6 need a running cluster, 7–8 need a running site.

---

## 1. Terraform state backend

**Why not in code:** Terraform cannot store its state in a resource it hasn't
created yet.

```bash
az group create -n rg-resume-site-tfstate -l eastus2
az storage account create -n resumesitetfstate -g rg-resume-site-tfstate \
  -l eastus2 --sku Standard_LRS --min-tls-version TLS1_2
az storage container create -n tfstate --account-name resumesitetfstate
```

Storage account names are globally unique across Azure; rename in
`infra/envs/prod/backend.tf` if taken.

The backend uses `use_azuread_auth = true` rather than an account key — the
plan identity holds Reader on this resource group, which doesn't include
`listKeys`.

**Worth adding on a rebuild:** blob versioning and soft delete. Neither is set
today, so there is currently no recovery from a corrupted or truncated state
file beyond whatever Terraform last wrote.

## 2. GitHub OIDC app registrations

**Why not in code:** these are the identities Terraform authenticates *as*.
Terraform can't create its own credentials.

Two app registrations, deliberately split so that a pull request from anywhere
can never hold write access:

| App | Purpose | Federated credential subject |
|---|---|---|
| `resume-site-gh-plan` | `terraform plan` on PRs | `repo:<owner>@<ownerId>/<repo>@<repoId>:pull_request` |
| `resume-site-gh-apply` | `terraform apply` on merge | `repo:<owner>@<ownerId>/<repo>@<repoId>:environment:production` |

Both use issuer `https://token.actions.githubusercontent.com` and audience
`api://AzureADTokenExchange`.

**The non-obvious part:** the subject uses GitHub's **immutable numeric IDs**,
not the plain `repo:owner/name` form most guides show. The plain form silently
fails to match. Get the IDs with:

```bash
gh api repos/<owner>/<repo> --jq '{repoId: .id, ownerId: .owner.id}'
```

The apply credential is bound to `environment:production`, which is what makes
the GitHub environment approval gate (section 4) a real security boundary
rather than a UI convenience — without an approved deployment to that
environment, no token is ever issued.

## 3. Azure role assignments

**Why not in code:** the grants that let CI authenticate must exist before CI
can run. The rest could in principle be Terraform-managed, and one now is.

### Manual — bootstrap grants

| Principal | Role | Scope |
|---|---|---|
| `resume-site-gh-plan` | Reader | `rg-resume-site-prod` |
| `resume-site-gh-plan` | Reader | `rg-resume-site-prod-aks-nodes` |
| `resume-site-gh-plan` | Reader | `rg-resume-site-tfstate` |
| `resume-site-gh-plan` | Storage Blob Data **Reader** | tfstate storage account |
| `resume-site-gh-plan` | Azure Kubernetes Service Cluster User | the AKS cluster |
| `resume-site-gh-apply` | Contributor | `rg-resume-site-prod` |
| `resume-site-gh-apply` | Contributor | `rg-resume-site-prod-aks-nodes` |
| `resume-site-gh-apply` | Storage Blob Data **Contributor** | tfstate storage account |

The plan identity's read-only blob access is why `terraform-plan.yml` runs with
`-lock=false`: acquiring a state lock is a blob write even for a read-only
plan.

### Manual — the constrained RBAC grant

`resume-site-gh-apply` also holds **Role Based Access Control Administrator**
on `rg-resume-site-prod`, so that workload role assignments can live in
Terraform rather than accumulating in this file.

It carries an ABAC condition restricting it to assigning **Network
Contributor** only. This matters: `roleAssignments/write` places no inherent
limit on *which* role may be granted, so an unconditioned grant would let the
pipeline assign itself Owner — making it Owner-equivalent at that scope.

To allow a new role, add its GUID to **both** brace lists in the condition
(one governs create, the other delete). Verify a condition is actually attached
after any change:

```bash
az role assignment list --all --assignee-object-id <apply-sp-object-id> \
  --query "[?roleDefinitionName=='Role Based Access Control Administrator'].{scope:scope,condition:condition}" -o json
```

A `null` condition is the Owner-equivalent case — delete and recreate rather
than leaving it.

### Not manual, and not mine

- **Cluster identity → Contributor on the node resource group.** Created and
  maintained by AKS. Don't import it; Terraform and the platform would fight.
- **Cluster identity → Network Contributor on the ingress public IP.** Managed
  by Terraform (`azurerm_role_assignment.ingress_pip_network_contributor`).
  Needed because the IP lives outside the node resource group, where AKS's
  automatic grant doesn't reach.

## 4. GitHub repository configuration

**Why not in code:** no Terraform provider is configured for GitHub, and adding
one would need a token — a credential this repo currently doesn't have to
manage.

| Setting | Value | Why |
|---|---|---|
| Repo variables | `AZURE_CLIENT_ID_PLAN`, `AZURE_CLIENT_ID_APPLY`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID` | Consumed by the terraform workflows. Variables, not secrets — none are credentials. |
| Environment `production` | Required reviewer | The approval gate for `terraform-apply`, and the thing the apply identity's federated credential is bound to. |
| Branch protection on `master` | PR required, 0 approvals, admins not enforced | Solo repo: the PR requirement is for an audit trail, not review. |
| Allow Actions to create and approve pull requests | **enabled** | Required by `app-ci`'s deploy job. Without it `gh pr create` fails with `GitHub Actions is not permitted to create or approve pull requests`. |
| Automatically delete head branches | enabled | Stops merged branches accumulating. |

No required status checks are configured. If that changes, the deploy job needs
attention: GitHub does not trigger `pull_request` workflows for PRs opened with
`GITHUB_TOKEN`, so its auto-created deploy PR would wait forever on checks that
never run.

## 5. Argo CD

**Why not in code:** the thing that applies manifests can't be installed by
applying a manifest.

```bash
./bootstrap/install-argocd.sh
```

Run once against a fresh cluster. It fetches credentials, installs the Argo CD
Helm chart at the pinned version using the exact values from
`manifests/platform/argocd/application.yaml`, then applies
`manifests/root.yaml`. From that point Argo CD manages its own upgrades —
bump `targetRevision` in a PR.

Print the initial admin password, change it, then delete
`argocd-initial-admin-secret`.

## 6. In-cluster secrets

**Why not in code:** no secrets management yet — no Key Vault, no CSI driver,
no workload identity federation. The cluster has `oidc_issuer_enabled` and
`workload_identity_enabled` on, so adding it is a small change when a second
secret makes it worth doing.

| Secret | Namespace | Contents |
|---|---|---|
| `alertmanager-discord-webhook` | `monitoring` | key `webhook-url` — the Discord webhook Alertmanager posts to |

```bash
kubectl -n monitoring create secret generic alertmanager-discord-webhook \
  --from-literal=webhook-url='<discord-webhook-url>'
```

Referenced by `manifests/platform/kube-prometheus-stack/alertmanager-config.yaml`
as a `SecretKeySelector`.

## 7. Grafana public dashboard

**Why not in code:** Grafana's Public Dashboard share config isn't
provisionable — not by the chart, not by the dashboard JSON. It's an API/UI
action against a running Grafana.

Enable sharing on the `resume-site` dashboard (uid `resume-site`, fixed
deliberately so re-provisioning the JSON doesn't orphan the share), then take
the resulting token URL.

**The URL is currently hardcoded** at `app/handlers.go`'s
`grafanaDashboardURL`, so re-creating the share means a code change and
redeploy. Tracked in #65 as something to move into config.

The share survives re-provisioning the dashboard JSON. It does **not** survive
losing Grafana's database — which lives on a PVC in the node resource group,
and so does not survive a cluster rebuild.

## 8. DNS

**Why not in code:** the zone is hosted at Cloudflare and no provider is wired
up. Managing it in Terraform would need an API token — again, a credential the
repo doesn't currently handle.

| Record | Type | Points at |
|---|---|---|
| `robertjcameron.com` | A | ingress public IP |
| `grafana.robertjcameron.com` | A | ingress public IP |

DNS-only (not proxied), so the A records resolve straight to Azure. Read the
current address from Terraform rather than copying it:

```bash
terraform -chdir=infra/envs/prod output -raw ingress_public_ip
```

TTL is 300s normally. Lower it to 60 before any planned IP change and restore
it afterwards.

---

## Deliberately not here

Terraform owns everything in Azure above the cluster boundary; Argo CD owns
everything inside it. If something appears in this file that either of them
could own, that's a defect, not a convention.

## Related

- `ARCHITECTURE.md` — system design and the reasoning behind the layout
- `infra/README.md` — Terraform usage
- #65 — what a cluster rebuild currently destroys, and why this list is not yet
  a complete recovery runbook
