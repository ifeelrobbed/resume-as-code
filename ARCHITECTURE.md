# Architecture

## The pitch

A resume site that's also a running demo of the platform work behind it.
Current title is Senior Site Reliability Engineer; the actual work is
closer to Platform Engineering (Kubernetes, Terraform, Crossplane,
Go tooling, GitOps). This project makes that explicit: the site is built,
deployed, and operated the way real infrastructure is, and shows that
work live rather than just describing it in bullet points.

## High-level architecture

```
GitHub (source: app, infra, manifests, promql)
  -> GitHub Actions (CI: build/test app, terraform plan)
  -> Argo CD (GitOps CD: syncs manifests/ into the cluster)
  -> AKS cluster (Azure)
       - ingress-nginx + cert-manager
       - resume site (Go app)
       - Prometheus + Grafana
       - custom controller(s) (phase 3)
```

Terraform owns anything that costs money or is infrastructure (AKS, VNet,
DNS). Argo CD owns everything running inside the cluster. Nothing is
`kubectl apply`'d by hand.

## Cost-conscious decisions

Running a full blown landing zone for this site is not feasible from a cost perpective. Explicitly avoided:

- **Azure Firewall** - NSGs at the subnet edge plus Kubernetes
  NetworkPolicies inside the cluster, enforced by Calico (the only engine
  kubenet supports). Worth noting this line was aspirational when first
  written and only became true with #58: no policies existed and no engine
  was installed, so anything written would have been accepted by the API
  server and enforced by nobody.
