# ADR 0003: Token bucket rate limiting, atomically in a Redis Lua script

Status: accepted (2026-11)

## Context

Limits must hold across multiple replicas for both requests/min and tokens/min. Options: fixed window (allows 2x bursts at window edges), sliding-window log (exact, but O(requests) memory per key), token bucket (bursty but bounded, O(1) state).

## Decision

Two token buckets per key (RPM, TPM), checked and updated by one Lua script. The script refills from `redis.call('TIME')`, decides all-or-nothing, and returns Retry-After. Output tokens are unknown up front, so the gateway reserves prompt estimate + max_tokens and reconciles afterwards; the bucket may go into debt.

## Consequences

Redis runs a script without interleaving other commands, so check-and-decrement is atomic across replicas without locks or WATCH retries, in one round trip. Using Redis's clock makes replica clock skew irrelevant. Keys are hash-tagged `{key_id}` so the script stays valid on Redis Cluster. Cost: a Redis round trip per request (~0.2 ms on the same node).
