# Load test results

Gateway overhead under load, measured against the mock upstream so provider latency is fixed and known. All numbers below are real runs (2026-11-25); raw k6 summaries are in `results/<run>/summary-*.json`, and per-step tables in `results/<run>/steps.tsv`.

## Headline

On a **production-like 2-CPU budget** (the Oracle node has 2 OCPUs):

| | Before fix | After fix |
|---|---|---|
| Throughput knee (gateway p99 overhead ≤ 10 ms, 0 errors) | **≈ 2,500 rps** | **≈ 4,000 rps** (+60%) |
| Client p99 at 3,000 rps (50 ms upstream) | 104.9 ms | 63.8 ms |
| Client p95 at 3,000 rps | 62.3 ms | 52.3 ms |
| Gateway overhead p50 at the knee | 0.66 ms | 0.20 ms |
| Gateway overhead p99 at the knee | 7.1 ms | 4.1 ms |

**Bottleneck:** an explicit `http.ResponseController.Flush()` after every non-streaming response, followed by two Redis round trips (token reconcile, cache fill) on the request goroutine. The flush forced a separate `write(2)` per response, and write syscalls were 26% of gateway CPU in the profile.

**Fix:** drop the flush and run the post-response bookkeeping in a goroutine after the handler returns (`internal/server/server.go`). net/http then sends the response in one write.

## Hardware and setup

| | |
|---|---|
| Host | Windows 11 laptop, Intel Core i9-14900HX (24 cores / 32 threads), 32 GB RAM |
| Runtime | Docker Desktop (WSL2, kernel 5.15, cgroup v1), VM with 32 CPUs / 15.5 GB |
| Stack | `compose.yaml` + `loadtest/compose.loadtest.yaml`: gateway, Postgres 17, Redis 7, mock upstream, OTel Collector, Tempo, Prometheus, Grafana, all on one host |
| Gateway | Go 1.27 build, `GOMAXPROCS` from the cgroup limit (`cpus: 2` in the constrained runs) |
| Upstream | `mockupstream -latency 50ms` (fixed, no jitter); streams: 20 chunks × 5 ms |
| Load generator | k6 (`grafana/k6:latest`) in a container on the same Docker network |

The load generator, mock and dependencies share the host with the gateway, so absolute numbers are pessimistic. The 2-CPU cap applies only to the gateway container.

## Methodology

- **Open model:** k6 `constant-arrival-rate`. Requests arrive at the target rate regardless of response times, so queueing shows up as latency instead of being hidden by a closed loop.
- **Steps:** 45 s at each rate, with a pause between steps (`loadtest/run.sh`).
- **Workload:** every `chat` request has a unique prompt and no `temperature`, so it bypasses the cache and goes through rate limiting, routing, the upstream and the ledger.
- **Per-request work:** every request uses a real virtual key (auth cache, the RPM and TPM Lua scripts in Redis), writes a ledger row via COPY, and creates a trace.
- **Two latency views:**
  - **Client latency** (k6 `http_req_duration`) includes the 50 ms upstream.
  - **Gateway overhead** is `tollgate_overhead_seconds` = total handler time − upstream time, read from Prometheus for the same window.
- **Knee:** the highest rate with 0 errors and gateway p99 overhead ≤ 10 ms (the SLO target in DESIGN.md).
- **Profiles:** pprof CPU, captured at the knee via the opt-in `TOLLGATE_PPROF_ADDR`, bound to host loopback. Files are in `results/profile/`.

Reproduce:

```bash
docker compose -p tollgate-lt -f compose.yaml -f loadtest/compose.loadtest.yaml up -d --build
export KEY=$(docker compose -p tollgate-lt run --rm --no-deps tollgate keys create --project lt --rpm 10000000 --tpm 2000000000)
loadtest/run.sh mylabel chat 1000 2000 3000      # GATEWAY_CPUS=4 to change the cap
```

## Results: 2 CPUs, before the fix (`results/baseline-2cpu`)

| target rps | achieved | errors | client p50 | client p95 | client p99 | overhead p50 | overhead p99 |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 500 | 497 | 0% | 52.0 ms | 53.0 ms | 54.3 ms | 0.66 ms | 1.7 ms |
| 1,000 | 996 | 0% | 51.7 ms | 53.6 ms | 74.7 ms | 0.62 ms | 19.4 ms* |
| 1,500 | 1,489 | 0% | 51.7 ms | 53.2 ms | 62.4 ms | 0.70 ms | 2.0 ms |
| 2,000 | 1,986 | 0% | 51.6 ms | 53.6 ms | 61.7 ms | 0.60 ms | 3.8 ms |
| **2,500** | 2,490 | 0% | 51.7 ms | 58.8 ms | 71.3 ms | 0.66 ms | **7.1 ms** |
| 3,000 | 2,978 | 0% | 51.7 ms | 62.3 ms | 104.9 ms | 0.74 ms | 14.6 ms |

\* A one-off outlier: the next step was back to 2.0 ms. It is kept rather than re-run selectively.

## Results: 2 CPUs, after the fix (`results/after-2cpu`)

