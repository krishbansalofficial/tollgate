# ADR 0006: Export metrics with the Prometheus client directly

Status: accepted (2026-11)

## Context

OpenTelemetry can carry metrics too (SDK -> collector -> Prometheus).

## Decision

Traces go through OpenTelemetry; metrics use `prometheus/client_golang` on a separate port, scraped by Prometheus.

## Consequences

Fewer moving parts on the metrics path (a collector outage does not blind alerting), and precise histogram buckets for sub-millisecond overhead. Label values come only from bounded sets, so cardinality is safe.
