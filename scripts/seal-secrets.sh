#!/usr/bin/env bash
# Creates the cluster's Secrets, encrypts them with the cluster's
# sealed-secrets public key, and writes SealedSecret files that are safe to
# commit. Plaintext never touches disk or git.
#
#   scripts/seal-secrets.sh ubuntu@<public_ip>
#
# First run: generates the Postgres password and seals everything.
# Later runs rotate keys: a value left blank keeps its current in-cluster
# value (read over ssh, never printed or written to disk), and the Postgres
# password is always kept, because the existing database still uses it.
# FORCE=1 starts over with a new Postgres password (fresh database only).
#
# Prompts for values (hidden input) so they don't land in shell history.
# Needs locally: ssh, kubectl, kubeseal, openssl.
set -euo pipefail

TARGET=${1:?usage: seal-secrets.sh <ssh-target>}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
APP_OUT=$ROOT/deploy/tollgate/sealed-secrets.yaml
OBS_OUT=$ROOT/deploy/observability/sealed-secrets.yaml

rotate=0
[[ -e $APP_OUT && ${FORCE:-} != 1 ]] && rotate=1
blank="skip"
((rotate)) && blank="keep current"

ask() { # ask VAR "prompt": read a value with hidden input unless already set
  local var=$1
  if [[ -z ${!var:-} ]]; then read -rsp "$2: " "${var?}"; echo; fi
}
ask GROQ_API_KEY "Groq API key (Enter to $blank)"
ask ANTHROPIC_API_KEY "Anthropic API key (Enter to $blank)"
ask GEMINI_API_KEY "Gemini API key (Enter to $blank)"
# Catch copy-paste slips (a label character glued on, a stray space) before
# they get sealed: the provider would only say "invalid key" at request time.
check() { # check VAR prefix name
  local v=${!1:-}
  [[ -z $v ]] && return 0
  [[ $v == "$2"* ]] || { echo "$3 key must start with '$2' (it starts with '${v:0:3}…'); re-run and paste only the key" >&2; exit 1; }
  [[ $v != *[[:space:]]* ]] || { echo "$3 key contains whitespace; re-run and paste only the key" >&2; exit 1; }
}
check ANTHROPIC_API_KEY sk-ant- Anthropic
check GROQ_API_KEY gsk_ Groq
[[ ${GEMINI_API_KEY:-} != *[[:space:]]* ]] || { echo "Gemini key contains whitespace" >&2; exit 1; }

if ((rotate)); then
  ask GRAFANA_PASSWORD "Grafana admin password (Enter to keep current)"
else
  ask GRAFANA_PASSWORD "Grafana admin password"
  [[ -n $GRAFANA_PASSWORD ]] || { echo "a Grafana password is required on first run" >&2; exit 1; }
fi

# current KEY: one value of the live tollgate-secrets ("" if unset). Never printed.
# One quoted string: ssh re-parses its args through the remote shell.
current() {
  ssh -o BatchMode=yes "$TARGET" "sudo k3s kubectl -n tollgate get secret tollgate-secrets -o 'jsonpath={.data.$1}'" | base64 -d
}

cert=$(mktemp)
trap 'rm -f "$cert" "$APP_OUT.tmp" "$OBS_OUT.tmp"' EXIT
echo "==> fetching the cluster's sealing certificate"
ssh -o BatchMode=yes "$TARGET" "sudo k3s kubectl -n kube-system get secret \
  -l sealedsecrets.bitnami.com/sealed-secrets-key=active \
  -o 'jsonpath={.items[0].data.tls\.crt}'" | base64 -d >"$cert"
[[ -s $cert ]] || { echo "could not fetch the sealing certificate" >&2; exit 1; }

seal() { kubeseal --cert "$cert" --format yaml; }

if ((rotate)); then
  echo "==> rotating: keeping the Postgres password and any key left blank"
  pg=$(current postgres-password)
  [[ -n $pg ]] || { echo "could not read the current Postgres password; refusing to mint a new one (FORCE=1 only for a fresh database)" >&2; exit 1; }
  [[ -n $GROQ_API_KEY ]] || GROQ_API_KEY=$(current groq-api-key)
  [[ -n $ANTHROPIC_API_KEY ]] || ANTHROPIC_API_KEY=$(current anthropic-api-key)
  [[ -n $GEMINI_API_KEY ]] || GEMINI_API_KEY=$(current gemini-api-key)
else
  pg=$(openssl rand -hex 24)
fi

args=(--from-literal=postgres-password="$pg"
  --from-literal=database-url="postgres://tollgate:$pg@postgres:5432/tollgate?sslmode=disable")
[[ -n $GROQ_API_KEY ]] && args+=(--from-literal=groq-api-key="$GROQ_API_KEY")
[[ -n $ANTHROPIC_API_KEY ]] && args+=(--from-literal=anthropic-api-key="$ANTHROPIC_API_KEY")
[[ -n $GEMINI_API_KEY ]] && args+=(--from-literal=gemini-api-key="$GEMINI_API_KEY")

# The GHCR image is public, so no pull secret is needed.
kubectl create secret generic tollgate-secrets -n tollgate "${args[@]}" --dry-run=client -o yaml | seal >"$APP_OUT.tmp"
if [[ -n $GRAFANA_PASSWORD ]]; then
  kubectl create secret generic grafana-admin -n observability \
    --from-literal=password="$GRAFANA_PASSWORD" --dry-run=client -o yaml | seal >"$OBS_OUT.tmp"
fi

# Only publish complete output: a failed seal must not leave a file that
# blocks the next run or gets committed half-written.
mv "$APP_OUT.tmp" "$APP_OUT"
[[ -e $OBS_OUT.tmp ]] && mv "$OBS_OUT.tmp" "$OBS_OUT"

# Reference the files from their kustomizations (idempotent).
for k in "$ROOT/deploy/tollgate/kustomization.yaml" "$ROOT/deploy/observability/kustomization.yaml"; do
  grep -q '^  - sealed-secrets.yaml' "$k" || sed -i 's/^resources:$/resources:\n  - sealed-secrets.yaml/' "$k"
done

echo "==> wrote $APP_OUT"
[[ -n $GRAFANA_PASSWORD ]] && echo "==> wrote $OBS_OUT"
echo "Commit and push; Argo CD applies them and the controller decrypts in-cluster."
echo "Pods read keys at startup: after the sync, restart them with"
echo "  ssh $TARGET sudo k3s kubectl -n tollgate rollout restart deploy/tollgate"
