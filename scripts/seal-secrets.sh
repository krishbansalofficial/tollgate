#!/usr/bin/env bash
# Creates the cluster's Secrets, encrypts them with the cluster's
# sealed-secrets public key, and writes SealedSecret files that are safe to
# commit. Plaintext never touches disk or git.
#
#   scripts/seal-secrets.sh ubuntu@<public_ip>
#
# Prompts for values (hidden input) so they don't land in shell history.
# Needs locally: ssh, kubectl, kubeseal, openssl.
set -euo pipefail

TARGET=${1:?usage: seal-secrets.sh <ssh-target>}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
APP_OUT=$ROOT/deploy/tollgate/sealed-secrets.yaml
OBS_OUT=$ROOT/deploy/observability/sealed-secrets.yaml

# Regenerating would mint a new Postgres password while the existing
# database keeps the old one, breaking the app. Require an explicit opt-in.
if [[ -e $APP_OUT && ${FORCE:-} != 1 ]]; then
  echo "$APP_OUT exists; rerun with FORCE=1 only for a fresh database." >&2
  exit 1
fi

ask() { # ask VAR "prompt": read a value with hidden input unless already set
  local var=$1
  if [[ -z ${!var:-} ]]; then read -rsp "$2: " "${var?}"; echo; fi
}
ask GROQ_API_KEY "Groq API key (Enter to skip)"
ask ANTHROPIC_API_KEY "Anthropic API key (Enter to skip)"
ask GEMINI_API_KEY "Gemini API key (Enter to skip)"
ask GRAFANA_PASSWORD "Grafana admin password"

cert=$(mktemp)
trap 'rm -f "$cert"' EXIT
echo "==> fetching the cluster's sealing certificate"
ssh -o BatchMode=yes "$TARGET" sudo k3s kubectl -n kube-system get secret \
  -l sealedsecrets.bitnami.com/sealed-secrets-key=active \
  -o 'jsonpath={.items[0].data.tls\.crt}' | base64 -d >"$cert"

seal() { kubeseal --cert "$cert" --format yaml; }
pg=$(openssl rand -hex 24)

args=(--from-literal=postgres-password="$pg"
  --from-literal=database-url="postgres://tollgate:$pg@postgres:5432/tollgate?sslmode=disable")
[[ -n $GROQ_API_KEY ]] && args+=(--from-literal=groq-api-key="$GROQ_API_KEY")
[[ -n $ANTHROPIC_API_KEY ]] && args+=(--from-literal=anthropic-api-key="$ANTHROPIC_API_KEY")
[[ -n $GEMINI_API_KEY ]] && args+=(--from-literal=gemini-api-key="$GEMINI_API_KEY")

# The GHCR image is public, so no pull secret is needed.
kubectl create secret generic tollgate-secrets -n tollgate "${args[@]}" --dry-run=client -o yaml | seal >"$APP_OUT"

kubectl create secret generic grafana-admin -n observability \
  --from-literal=password="$GRAFANA_PASSWORD" --dry-run=client -o yaml | seal >"$OBS_OUT"

# Reference the files from their kustomizations (idempotent).
for k in "$ROOT/deploy/tollgate/kustomization.yaml" "$ROOT/deploy/observability/kustomization.yaml"; do
  grep -q '^  - sealed-secrets.yaml' "$k" || sed -i 's/^resources:$/resources:\n  - sealed-secrets.yaml/' "$k"
done

echo "==> wrote $APP_OUT and $OBS_OUT"
echo "Commit and push them; Argo CD applies them and the controller decrypts in-cluster."
