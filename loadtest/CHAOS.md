# Failure testing

Each scenario was run for real against the compose stack with `loadtest/chaos.sh <scenario>` (2026-11-25). The stack is `compose.yaml` + `loadtest/compose.chaos.yaml`: a primary mock upstream, plus a second mock wired in as the `ollama` provider, which is the configured last-resort fallback (`TOLLGATE_FALLBACK_MODEL=ollama/llama3.2:3b`).

## Summary

| Scenario | Client impact | Behaviour |
|---|---|---|
| Redis down, fail-open (default) | **None**: 20/20 requests 200 | Rate limiting and cache bypassed and logged as degraded; readiness stays 200 |
| Redis down, fail-closed | **20/20 → 503** | Spend protected and availability sacrificed, as configured |
| Primary returns 100% 500s | **None**: 40/40 200 via fallback | Breaker opened after ~2 requests; then the primary is skipped instantly |
| Primary returns 100% 429s | **None**: 40/40 200 via fallback | Same path as 500s |
| Primary returns 30% 500s | **None**: 40/40 200 | 35 served by primary (some after a retry), 5 by fallback |
| Gateway SIGTERM mid-stream | **None**: stream completed | Drain finished the in-flight stream, ledger flushed, clean exit |
| Gateway SIGKILL mid-stream | Stream cut after 8/20 chunks, no `[DONE]` | Ledger row lost (in-memory buffer), as designed for a hard kill |

## 1. Redis down

```
-- fail mode: open; Redis stopped:      20 × 200 (provider mock)
   one request latency: 145 ms          readyz: 200
-- fail mode: closed; Redis stopped:    20 × 503
   one request latency: 109 ms          readyz: 200
-- either mode; Redis back:             5 × 200, without a restart
```

- **Fail-open** is the default ([ADR 0004](../docs/adr/0004-fail-open-default.md)). Requests keep flowing, and each one logs `rate limiter degraded`, so the outage is visible.
- **Fail-closed** returns an OpenAI-shaped 503, `rate limiter unavailable`.
- **Readiness** stays green in both modes, by design. Otherwise a Redis outage would remove every pod from the Service and turn a degraded gateway into a dead one.
- **Recovery** is automatic: go-redis reconnects on the next call.
- **Measured cost:** degraded requests took 109–145 ms instead of ~25 ms. Stopping the container also removes its DNS name, so each call pays a failed lookup plus the 50 ms Redis timeout on both the rate-limit and cache paths.
  - **Possible improvement:** an in-process circuit breaker around Redis would make a degraded request as fast as a healthy one.

## 2. Primary provider failing

The primary mock was restarted with injected failures. The gateway was restarted before each phase, so each phase starts with a closed breaker. 40 requests per phase.

| primary mode | served by | mean attempts | p50 latency | notes |
|---|---|---:|---:|---|
| `-error-rate 1` (500s) | 40 × ollama (fallback) | 1.13 | 21 ms | breaker `open` logged |
| `-429-rate 1` (429s) | 40 × ollama (fallback) | 1.13 | 21 ms | breaker `open` logged |
| `-error-rate 0.3` | 35 × mock, 5 × ollama | 1.17 / 3.00 | 21 / 127 ms | fallback requests paid 2 failed attempts + backoff |

- **Breaker:** with 100% failures it opened after about 2 requests, the minimum of 5 failed attempts at 2 attempts per request plus retries. After that, the primary is skipped without a network call, which is why mean attempts is about 1.1 and p50 is unchanged.
- **Partial failure:** at 30% failures, retries absorb most errors (35/40 still served by the primary). Requests that fail twice fall back.
- **Client impact:** none. The client saw zero errors in every phase.
- **Visibility:** because fallback hides the failure from clients, the signals are:
  - `tollgate_breaker_state` and the `TollgateBreakerOpen` alert;
  - the `X-Tollgate-Provider` response header;
  - the ledger's `provider` and `attempts` columns.

## 3. Gateway killed mid-stream

The stream is 20 chunks × 400 ms (~8 s). The gateway container is signalled 3 s in.

```
-- SIGTERM at ~3 s: curl exit=0,  content chunks=20, ended with [DONE]=yes, ledger rows: 1
   shutdown: draining (delay 2s) … shutdown: complete usage_written=1 usage_dropped=0   (+5.2 s)
-- SIGKILL at ~3 s: curl exit=18, content chunks=8,  ended with [DONE]=no,  ledger rows: 0
```

**SIGTERM** (what Kubernetes sends on rollout or eviction):
- Readiness flips to 503 and new connections drain for 2 s (`TOLLGATE_SHUTDOWN_DELAY`).
- `http.Server.Shutdown` then waits for the open stream to finish.
- The ledger flushes and the process exits. That took 5.2 s, well inside `terminationGracePeriodSeconds: 35`.

**SIGKILL** (OOM kill, node loss):
- The TCP connection drops. The client sees a truncated stream with no `[DONE]` (curl exit 18), which OpenAI SDKs surface as a connection error.
- The ledger row for that request is lost with the in-memory buffer. This is the trade-off in [ADR 0007](../docs/adr/0007-async-lossy-ledger.md); a durable queue fixes it at scale.

## Harness bugs found and fixed while running these

- **Container-name conflict:** replacing the primary mock failed on a container-name conflict, so the first provider run tested only the "100% 500s" mode three times. Fixed and re-run; the table above is from the fixed run.
- **Shared DNS alias:** a `docker compose run` mock container survived `down` and shared the `mockupstream` alias, so requests were split between two differently configured mocks. The first SIGKILL run hit the fast one and "completed" before the kill. Fixed with `--remove-orphans` plus explicit cleanup, then re-run.
