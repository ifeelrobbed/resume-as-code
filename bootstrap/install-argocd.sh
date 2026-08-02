#!/usr/bin/env bash
# Imperative, break-glass only: run once against a fresh cluster to get
# Argo CD running, then it manages its own upgrades from manifests/
# (see ARCHITECTURE.md "GitOps bootstrap and upgrade path"). Re-run only
# after a full cluster rebuild.
set -euo pipefail

ARGOCD_VERSION="v3.4.6"
INFRA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../infra/envs/prod" && pwd)"

RESOURCE_GROUP="${RESOURCE_GROUP:-$(terraform -chdir="$INFRA_DIR" output -raw resource_group_name)}"
CLUSTER_NAME="${CLUSTER_NAME:-$(terraform -chdir="$INFRA_DIR" output -raw cluster_name)}"

echo "Fetching credentials for $CLUSTER_NAME in $RESOURCE_GROUP..."
az aks get-credentials -g "$RESOURCE_GROUP" -n "$CLUSTER_NAME" --overwrite-existing

kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -
# --server-side: the applicationsets.argoproj.io CRD is too large for
# client-side apply's last-applied-configuration annotation (262144-byte cap).
kubectl apply --server-side --force-conflicts -n argocd \
  -f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"

echo "Waiting for argocd-server to become ready..."
kubectl -n argocd rollout status deployment/argocd-server --timeout=300s

echo "Applying root app-of-apps (manifests/root.yaml)..."
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kubectl apply -f "$REPO_ROOT/manifests/root.yaml"

echo
echo "Argo CD is up. Initial admin password:"
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
echo
echo "(delete the argocd-initial-admin-secret once you've logged in and changed it)"
