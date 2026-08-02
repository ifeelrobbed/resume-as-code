#!/usr/bin/env bash
# Imperative, break-glass only: run once against a fresh cluster to get
# Argo CD running, then it manages its own upgrades from manifests/
# (see ARCHITECTURE.md "GitOps bootstrap and upgrade path"). Re-run only
# after a full cluster rebuild.
#
# Installs via `helm install` directly (matching manifests/platform/argocd's
# chart+version+values exactly) rather than the static install.yaml manifests.
# Bootstrapping with the same Helm release from day one means Argo CD's own
# self-management Application finds zero drift on its first sync - no
# migration step, so none of Kubernetes' immutable Deployment/StatefulSet
# selector fields ever need to change out from under a running resource.
set -euo pipefail

ARGOCD_CHART_VERSION="10.2.2" # app version v3.4.6
INFRA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../infra/envs/prod" && pwd)"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

RESOURCE_GROUP="${RESOURCE_GROUP:-$(terraform -chdir="$INFRA_DIR" output -raw resource_group_name)}"
CLUSTER_NAME="${CLUSTER_NAME:-$(terraform -chdir="$INFRA_DIR" output -raw cluster_name)}"

echo "Fetching credentials for $CLUSTER_NAME in $RESOURCE_GROUP..."
az aks get-credentials -g "$RESOURCE_GROUP" -n "$CLUSTER_NAME" --overwrite-existing

helm repo add argo https://argoproj.github.io/argo-helm >/dev/null
helm repo update argo >/dev/null

# Reuse the exact Helm values manifests/platform/argocd/application.yaml
# declares, so this bootstrap install and Argo CD's later self-sync can
# never drift apart.
VALUES_FILE="$(mktemp)"
trap 'rm -f "$VALUES_FILE"' EXIT
python3 -c "
import yaml
with open('$REPO_ROOT/manifests/platform/argocd/application.yaml') as f:
    app = yaml.safe_load(f)
print(app['spec']['source']['helm']['values'])
" > "$VALUES_FILE"

echo "Installing Argo CD (chart $ARGOCD_CHART_VERSION) via Helm..."
helm upgrade --install argocd argo/argo-cd \
  --version "$ARGOCD_CHART_VERSION" \
  --namespace argocd --create-namespace \
  -f "$VALUES_FILE" \
  --wait --timeout 5m

echo "Applying root app-of-apps (manifests/root.yaml)..."
kubectl apply -f "$REPO_ROOT/manifests/root.yaml"

echo
echo "Argo CD is up. Initial admin password:"
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
echo
echo "(delete the argocd-initial-admin-secret once you've logged in and changed it)"
