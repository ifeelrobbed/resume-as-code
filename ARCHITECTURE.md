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

### What it actually costs

Roughly **$88/month**, by service:

| Virtual Machines | Storage | Virtual Network | Load Balancer | Bandwidth |
| --- | --- | --- | --- | --- |
| $61.61 | $18.17 | $7.06 | $0.80 | <$0.01 |

This section previously claimed "well under $50/month at MVP scale", which is no longer
true and is worth leaving on the record rather than quietly overwriting. The node pool was
moved to a larger SKU (`Standard_D2as_v7`, 2 vCPU / 8 GiB) because the original could not
hold the workload once Prometheus, Grafana and Argo CD were all running - and the VM line
alone now exceeds what the whole estate was supposed to cost.

The avoided-services list above is still doing its job; the node is simply bigger than
planned. Whether it needs to be is an open question - see #118 for the review, which is
waiting on enough real traffic to size against rather than guessing from an idle cluster.

## Repo layout

Single monorepo (`resume-as-code`), not split source/GitOps repos - the
split's isolation benefit isn't worth the cross-repo overhead for a solo
project, and it's easier for a reviewer to browse one repo.

```
resume-as-code/
├── app/            Go app: serves the site on :8080, metrics on :9090
├── infra/          Terraform: envs/prod wires modules/{networking,aks}
├── manifests/       everything Argo CD watches (platform, observability, apps)
├── promql/rules/    recording/alerting rules as code (the only part Argo syncs)
├── promql/tests/    promtool unit tests for those rules
├── bootstrap/        the one imperative script: installs Argo CD
└── .github/workflows/ app-ci, app-release, manifests-ci, terraform-plan/apply
```

Phase 3's controllers would add a `controllers/` directory. This listing used to
include it as "empty for now", describing a directory that has never existed.

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
- Styling is a single hand-written stylesheet with CSS custom properties and
  self-hosted fonts - no npm, no build step, nothing fetched from a CDN at
  page load. This bullet used to claim Tailwind via a CDN script tag, which
  was the original plan and never what shipped; the reason for writing it
  down still holds, in that web/CSS is not the skill being showcased and a
  toolchain would cost time better spent elsewhere.
- Build time is injected at compile time via `-ldflags` (not read from
  container filesystem timestamps), so `/status` can report an accurate
  last-deployed time for the homepage stats strip.

Homepage is intentionally short: hero framing, a status strip (visitors,
uptime, last deploy, Argo CD sync), an observability strip (req/hr, p95
latency, error rate) with a link to the public Grafana dashboard, skills, a
brief experience teaser, and links out. Full work history lives on a separate
`/resume` page. Reasoning: hiring managers spend seconds per application, so
the homepage should make its point immediately rather than require scrolling
through the full resume first.

Every panel carries a hover/tap tooltip naming the query or source behind its
number, in the spirit of Grafana making a panel's query visible to whoever
opens it. Panels not backed by a query say so rather than implying one.

## Pod security

The `resume-site` namespace enforces the [restricted Pod Security
Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/).
The API server rejects any pod there that runs as root, allows privilege
escalation, keeps capabilities, or omits a seccomp profile. The labels are set
via `managedNamespaceMetadata` on the Argo CD Application, so they are
reconciled like any other config rather than applied by hand.

The pod's `securityContext` and the namespace label do different jobs. The
first describes what the workload asks for; the second is what makes the
cluster refuse anything else. Without the label, deleting the security fields
would be accepted silently.

`enforce`, `warn` and `audit` are all set, which is not belt-and-braces:
`enforce` is evaluated only on Pods, so a non-compliant Deployment would be
accepted and only its pods would fail, buried in a ReplicaSet event. `warn`
and `audit` also evaluate pod-controller resources, so a bad Deployment is
reported at apply time.

Deliberately scoped to `resume-site`. `monitoring` cannot meet the standard -
node-exporter needs host namespaces and hostPort, and Grafana's chart runs as
root - so labelling it would break the observability stack. (`ingress-nginx`
would in fact pass, since dropping `ALL` and adding back only
`NET_BIND_SERVICE` is permitted, but it is left alone as it is upstream
chart-managed.)

## What is exposed to the internet

The app listens on two ports. `:8080` serves the site and is what the Ingress
routes to; `:9090` serves `/metrics` and nothing else, and no Ingress
references it. The Service exposes both so the ServiceMonitor can find the
scrape target, and the NetworkPolicy admits only the `monitoring` namespace to
`:9090`.

