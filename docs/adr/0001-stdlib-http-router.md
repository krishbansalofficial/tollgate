# ADR 0001: Use the Go standard library HTTP mux

Status: accepted (2026-11)

## Context

The gateway exposes a handful of routes (`POST /v1/chat/completions`, health, readiness, metrics). Router libraries (chi, gin) add path params, middleware chains and context helpers.

## Decision

Use `net/http.ServeMux`. Since Go 1.22 it matches methods and path patterns (`POST /v1/...`), which covers every route. Middleware is plain `func(http.Handler) http.Handler`.

## Consequences

No dependency to audit or upgrade, and `http.ResponseController` (flush, write deadlines for streaming) works without adapter shims. If the API grows to dozens of parameterised routes, chi is a drop-in upgrade because it uses the same handler types.
