# resume-as-code

My resume, built and operated the way I build production systems: Terraform-provisioned
AKS, GitOps delivery, and a live observability stack — with a Go app that is both the site
and a running demo of the platform under it.

**Live: [robertjcameron.com](https://robertjcameron.com)**

[![app-ci](https://github.com/ifeelrobbed/resume-as-code/actions/workflows/app-ci.yml/badge.svg?branch=master)](https://github.com/ifeelrobbed/resume-as-code/actions/workflows/app-ci.yml)
[![manifests-ci](https://github.com/ifeelrobbed/resume-as-code/actions/workflows/manifests-ci.yml/badge.svg?branch=master)](https://github.com/ifeelrobbed/resume-as-code/actions/workflows/manifests-ci.yml)
[![terraform-apply](https://github.com/ifeelrobbed/resume-as-code/actions/workflows/terraform-apply.yml/badge.svg?branch=master)](https://github.com/ifeelrobbed/resume-as-code/actions/workflows/terraform-apply.yml)

## What it is

The homepage shows live numbers — visitors, uptime, last deploy, Argo CD sync state,
request rate, p95 latency, error rate. Every one is real, read from Prometheus or Azure
Blob Storage at request time. Hover or tap any panel and it names the query or source
behind it.

Push to `master` builds an image, opens and merges its own manifest bump, and Argo CD
rolls it out. Nothing is applied by hand.

## Stack

| | |
| --- | --- |
| **Cloud** | Azure — AKS, VNet/NSG, Blob Storage, workload identity |
| **IaC** | Terraform, remote state, OIDC-authenticated CI with a plan/apply split |
| **Delivery** | Argo CD app-of-apps, self-managing; GitHub Actions |
| **Runtime** | Go (stdlib `net/http` + `html/template`), distroless image |
| **Observability** | Prometheus, Grafana, Alertmanager → Discord, blackbox probes |
| **Networking** | ingress-nginx, cert-manager (Let's Encrypt), Calico NetworkPolicy |

## Worth a look

- **No credentials anywhere.** The app reads and writes Blob Storage through workload
  identity — no secret in the image, in a Kubernetes Secret, or in this repo.
  ([Secrets](ARCHITECTURE.md#secrets))
- **Alerting rules are unit-tested.** `promtool` tests run in CI against synthetic series,
  because these alerts have never fired in anger and untested alerts are untested code.
  ([promql/tests](promql/tests))
- **The namespace enforces restricted Pod Security**, so a non-compliant pod is rejected at
  admission rather than merely discouraged by a `securityContext` that could be deleted.
  ([Pod security](ARCHITECTURE.md#pod-security))
- **The visitor count survives a cluster rebuild.** It moved out of Prometheus — a rolling
  window that reset whenever the TSDB was lost — into a blob with optimistic concurrency.
- **Cost is a design constraint**, not an afterthought: no Azure Firewall, no Application
  Gateway, no managed Prometheus, one node pool.
  ([Cost decisions](ARCHITECTURE.md#cost-conscious-decisions))

## Docs

| | |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | System design, trade-offs, and what is deliberately *not* here |
| [BOOTSTRAP.md](BOOTSTRAP.md) | Out-of-band setup, plus the recovery runbook for a lost cluster |
| [infra/README.md](infra/README.md) | Terraform layout and usage |

Design decisions live in commit messages and PR descriptions rather than a changelog —
`git log` is the record of why things are the way they are.