Two ports rather than a deny rule on the path, because the Ingress routes `/`
with `pathType: Prefix` - every route on the public listener is reachable, so
the separation has to be structural rather than a filter that a later edit
could undo. It also needs no ingress-controller features; blocking the path
would have meant enabling snippet annotations globally, which ingress-nginx
disables by default for good reason.

`/metrics` was public until this changed. It exposed the Go version and full
runtime detail (free fingerprinting against known CVEs), exact traffic volume,
and `go_goroutines` - which is the signal that would reveal a Slowloris in
progress, so publishing it let an attacker watch their own progress.

**`/status` and `/readyz` stay public, deliberately.** Everything `/status`
returns is already rendered on the homepage: build time, uptime, and the
visitor count. Moving it would hide nothing while making the kubelet probes
depend on the admin port. It is also a reasonable thing for a site like this to
let you curl.

The admin listener is not instrumented. A scrape every 30s would otherwise
appear in the request counters it is reporting, which on a site with a few
dozen real visits a day would swamp them - the same reason the blackbox
exporter's probes are already excluded.

## Secrets

Two separate problems, at different stages.

**Azure credentials: solved, with workload identity.** The app reads and
writes the visitor count in Blob Storage without a credential existing
anywhere - not in the image, not in a Kubernetes Secret, not in this repo.
`infra/envs/prod/app-identity.tf` creates a user-assigned identity and
federates it to the `resume-site` ServiceAccount; the pod receives a
projected token that AKS exchanges for an Azure token at runtime. Access is
scoped to the one blob container, not the storage account.

`oidc_issuer_enabled` and `workload_identity_enabled` are what make that
possible, and were turned on at provisioning time because changing either
later forces a new cluster.

The app asks for `NewWorkloadIdentityCredential` explicitly rather than
`DefaultAzureCredential`. The default walks a chain of credential sources, so
a missing projected token would silently fall back to some other identity;
here that should be a hard failure.

**Kubernetes Secrets: one exists, created by hand.**
`alertmanager-discord-webhook` in `monitoring` holds the URL Alertmanager
posts to. It is created out of band and documented in `BOOTSTRAP.md` - a
webhook URL is a credential, and this repo is public.

**Key Vault and the CSI driver stay deferred.** A single hand-created Secret
does not justify the moving parts, and the case that actually mattered - the
app authenticating to Azure - is already handled without them. Standard-tier
cost at this scale is negligible (no subscription fee; ~$0.03 per 10,000
operations), so this is a judgement about complexity, not money.

Worth recording, since it is the kind of thing that is easy to quietly
overwrite: this section used to predict that Grafana auth or an Argo CD
private repo credential would be what forced a secrets story. Neither was.
Alertmanager needing somewhere to send alerts was.

## Phased roadmap

1. **MVP** — *done*. Terraform-provisioned AKS + networking. GitHub Actions
   builds the Go app and pushes an image. Argo CD syncs the Deployment +
   ingress-nginx + cert-manager. Site live over HTTPS at a real domain.
2. **Observability** — *done*. Prometheus + Grafana as GitOps-managed
   workloads, recording and alerting rules checked into `promql/rules/` and
   unit-tested with promtool, a public/read-only Grafana dashboard linked
   from the site, and Alertmanager routing to Discord.
3. **Custom controller** — not started. Leading candidates (phases 1-2 are
   live now and have not changed the shortlist much):
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

Phases 1 and 2 are live. The site serves from AKS over HTTPS, every number on
it is real, delivery is fully GitOps, and the observability stack alerts to
Discord.

The alerting path is proven end to end: `KubeClientErrors` (a
kube-prometheus-stack rule, severity `warning`) fired for real and was
delivered, so routing, the receiver and the webhook secret all work.

What is genuinely untested:

- **None of this repo's own rules in `promql/rules/` have fired.** They are
  unit-tested against synthetic series and mutation-tested, but nothing has
  yet gone wrong in a way that trips `ResumeSiteDown`,
  `ArgoApplicationOutOfSync` or the rest.
- **The degraded UI states have never rendered from real drift** - the amber
  sync panel is covered by tests and nothing else.
- **Disaster recovery has never been rehearsed end to end.** The runbook in
  BOOTSTRAP.md is written from what each step does, not from having done them
  in sequence against a genuinely lost cluster.

Phase 3 (a custom controller) has not started. Open work is tracked in GitHub
issues rather than here, so this section cannot rot into a stale to-do list -
which is what it had become before this rewrite, still describing app/ and
manifests/ as "next up" weeks after both shipped.
