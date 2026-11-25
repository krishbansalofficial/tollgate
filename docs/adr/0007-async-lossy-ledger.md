# ADR 0007: Asynchronous usage ledger that drops rather than blocks

Status: accepted (2026-11)

## Context

Every request needs a ledger row (tokens, cost, latency). Writing synchronously adds Postgres latency and couples availability to the database.

## Decision

Handlers do a non-blocking send into a buffered channel (10,000 rows). One writer batches by size (500) or time (1 s) and inserts with COPY, retrying 3 times. If the buffer is full or retries fail, rows are dropped and counted. Shutdown drains HTTP first, then closes the channel and flushes.

## Consequences

The request path never waits on Postgres. The cost is possible ledger loss during a long database outage or a SIGKILL. It is visible as `tollgate_usage_dropped_total`, with an alert. At 100x scale, a durable queue (Kafka/Redpanda) would replace the channel.
