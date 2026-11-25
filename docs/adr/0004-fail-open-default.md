# ADR 0004: Rate limiter fails open by default when Redis is down

Status: accepted (2026-11)

## Context

If Redis is unreachable the limiter cannot decide. Fail closed (reject) protects spend and quotas; fail open (admit) protects availability.

## Decision

Configurable via `TOLLGATE_RATELIMIT_FAIL_MODE`, default `open`. Redis calls have a 50 ms timeout. Readiness does not depend on Redis.

## Consequences

A cache/limiter outage degrades the gateway instead of taking every API consumer down. Spend remains bounded by provider-side limits, per-project budgets and alerting on the `rate limiter degraded` signal. Deployments where spend control matters more than uptime set `closed` and get 503s during the outage (measured in loadtest/CHAOS.md).
