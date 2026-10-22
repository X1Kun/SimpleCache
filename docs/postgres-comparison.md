# PostgreSQL cache workload comparisons

Run the same bounded workload against three independent caches and three
hash-coordinated caches, backed by real PostgreSQL. The benchmark compares
source work and the cost of routing, not an assumed speedup.

```bash
make postgres-compare
make postgres-compare COMPARE_QUICK=1
make postgres-compare COMPARE_WORKERS=8 COMPARE_REPEATS=3 COMPARE_PASSES=3
```

The default is three paired repeats, 12 closed-loop workers, 512 keys with exact
4KiB JSON values, and 1MiB logical cache per process. This working set exceeds
one cache and fits each actual owner on the 100-vnode ring. Invalid combinations
are rejected instead of silently running a misleading capacity comparison.
Quick mode permits one repeat and one capacity pass, and labels the entire
report as a functional smoke rather than a baseline.
These are fixed-count diagnostics; even default stages can finish quickly.
Their percentiles describe this trace, not sustained saturation throughput or
a production latency SLO.

## Isolation and implementation

The wrapper builds the driver and a diagnostic fixture into a unique
`bin/reports/<UTC>-postgres-compare-<random>/` directory. By default it creates
only a new PostgreSQL Compose service with a random loopback port, separate
network and volume. It removes these owned resources on ordinary success/failure.
It does not deploy to Kind or use the product example's existing database volume.

To use an explicitly selected test database instead, set
SIMPLECACHE_COMPARE_DATABASE_URL to a PostgreSQL URL. The account needs CREATE
SCHEMA permission. Each run creates a randomly named schema/table and removes
only that schema after all its cache children stop. No caller table is truncated
or dropped. The URL and mutation token are not written to reports or process
arguments. Host/database credentials remain environment variables.

All variants run fresh, separate cache processes with GOMAXPROCS=2. The fixture
reuses the real PostgreSQL source, Group, Router, API/mutation handlers, peer
handlers and metrics. It owns a small diagnostic startup/shutdown path; it is
not the deployed executable. Loopback API and peer ports are allocated before
startup and released immediately before the child binds; a competing bind causes
a recorded startup failure before any benchmark requests are sent.

Independent mode disables peer **reads**; the same three peers are still used
for write invalidation. Coordinated mode enables the current consistent-hash
read policy. The main independent/coordinated pair keeps write semantics, total cache
memory, source concurrency, connection limits, TTL and request traces equal. Source budgets remain 800ms,
peer reads 1s and route work 1.2s. The one-hour source-cache TTL avoids expiry bias in the main pair.

Zipf and mixed scenarios also run `coordinated-hot`: one eighth of the same total
cache budget is reserved for peer replicas, with at most one-second TTL; the
remaining seven eighths store source fills. It is an opt-in tradeoff, not extra
cache capacity. The mixed scenario additionally runs `coordinated-global`,
which reproduces the former group-wide generation counter; coordinated uses
256 fixed stripes. Stripes reduce unrelated fill cancellation, not mutex
contention, and hash collisions remain conservative. Pair these variants
against coordinated, not against a different memory budget.

## Scenarios

| Scenario | Preparation and measurement | Purpose |
| --- | --- | --- |
| Cold key | Empty caches; 192 requests across all entries. Hold an exclusive lock on the owned PostgreSQL table until the expected loading processes are observed, with a 500ms observation deadline. | Observe three independent source fills versus one coordinated fill under real overlapping database waits. |
| Capacity | One full working-set warmup on each entry, then two measured passes through the same indexed trace. | Measure duplication/eviction and whether aggregate cache capacity avoids reloads. |
| Updates | Warm four keys at all entries; four sequential API commits, each followed by reads through all three nodes. | Check fresh values after quiescent acknowledged invalidation and count its source/peer work. |
| Zipf reads | Full working-set warmup at every entry, then 8,192 deterministic Zipf reads (s=1.2) with random entry nodes. | Measure peer RPC amplification on hot keys and the optional hot-replica tradeoff. |
| Mixed 95/5 | Four times the normal key count (capped at 4,096), 512 Zipf warmup reads, then 30 windows of one write and 19 overlapping Zipf reads; drain each window. | Check declared payload versions, measure stale-at-start observations and discarded fills under hot-key writes. |
| Node failure | Warm a key on its owner node-2, stop that process, then direct 96 requests to the two survivors with stale static membership. Restart the empty owner, explicitly invalidate, then read across all three again. | Observe fallback load and the explicit recovery cost. |

