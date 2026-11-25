# ADR 0008: Package Kubernetes manifests with Kustomize, not Helm

Status: accepted (2026-11)

## Context

Argo CD needs a declarative source. Helm adds templating for reuse across many environments; this project has one cluster.

## Decision

Plain manifests with Kustomize. Upstream components (sealed-secrets, cert-manager) are pinned release manifests referenced as remote resources. CI pins the image with tag@digest in `deploy/tollgate/kustomization.yaml`.

## Consequences

What is in git is what runs: no template rendering to debug, and diffs are reviewable. Shared config files (collector, Prometheus, dashboards) are reused by docker compose and Kustomize configMapGenerator. If multiple environments appear, overlays cover most needs before Helm would.
