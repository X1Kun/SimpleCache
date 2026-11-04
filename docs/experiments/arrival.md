# Arrival-rate experiments

The first delivery qualifies five read behaviours against the production app
assembly, including its Outbox worker, and an isolated PostgreSQL schema.
This is a one-node integration path. It does not measure Envoy/Gateway capacity,
network partitions, write contention, or a long-running resource envelope.

| Distribution | Behaviour | Fixture requirement |
| --- | --- | --- |
| `hot` | All requests read key 0 | Known version-1 rows |
| `distinct` | Each planned request uses a different key | Keys >= total planned requests |
| `scan` | Sequential traversal, wrapping at dataset end | Known version-1 rows |
| `hot-scan` | 50% hot-key readers, 50% sequential readers | At least 2 keys; reports cohorts separately |
| `hit-miss` | 50% existing keys, 50% absent keys | No rows named `prefix-missing-N`; reports cohorts separately |
| `zipf` | Reproducible skewed reads | Known version-1 rows |

`hot` with an empty cache qualifies same-key cold loading. `distinct` qualifies
many-key cold loading only when its keys were not already warmed. `hot-scan`
becomes a cache-pollution experiment only when enough scan keys are actually
visited to exceed effective cache capacity; selecting the name alone does not
establish that condition. The short smoke below is a functional qualification.

## Run the production-runtime qualification

Use a dedicated initialized test database with CREATE SCHEMA permission. Each
run creates one unique schema and removes it after the application is stopped.
No existing caller tables or key versions are reset.

```bash
export SIMPLECACHE_TEST_DATABASE_URL='postgres://.../dedicated_test_db?sslmode=disable'
export SIMPLECACHE_ARRIVAL_REPORT_DIR="$PWD/bin/reports/arrival"
make arrival-smoke
```

The five behaviours each use baseline/pressure/recovery stages at 30/120/30
requests per second, one second per stage. They preserve separate JSON summaries,
raw request JSONL, Markdown reports, and 500ms application metric samples. The
runtime has a 1 MiB logical cache, two source slots, two pending source slots,
and a relay sharing its PostgreSQL pool. Each behaviour has a fresh key prefix;
the cache process is shared between behaviours, so these short results are not
paired capacity comparisons. PostgreSQL 17 runs this qualification
in CI; a local PostgreSQL run records only its actual environment.

A passing integration test establishes a usable path and payload correctness.
Inspect each experiment's `arrival.status`: client drops or HTTP errors remain
in its report even when the test correctly captures that outcome. Such a run
must not be quoted as an error-free capacity baseline. Failure to start, collect
metrics, or validate values fails the test.

## Run a custom schedule

Example configuration (save as `arrival.json` and replace the endpoint/host):

```json
{
  "endpoint": "http://127.0.0.1:9999",
  "host": "cache.local",
  "prefix": "owned-arrival-fixture",
  "scenario": "arrival",
  "distribution": "hot-scan",
  "keys": 4096,
  "value_bytes": 4096,
  "reads": 1,
  "workers": 32,
  "max_pending": 64,
  "max_schedule_lag_ms": 5,
  "max_catch_up": 8,
  "seed": 42,
  "stages": [
    {"name": "baseline", "rate": 100, "seconds": 10},
    {"name": "pressure", "rate": 500, "seconds": 10},
    {"name": "recovery", "rate": 100, "seconds": 10}
  ]
}
```

From `geecache-engine/`:

```bash
go run ./cmd/gateway-bench -config arrival.json -fixture fixture.sql
# Apply fixture.sql only to an owned disposable database/schema, before traffic.
# Replace cache processes after any fixture version reset; use a fresh prefix.
go run ./cmd/gateway-bench -config arrival.json -out ../bin/reports/custom-arrival
```