The controlled lock is confined to the run's owned table; it is not an artificial
sleep added to the source. Cold latency includes that gate and is not a normal
database latency baseline. Other scenarios add no source delay.

The update scenario stops writes while post-update reads are checked. The mixed
scenario instead overlaps a single serialized writer with bounded readers:
the worker limit includes the writer. It writes the hottest key with increasing
versions; unrelated keys must stay at version one. Each response must match the
complete deterministic payload of a version in the submitted interval. A read
older than the acknowledged version known at its start is counted separately
as stale-at-start, not corruption. Final reads after explicit invalidation must
return the latest version. This bounded window workload is inspired by YCSB's
read-heavy mix; it is not YCSB, sustained saturation, or a linearizability checker.
Override `COMPARE_ZIPF_READS` and `COMPARE_MIXED_WINDOWS` to change sample sizes.

The original post-update checks and the mixed final invalidation check do not establish
linearizability or read-after-write consistency during arbitrary concurrent
distributed traffic. The fault is a stopped loopback process, not a blackholed
network; membership does not change. Recovery is explicit invalidation, not
automatic migration or notification replay. Any partial recovery notifications
are retained, with at most three explicit attempts.

## Fairness and evidence

- Every trial has fresh cache subprocesses. Values are reset in PostgreSQL before
  each trial. Paired stages have identical key/node/version traces and hashes;
  the worker bound is equal, but actual closed-loop arrival/completion order can
  differ. Odd repeats use the listed variant order; even repeats reverse it. A default
  run now has 45 trials (15 variants/scenarios × three repeats); quick has 15.
- The capacity trace is a sequential scan deliberately exercising duplicated
  working sets at every entry. It is not an estimate of a production access
  distribution or hit rate.
- Database/OS caches are not flushed. The small dataset can be entirely resident
  in PostgreSQL memory even while exceeding a single cache's logical budget.
- Reports aggregate source calls across all participating processes. Actual
  Getter calls are not a total count of every SQL statement, startup/schema
  query, or PostgreSQL background operation. Warmup and setup are excluded.
- Source loads per request are not a hit ratio: request coalescing also reduces
  source calls. API and peer local lookup counters remain separate in JSON.
- Before/after snapshots retain per-node PID, CPU, cumulative Go allocated bytes, RSS, heap, cache/hot bytes,
  entries, removals, source loads, writes, peer requests, fallbacks and local
  lookups. Counter sets split around node restart; different PIDs are never
  subtracted. RSS is a snapshot sum, not a continuously sampled peak. Allocated bytes are
  monotonic stage deltas across cache processes, including writes and metrics
  collection, not retained memory or wire bytes.
- Peer request counts show routing work; byte-level network traffic is not
  measured. Read percentiles include all attempted read requests, with HTTP
  errors and wrong values reported separately. Update elapsed time also includes
  the writes, whose durations and acknowledgements are retained separately.
- Aggregates pair successful stages by repeat and trace digest. Per-trial P95s
  are summarized as medians, never pooled as though they were raw samples.
  Latencies of the small update/recovery stages remain functional observations.

Each run keeps report.md, report.json, wrapper-status.json, raw request JSONL,
per-stage/node Prometheus text, fixture logs, binary SHA256, initial and restarted
PIDs, dataset settings/placement and the matching binaries. Failures retain
partial reports and logs. Check both the driver report and wrapper exit status;
wrapper cleanup/build errors are not successful benchmark results. Hard-killing
the wrapper/host can prevent cleanup; recorded PIDs/project names aid diagnosis.

Functional success requires correct declared payloads/statuses, completed
requests, valid metrics, final post-invalidation freshness and cache bounds.
Mixed stale-at-start counts remain visible and do not imply stronger consistency. It never requires a latency or percentage improvement.
Coordinating cache fills can save source work while adding peer latency/CPU; use
the measured result, including unfavorable results, to choose subsequent changes.

