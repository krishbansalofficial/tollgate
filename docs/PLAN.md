# Tollgate: Phased plan

Every phase ends with: tests green (`go test -race ./...`), lint clean, the docs it touched updated, and a commit per meaningful step. Estimates assume ~3 focused hours per day.

## Phase 1: Skeleton, one provider, keys, CI (≈ 3–4 days)

1. `go mod init`, layout (`cmd/tollgate`, `cmd/mockupstream`, `internal/...`), Makefile.
2. `internal/config`: env parsing, fail fast. `internal/api`: OpenAI request/response types.
3. HTTP server: routes, OpenAI error envelope, request ID, slog JSON, `/healthz`, `/readyz`, graceful shutdown.
4. `cmd/mockupstream`: OpenAI-compatible, with latency, error, and 429 injection.
5. `internal/providers/openaicompat`, used for Groq and the mock. Non-streaming only.
6. Postgres + goose migrations (`projects`, `api_keys`); `tollgate migrate`, `tollgate keys create`.
7. `internal/auth`: SHA-256 lookup, TTL cache, negative cache, singleflight.
8. docker-compose (Postgres, Redis, mockupstream, tollgate); Dockerfile (multi-stage, distroless, nonroot).
9. GitHub Actions: lint, `test -race`, build.

**Exit:** `curl` with a virtual key → Groq answer; bad key → 401; CI green.

## Phase 2: Rate limiter, cache, second provider, resilience (≈ 4–5 days)

1. Lua token bucket (RPM + TPM, all-or-nothing), reconcile/refund, fail-mode flag, 429 + Retry-After.
2. Testcontainers Redis integration tests and a 200-goroutine concurrency test.
3. Exact-match cache: canonical key, temperature 0 only, `X-Tollgate-Cache`.
4. Anthropic adapter (translation and golden tests).
5. Model catalog YAML, router policies: pinned, cheapest, and fastest (EWMA).
6. Retries (full jitter, retry budget, Retry-After), per-provider circuit breaker, fallback chain.

**Exit:** the mock returns 100% 500s and requests still succeed through the fallback; the rate limit holds under concurrency.

## Phase 3: Usage ledger, tracing, metrics, dashboards (≈ 3 days)

1. `usage_events` migration, buffered channel, batched `CopyFrom` writer, drop metric, shutdown flush test.
2. Prometheus metrics (§3.9 of DESIGN), including the overhead histogram.
3. OTel tracing, with trace IDs in logs; collector, Tempo, Prometheus, and Grafana in compose.
4. Grafana dashboards as JSON in the repo: Overview, Providers, Spend.

**Exit:** one request is visible as a trace, a ledger row, and dashboard panels.

## Phase 4: Infra and live deploy (≈ 4–5 days, the most calendar risk)

1. Terraform `modules/{network,k3s-node}` + `envs/oracle`; 2 OCPU / 12 GB cap enforced by validation.
2. Kustomize manifests: tollgate, Redis, Postgres, collector, Tempo, Prometheus, Grafana. cert-manager + Traefik ingress.
3. Sealed Secrets; Argo CD app-of-apps watching `deploy/`.
4. CI: multi-arch image (amd64 + arm64) → GHCR, Trivy scan, SBOM (syft), bump the image tag in `deploy/` → Argo CD sync.
5. Migrations as an Argo CD PreSync Job.

**Exit:** a `git push` to main rolls out a new image on Oracle with no manual step; `https://<domain>/healthz` is green.

**Needs from you:** an Oracle Cloud account (card verification), the domain decision, and Groq/Anthropic/Gemini API keys. I never see keys in plain text in the repo.

## Phase 5: Streaming, Gemini, Ollama, load and chaos (≈ 4–5 days)

1. SSE streaming across all adapters, TTFT, mid-stream failure handling, client-disconnect cancellation.
2. Gemini adapter; Ollama via the openaicompat adapter over Tailscale as the last-resort fallback.
3. k6 scripts against the mock: ramp to find the knee, record p50/p95/p99 overhead. Profile with pprof, fix the bottleneck, re-measure → `loadtest/RESULTS.md`.
4. Chaos: Redis down (both fail modes), primary 429/500 storm, pod kill mid-stream → written results.

## Phase 6: Docs polish (≈ 2 days)

README in the requested order, architecture SVG, ADRs finalised, real-traffic section once my apps run on it.

## Stretch (only after Phase 6)

Hard budgets + alerts → SLO + burn-rate alerts → semantic cache (pgvector, logged false-hit rate) → multi-replica deployment proof → Next.js admin dashboard.

## Local tooling gaps found today

- Go 1.27 ✓, kubectl ✓
- Docker Desktop is installed but not running. Testcontainers and compose need it from Phase 1 step 6.
- k6 and Terraform are not installed. They are needed in Phases 4 and 5: `winget install k6.k6 Hashicorp.Terraform`.
