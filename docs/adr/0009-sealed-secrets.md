# ADR 0009: Sealed Secrets for secrets in GitOps

Status: accepted (2026-11)

## Context

Secrets must never be committed in plain text, yet Argo CD deploys from git. External secret stores (Vault, cloud KMS) cost money or another service to run.

## Decision

Use Bitnami Sealed Secrets. `scripts/seal-secrets.sh` builds secrets from hidden prompts, encrypts them with the cluster's public key, and writes SealedSecret files to commit.

## Consequences

$0 and fully GitOps: only the in-cluster controller can decrypt. Losing the controller's private key means re-sealing all secrets (back it up). Rotation is a re-seal and commit.