| mode | target rps | achieved | errors | client p50 | client p95 | client p99 | overhead p50 | overhead p99 |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| chat | 2,000 | 1,989 | 0% | 51.1 ms | 51.8 ms | 61.4 ms | 0.19 ms | 0.7 ms |
| chat | 2,500 | 2,490 | 0% | 51.1 ms | 51.9 ms | 55.8 ms | 0.19 ms | 0.9 ms |
| chat | 3,000 | 2,980 | 0% | 51.1 ms | 52.3 ms | 63.8 ms | 0.19 ms | 1.2 ms |
| chat | 3,500 | 3,486 | 0% | 51.1 ms | 52.8 ms | 63.2 ms | 0.19 ms | 2.2 ms |
| chat | **4,000** | 3,984 | 0% | 51.1 ms | 54.7 ms | 72.1 ms | 0.20 ms | **4.1 ms** |
| chat | 5,000 | 4,966 | 0% | 51.3 ms | 61.3 ms | 81.5 ms | 0.21 ms | 10.7 ms |
| cache hits | 3,000 | 2,992 | 0% | 0.5 ms | 0.8 ms | 3.9 ms | 0.38 ms | 1.5 ms |
| stream | 1,000 | 991 | 0% | 161.6 ms† | 164.9 ms | 168.4 ms | 0.63 ms | 2.0 ms |

† Full stream duration (50 ms + 20 chunks × 5 ms ≈ 150 ms). Streaming TTFT at 1,000 rps, from `tollgate_ttft_seconds`: p50 ≈ 60 ms, p99 ≈ 80 ms, against ≈ 55 ms of mock latency before the first chunk. The histogram's buckets are coarse in this range (40 ms / 80 ms), so these are bucket-interpolated values.

## Results: unconstrained CPU, before the fix (`results/unconstrained`)

The same build as "before", with the gateway allowed all 32 CPUs. This shows the ceiling of a large node.

| target rps | achieved | errors | client p99 | overhead p50 | overhead p99 |
|---:|---:|---:|---:|---:|---:|
| 1,000 | 994 | 0% | 56.7 ms | 0.46 ms | 1.2 ms |
| 2,000 | 1,986 | 0% | 58.2 ms | 0.68 ms | 2.0 ms |
| 3,000 | 2,989 | 0% | 65.8 ms | 0.85 ms | 4.1 ms |
| 4,000 | 3,962 | 0% | 74.6 ms | 1.01 ms | 5.8 ms |
| **5,000** | 4,965 | 0% | 74.3 ms | 1.42 ms | **9.1 ms** |
| 6,000 | 5,947 | 0% | 104.4 ms | 1.70 ms | 13.4 ms |
| 8,000 | 5,301 | 0.43% | 8,123 ms | 91.5 ms | ≥ 500 ms |

The gateway used only about 4.9 of its 32 CPUs at 6,000 rps. The profile there was dominated by scheduler wake-ups (`runtime.findRunnable` 27%, futex 17%), not application code. Past the knee, at 8,000 rps, the ledger buffer overflowed by design: 477 rows were dropped and counted in `tollgate_usage_dropped_total` rather than blocking requests. At a sustained 6,000 rps the ledger wrote 239,039 rows in 40 s and dropped 0.

## How the bottleneck was found

1. **2-CPU profile at 3,000 rps, before the fix** (`results/profile/cpu-2cpu-3000.pb.gz`):
   - `syscall.write`: 26.5% cumulative, almost all from `bufio.(*Writer).Flush`.
   - `net/http.(*ResponseController).Flush`: 7.5% on its own, the explicit flush added so the client got its answer before the bookkeeping.
   - `ratelimit.(*Limiter).Allow`: 9.5%. `ratelimit.(*Limiter).Reconcile`: 8.8%.
   - Access log (`slog` JSON): 5.1%.
2. **Hypothesis:** the explicit flush costs an extra write per response, and the bookkeeping holds the request goroutine and connection for two more Redis round trips.
3. **Change:** remove the flush and move reconcile and cache fill into a goroutine after the handler returns. Inputs are copied first, so the goroutine shares no state with the finished request.
4. **Re-measure:** same steps, same CPU cap. The client-visible tail improved at every comparable rate, and the knee moved from about 2,500 to about 4,000 rps.

**Caveat on the overhead metric:** `tollgate_overhead_seconds` is measured until the handler returns. Before the fix that included the post-flush bookkeeping; after the fix it doesn't. Part of the overhead drop (p50 0.66 → 0.20 ms) is therefore that time leaving the request goroutine, which is the point of the change but also a change in what's measured. The **client-side k6 percentiles** are the like-for-like comparison, and they show the real improvement.

## Next bottlenecks

- **Redis round trips:** two per request (Allow, then Reconcile), about 18% of CPU combined. Folding the previous request's reconcile delta into the next Allow call, in the same script, would remove one round trip per request at the cost of slightly delayed refunds.
- **Access log:** 5% for one JSON line per request. Sampling successful requests would recover most of it.
- **Allocations:** JSON decode of request and response (about 9%) and `mallocgc` (about 9%). Pooled buffers would help.

## A measurement mistake, kept for honesty

The first ramp attempt was discarded. A backgrounded runner that looked dead (Git Bash's `ps` doesn't list it) was still running while a second and third were started. That stacked two or three k6 instances on top of each other and produced a false knee near 2,000 rps. The tables above come from runs with exactly one load generator, verified with `docker ps` before each run.
