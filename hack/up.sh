#!/usr/bin/env bash
# Bring up the whole project on the cluster KUBECONFIG points at:
# operator (CRD, RBAC, NetworkPolicy, Deployment, UI Service), the sample
# ClusterView and the 20-pod demo workload. Safe to re-run.
#
# Ends by opening the UI (hack/ui.sh). Loads .env itself if it isn't sourced.
#
# Usage:
#   make up                  # deploy the pinned release image (hack/IMAGE), open the UI
#   make up IMG=<image>      # deploy another image that's already pushed
#   make up NO_UI=1          # deploy only
set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=hack/env.sh
source hack/env.sh

NS=clusterview-operator-system
DEPLOY=clusterview-operator-controller-manager
: "${IMG:?IMG must be set (the Makefile sets it)}"
: "${KUBECTL:=kubectl}"
: "${MAKE:=make}"

step() { printf '\n==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

step "Checking cluster access"
[[ -n "${KUBECONFIG:-}" ]] || die "KUBECONFIG is not set. Copy .env.example to .env and fill it in"
ctx=$("$KUBECTL" config current-context) || die "kubectl has no current context"
"$KUBECTL" get nodes >/dev/null || die "can't reach the cluster in context $ctx"
echo "context: $ctx"
echo "image:   $IMG"
# Nothing is built here: the image is already in GHCR, so bring-up needs no
# podman or push access. Maintainers publish new images with `make release`.


# The namespace must exist before the pull secret, and the secret before the pod
# pulls. Applying the namespace on its own is idempotent.
step "Ensuring namespace and image pull secret"
"$KUBECTL" create namespace "$NS" --dry-run=client -o yaml | "$KUBECTL" apply -f - >/dev/null
if "$KUBECTL" -n "$NS" get secret ghcr-pull >/dev/null 2>&1; then
  echo "ghcr-pull secret already exists"
elif [[ -n "${GHCR_PULL_TOKEN:-}" ]]; then
  # The token goes straight from the env into the API call and is never printed.
  "$KUBECTL" -n "$NS" create secret docker-registry ghcr-pull \
    --docker-server=ghcr.io --docker-username="${GHCR_USER:-samkeet}" \
    --docker-password="$GHCR_PULL_TOKEN" >/dev/null
  echo "created ghcr-pull secret from GHCR_PULL_TOKEN"
else
  die "no ghcr-pull secret in $NS and GHCR_PULL_TOKEN is not set.
Set GHCR_PULL_TOKEN (a read:packages-only PAT) in .env, or create the secret yourself:
  kubectl -n $NS create secret docker-registry ghcr-pull --docker-server=ghcr.io \\
    --docker-username=<github-user> --docker-password=<read:packages PAT>"
fi

step "Deploying the operator"
"$MAKE" deploy IMG="$IMG"
# make deploy pins the image in config/manager/kustomization.yaml. Restore it so a
# deploy never leaves a dirty tree behind.
git checkout -- config/manager/kustomization.yaml 2>/dev/null || true
"$KUBECTL" -n "$NS" rollout status "deploy/$DEPLOY" --timeout=180s

step "Applying the sample ClusterView"
"$KUBECTL" apply -f config/samples/observe_v1alpha1_clusterview.yaml

step "Applying the demo workload (20 pods)"
"$MAKE" demo

step "Waiting for the ClusterView status"
for _ in $(seq 1 30); do
  nodes=$("$KUBECTL" get clusterview cluster -o jsonpath='{.status.nodeCount}' 2>/dev/null || true)
  [[ -n "$nodes" ]] && break
  sleep 2
done
"$KUBECTL" get clusterview

if [[ "${NO_UI:-0}" == 1 ]]; then
  printf '\nClusterView is up. Open the UI with: make ui\n'
  exit 0
fi

step "Opening the UI"
exec hack/ui.sh
