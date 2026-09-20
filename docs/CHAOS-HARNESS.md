# Deterministic chaos and latency harness

This suite is intentionally provider-free. Its default path must run without API keys, network access, wall-clock sleeps longer than small scheduling bounds, or paid services.

## Coverage matrix

| Failure surface | Deterministic assertion |
|---|---|
| Scheduler lease | second gateway cannot own the same scheduler store |
| Scheduler misfire | skip/fire-once/grace policies coalesce downtime deterministically |
| DST fallback | repeated local wall-clock instant does not double-fire |
| Timeout/cancellation | executor observes context cancellation and run reaches terminal state |
| Concurrency | active executions never exceed configured cap |
| Idempotency | scheduled execution key is derived from job ID + scheduled instant |
| Trigger restart | claimed processing delivery is requeued and executes once after restart |
| Trigger retry | delivery identity survives retries and final success replaces transient failure state |
| Trigger corruption | malformed inbox payload is quarantined instead of wedging the queue |
| Temporary chat | Save/durable-append remain memory-only and cleanup removes the transient session |
| Temporary chat races | concurrent Open for one key resolves to one session object |
| Streaming | existing API streaming tests assert incremental SSE/tool activity without a live provider |
| Provider failures | existing fake-provider and retry tests exercise cancellation and malformed responses |
| Latency | `runstats.LatencyRecorder` separately records pre-provider, TTFB, and total latency with an injected clock |

## Reproducible offline commands

```sh
go test ./internal/cron ./internal/triggers ./internal/session ./internal/api ./internal/agent ./internal/runstats -count=1
go test -race ./internal/cron ./internal/triggers ./internal/session ./internal/api ./internal/agent ./internal/runstats -count=1
go test ./internal/runstats -run '^$' -bench '^BenchmarkLatencyRecorder$' -benchmem -count=5
```

For scheduler/trigger restart testing, the harness uses real atomic files in `t.TempDir()` and process-local deterministic fault injection; it does not mock persistence away.

## Live-provider benchmark

Live-provider latency is deliberately **not** part of the default harness. When credentials are intentionally supplied, measure the same milestones: request accepted -> provider dispatch (`pre_provider`), request accepted -> first streamed content (`ttfb`), and request accepted -> terminal event (`total`). Keep provider/network measurements in a separate report because they are not reproducible CI gates.

## Regression policy

Do not weaken existing checks to make chaos tests pass. A discovered race, duplicate execution, persistence leak, or missing terminal state is a product defect. Fix the implementation or add a narrowly documented platform exception. The offline harness is the merge gate; live-provider numbers are diagnostic only.