- **Application Gateway** - ingress-nginx + cert-manager (Let's Encrypt).
- **Azure Managed Prometheus/Grafana** - self-hosted in-cluster instead
  (also more demo-worthy: it's the observability stack, self-run).
- **Hub-spoke networking / Bastion** - one VNet, one subnet;
  `az aks command invoke` for admin access instead of a jump box.
- **Standard AKS control-plane tier** - `sku_tier = "Free"`, since the
  SLA it buys doesn't matter here.
- **Azure CNI** - `kubenet` instead, simpler and cheaper at this scale.
- **Spot pricing** - not used. Azure doesn't allow the default/system
  node pool to run at Spot priority, and this module deliberately has
  only the one pool, so the node runs on-demand.
- **Statically provisioned PVC disks** - the Prometheus and Grafana disks
  are left where `managed-csi` puts them, in the AKS node resource group,
  which means a cluster rebuild destroys them. Pinning them to the app
  resource group instead would cost the same (~$1.20/month either way -
  it is the same disk) but adds a static PersistentVolume, a Helm values
  override, a widened RBAC condition and another recovery step, to
  protect 15 days of metrics on a self-healing rolling window.

  Accepted deliberately: snapshot before a *planned* rebuild (see
  BOOTSTRAP.md), and accept the loss for an unplanned one. What made this
  affordable was moving the visitor count out of Prometheus (#75) - it
  was the only number on the site whose loss was actually visible.

Rough floor: well under $50/month at MVP scale.

## Repo layout

Single monorepo (`resume-as-code`), not split source/GitOps repos - the
split's isolation benefit isn't worth the cross-repo overhead for a solo
project, and it's easier for a reviewer to browse one repo.

```
resume-as-code/
├── app/            Go app: serves the site, exposes /metrics and /status
├── controllers/     future Go controllers (phase 3), empty for now
├── infra/          Terraform: envs/prod wires modules/{networking,aks}
├── manifests/       everything Argo CD watches (platform, observability, apps)
├── promql/rules/    recording/alerting rules as code (the only part Argo syncs)
├── promql/tests/    promtool unit tests for those rules
├── bootstrap/        the one imperative script: installs Argo CD
└── .github/workflows/ app-ci, terraform-plan/apply, manifests-lint
```

## GitOps bootstrap and upgrade path

Argo CD is installed once via `bootstrap/install-argocd.sh` (imperative,
break-glass only - re-run only after a full cluster rebuild). Immediately
after, an Argo CD `Application` is added pointing at Argo CD's own
version-pinned Helm chart in `manifests/platform/argocd/` - the
well-known "Argo CD manages itself" pattern. From then on:

- **Upgrades** = bump the chart version in git, open a PR, merge. No
  manual `helm upgrade`.
- **CRDs** need explicit handling - Helm doesn't upgrade existing CRDs
  automatically; enable Argo CD's CRD-management flag or apply CRDs as a
  pre-sync step.
- **Rollback** = `git revert`, same as any other workload.
- **Disaster recovery** = `terraform apply`, then re-run the bootstrap
  script; the root app-of-apps takes over from there. Two manual steps
  remain (the Discord webhook secret, and re-enabling the Grafana share),
  and two things are lost: Prometheus history and Grafana's database. The
  ordered runbook is in [`BOOTSTRAP.md`](BOOTSTRAP.md#recovery-rebuilding-from-a-lost-cluster).

  This bullet used to claim recovery was just the bootstrap script. That
  was true when written and quietly stopped being true - at one point a
  rebuild would also have destroyed the site's DNS target and required a
  code change to restore the Grafana link. Both are fixed; the lesson
  that a documented claim can rot without anything failing is the reason
  this now points at a runbook rather than restating itself.

## App stack

Single Go binary (stdlib `net/http` + `html/template`), not a separate
frontend framework/build pipeline:

- Leverages existing Go experience directly.
- Compiles to a small static binary -> tiny container image (`scratch`/
  distroless), fast blue/green swaps for the phase-3 rollout controller.
- `promhttp` exposes `/metrics` natively - one less integration for the
  observability story.
- Styling via the **Tailwind CDN script tag** - no npm/build toolchain,
  chosen deliberately since web dev/CSS isn't the skill being
  showcased here; time is better spent on the Kubernetes/Go side.
- Build time is injected at compile time via `-ldflags` (not read from
  container filesystem timestamps), so `/status` can report an accurate
  last-deployed time for the homepage stats strip.

Homepage is intentionally short: hero framing, a live stats strip
(visitor count, uptime, last deploy, Grafana link), skills, a brief
experience teaser, and a link out. Full work history lives on a separate
`/resume` page. Reasoning: hiring managers spend seconds per application,
so the homepage should make its point immediately rather than require
scrolling through the full resume first.

## Secrets

Deferred until an actual secret exists (no Key Vault, no CSI driver, no
workload identity wiring yet). AKS already has `oidc_issuer_enabled` and
`workload_identity_enabled` turned on (free to enable, avoids a later
cluster property change) so this is a small addition when needed -
likely triggered by Grafana auth or an Argo CD private repo credential,
not by a calendar phase. Standard-tier Key Vault cost at this scale is
negligible (no subscription fee; ~$0.03 per 10,000 operations).

## Phased roadmap

1. **MVP**: Terraform-provisioned AKS + networking (done). GitHub Actions
   builds the Go app and pushes an image. Argo CD syncs the Deployment +
   ingress-nginx + cert-manager. Site live over HTTPS at a real domain.
2. **Observability**: Prometheus + Grafana as GitOps-managed workloads,
   PromQL checked into `promql/`, a public/read-only Grafana dashboard
   linked from the site.
3. **Custom controller**: leading candidates (may change once phases 1-2
   are live and suggest better signals):
   - **Drift/cost reporter** - diffs live cluster state vs. last Argo CD
     sync, surfaces a cost/drift report as a Grafana panel or site page.
     Spiritual sibling to a prior Azure Firewall DNAT/Traffic Manager
     reconciler.
   - **Progressive rollout controller** - watches new image tags and
     performs a small blue/green promotion of the site itself; doubles
     as a live "watch me deploy" demo.
   - Other ideas on the table: a visitor-activity-reactive controller,
     and a self-service `Project`/`Experiment` CRD in the Crossplane
     spirit for spinning up home-lab demo namespaces.

## Status

Infra (AKS + networking Terraform) is in place. Next up: app/ scaffold,
manifests/ (Argo CD app-of-apps, ingress-nginx, cert-manager), and the
GitHub Actions workflows.