CI runs the explicit quick workload against its dedicated PostgreSQL service.
The default three-repeat investigation remains a manual experiment.

## Historical four-scenario observation, 2026-10-22

Run `20261022T023036Z-postgres-compare-nQLluQiG` completed all 24 trials against
PostgreSQL 17.11 with zero read errors, wrong values or skipped requests.
The working set was 2,134,016 logical bytes against three 1MiB caches.
The table below uses medians of three matching trial observations.

| Stage | Source loads: independent / coordinated | Peer reads: independent / coordinated |
| --- | --- | --- |
| Cold burst, 192 reads, controlled lock | 3 / 1 | 0 / 35 |
| Capacity, 3,072 reads after warmup | 3,072 / 0 | 0 / 2,048 |
| Four updates followed by 12 reads | 12 / 4 | 0 / 8 |
| Stopped owner, 96 reads on survivors | 2 / 2 | 0 / 2 |
| Restart plus explicit invalidation, 96 reads | 3 / 1 | 0 / 17 |

For the capacity stage, P95 medians were 2.064ms independent and 2.157ms
coordinated; trial ranges overlap. The fixture demonstrates reduced source work
in this duplicated working-set case, but does not establish a general latency
improvement. Source CPU/I/O savings were not measured. Cold-gate latencies and
small update/recovery samples are not ordinary latency baselines.

The owned schema was dropped and wrapper cleanup returned exit code zero.
Raw evidence and matching binaries remain in the ignored local report directory;
future runs generate their own independently identifiable evidence.

## Optimization comparison, 2026-10-22

Run `20261022T074739Z-postgres-compare-takb8Znb` completed 45 trials / 63 stages,
with 101,412 measured reads, no read errors, corrupt values or skipped requests.
Matching stage trace digests, stable per-stage PIDs, logical capacity bounds,
owned-schema deletion and wrapper cleanup were checked. The following numbers
are medians of three paired trials, using 1MiB total logical bytes per node:

| Workload / metric | Coordinated | Coordinated + 128KiB hot share |
| --- | ---: | ---: |
| Zipf, 8,192 reads: source calls | 0 | 0 |
| Zipf: peer reads | 4,670 | 1,617 |
| Zipf: Go allocated MiB during stage | 223.58 | 97.48 |
| Zipf: P95 ms | 2.051 | 1.493 |
| Mixed, 570 reads / 30 writes: source calls | 42 | 42 |
| Mixed: peer reads | 344 | 219 |
| Mixed: Go allocated MiB during stage | 20.96 | 16.01 |
| Mixed: P95 ms | 2.435 | 2.274 |

This supports opt-in hot replicas for the tested skew: about 65% fewer Zipf
peer reads at the same total cache budget. It does not establish a production
SLO, a sustained throughput gain or a benefit under scans/uniform churn.
Allocation deltas include all cache-process work within each measured stage;
RSS is a separate boundary snapshot. The hot reservation can worsen source
misses if the remaining owner area is too small.

The mixed global/striped comparison had 42 source loads and zero discarded
source fills in both variants: no source-load benefit was demonstrated here.
A deterministic blocked-source test separately demonstrates the narrower
contract: invalidating another stripe preserves the active fill (one source
call versus two with a global epoch); a stripe collision still fences it.
This is correctness/scope evidence, not a measured PostgreSQL speedup.

Mixed stale-at-start observations were zero in this run, including hot mode.
That finite observation does not prove read-after-write consistency: overlapping
notifications can still admit old replies. Default hot budget remains zero;
short inherited TTL and invalidation fencing bound retention without creating
a distributed read barrier. A real HTTP-peer lifecycle test additionally warms
two hot replicas, then verifies acknowledged writes and deletes clear copies.

A three-repeat ByteView microbenchmark in the same evidence directory records
4,096B / one allocation for the 4KiB defensive-copy write and zero bytes / zero
allocations for WriteTo. The counting writer isolates copy allocation and does
not transmit the body: its timings are not HTTP latency or network throughput.
ByteSlice still returns a defensive copy.
