#!/usr/bin/env bash
# One-time cluster bootstrap after `terraform apply`. Installs Argo CD and
# applies the app-of-apps root. The repo is public, so Argo CD reads it over
# HTTPS with no credentials. Everything after this arrives through git.
#
#   infra/bootstrap.sh ubuntu@<public_ip>
#
# Needs locally: ssh, kubectl (only for client-side YAML generation).
set -euo pipefail

TARGET=${1:?usage: bootstrap.sh <ssh-target>}
ARGO_VERSION=v3.6.3
ROOT=$(cd "$(dirname "$0")/.." && pwd)

# All cluster commands run on the node: the API server is never exposed.
remote() { ssh -o BatchMode=yes "$TARGET" sudo k3s kubectl "$@"; }

echo "==> waiting for k3s (cloud-init can take a few minutes)"
until remote get nodes >/dev/null 2>&1; do sleep 10; done
remote wait --for=condition=Ready node --all --timeout=300s

echo "==> installing Argo CD $ARGO_VERSION"
kubectl create namespace argocd --dry-run=client -o yaml | remote apply -f -
remote apply -n argocd --server-side -f "https://raw.githubusercontent.com/argoproj/argo-cd/$ARGO_VERSION/manifests/install.yaml"
remote -n argocd rollout status deploy/argocd-repo-server --timeout=300s

echo "==> applying the root application"
remote apply -f - <"$ROOT/deploy/argocd/root.yaml"

cat <<EOF

Done. Argo CD is now syncing deploy/apps. Next:
  1. scripts/seal-secrets.sh $TARGET      (creates and commits encrypted secrets)
  2. Argo CD UI:  ssh -L 8080:localhost:8080 $TARGET 'sudo k3s kubectl -n argocd port-forward svc/argocd-server 8080:443'
     password:    ssh $TARGET sudo k3s kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
EOF