Fixture SQL has no database client or credentials embedded. It resets the owned
fixture rows to version 1 and deliberately bypasses event creation; never use it
on live data. `dataset_profile=mixed` and `key_style=mixed` use the same generator
for seeding and full-response verification. `reads` is retained for legacy config
validation and does not determine the arrival count; stages determine that count.

## Interpretation

- Planned count is the sum of rate * seconds, at most 100,000 over 120 seconds.
- Worker count and the pending queue bound outstanding client work. Zero pending
  permits direct handoff only. A full queue produces `client_capacity` drops.
- `max_schedule_lag_ms=0` retains the strict one-request-interval late-drop
  policy. Positive values (at most 50ms) set a rate-independent dispatch lag
  budget. Kind experiments explicitly use 5ms.
- `max_catch_up` bounds dispatches between timer waits (default 4, maximum 64).
  Over-budget work is `scheduler_late`; additional due work beyond a catch-up
  batch is `catch_up_limit`. There is no unlimited replay of overdue requests
  and no busy-spin loop. Small, bounded microbursts are permitted and declared.
- Raw records retain `dispatched` and `dispatch_lag_ms`; each group reports
  `schedule_to_dispatch` as well as schedule-to-start and total latency. The lag
  budget governs admission to the client queue, not worker queue waiting. A slow
  endpoint can still increase queue delay, fill the bounded queue, and cause
  `client_capacity` drops. No latency is reset to make those delays disappear.
- Cancellation retains all planned records; unattempted work is `canceled`.
- Planned = attempted + dropped. Attempted = successful + failed outcomes;
  error and incorrect counters can overlap and must not be added blindly.
- Expected missing-key 404 is successful. Existing-key 404, absent-key 200,
  wrong key, payload, or ETag is an incorrect outcome.
- Stages are continuous and attributed by planned time. Queued pressure-stage
  work can execute during recovery; queue/total latency preserves that backlog.
- Each stage and cohort has schedule-to-start, request, and schedule-to-completion
  P50/P95/P99. Request latency includes response body reading. Validation cost
  also contributes to total completion latency. Drops have no latency sample.
- Reports separate successful and failed request latency. Overall elapsed and
  successful outcomes/second include final drain; stage rate is offered load,
  not an achieved-throughput claim.
- `failed` means incorrect values or cancellation; `client-limited` means drops;
  `completed-with-errors` means HTTP/transport failures without drops; only
  `passed` is a clean run. Every non-passed run exits nonzero after saving evidence.

In this integration qualification the app, client and Go test runner share one
process. Process CPU/RSS/heap/GC samples include all three and cannot establish
server-only resource cost. Capacity measurements need separate client/server
processes and resource budgets.

The sampler records raw application counters and gauges (CPU, RSS, heap, GC, queue,
source work, Outbox). Sampling is not a continuous peak, and client CPU or DB
locks/connections need separate observations before asserting a bottleneck.

## Following deliveries

1. Deploy this client through the shared Gateway Service; retain per-Pod process
   identities and sampled cache/proxy/database/client resource timelines.
2. Sweep rates and working-set sizes, qualify baseline stability, and measure
   recovery after offered load drops. Repeat trials in alternating routing order.
3. Add mass expiry plus source slowdown, notification delay/lease recovery, and
   concurrent update/delete/recreate histories with an independent value oracle.
4. Run long-duration mixed-size workloads and controlled CPU/memory envelopes.

Do not treat those following deliveries as covered by this smoke.

The separate-Pod Gateway runner is now available as `make gateway-arrival`.
See [Kind experiments](arrival-kind.md) for rate stages, workload qualification,
resource sampling and interpretation limits.

## Scheduler calibration

`make arrival-calibration` builds an independent in-memory reference server and
client, alternates strict and 5ms/eight-slot policies for three repetitions at
1,500 requests/s, and retains all attempts. The reference server independently
counts received API requests; counts must agree with client attempted totals.
`calibration.json` classifies all three bounded trials, preserving strict-policy
drops. This validates the client path on the actual host; it is not cache capacity.
