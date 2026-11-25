# Architecture decision records

- [ADR 0001: Use the Go standard library HTTP mux](0001-stdlib-http-router.md)
- [ADR 0002: Store virtual keys as SHA-256, not bcrypt/argon2](0002-sha256-key-hashing.md)
- [ADR 0003: Token bucket rate limiting, atomically in a Redis Lua script](0003-token-bucket-in-redis-lua.md)
- [ADR 0004: Rate limiter fails open by default when Redis is down](0004-fail-open-default.md)
- [ADR 0005: Talk to providers with raw net/http, not vendor SDKs](0005-raw-http-providers.md)
- [ADR 0006: Export metrics with the Prometheus client directly](0006-prometheus-client-direct.md)
- [ADR 0007: Asynchronous usage ledger that drops rather than blocks](0007-async-lossy-ledger.md)
- [ADR 0008: Package Kubernetes manifests with Kustomize, not Helm](0008-kustomize-over-helm.md)
- [ADR 0009: Sealed Secrets for secrets in GitOps](0009-sealed-secrets.md)
