# Kind arrival and recovery experiments

Run from the repository root:

```bash
make gateway-test
GOPROXY=https://goproxy.cn,direct make gateway-arrival
```

The runner creates a unique owned Kind cluster, builds the production Dockerfiles,
loads exact local images and runs three cache Pods, two Envoy data-plane Pods,
PostgreSQL 17 and one separate client Pod. Requests go through the shared Gateway
Service inside the cluster. No workstation port-forward carries measured client
traffic; port-forwards only qualify routes and collect metrics.

Existing unrelated clusters are not used. Success and failure both collect logs
and attempt to delete the owned cluster and temporary proxy image tag. The report
records cleanup status; cleanup failure invalidates the run. `GATEWAY_KEEP_FAILED=1`
retains a failed fixture explicitly. Measurements with client drops are recorded
as client-limited, not as clean capacity runs.

## Workload

Four behaviours run independently: hot, scan, hot-scan (50/50) and hit-miss
(50/50). Each gets a fresh prefix and new cache processes. Preflight reads key 0
through each proxy outside timing. The hot test therefore measures a warm key;
scan starts mostly cold. The data is read-only version 1, with 2,048 exact 4 KiB
values (about 8 MiB plus keys), against 1 MiB logical cache per shard / 3 MiB total.
The runner asserts that scan users actually read enough unique keys to exceed
aggregate cache capacity and actually revisit keys; a fixture size alone is not
sufficient proof of capacity pressure.

| Stage | Offered requests/s | Seconds |
| --- | ---: | ---: |
| baseline | 100 | 10 |
| load-300 | 300 | 10 |
| load-800 | 800 | 10 |
| load-1500 | 1500 | 10 |
| recovery | 100 | 15 |

Each trial plans 28,500 reads. Each cache Pod has a 500m CPU / 256 MiB memory limit (100m / 128 MiB requests).
The client has 64 workers, 128 pending slots,
GOMAXPROCS=2, a two-CPU and 256 MiB limit. It explicitly uses a 5ms dispatch-lag budget and at most eight dispatches per
catch-up batch. It retains scheduler-late, catch-up-limit, queue-full and canceled
drops, plus raw dispatch delay. Zero budget retains the earlier strict policy. Per-stage/per-cohort HTTP and total latency distributions
are kept alongside the raw request history. Client `started_unix`/`finished_unix` and sample wall-clock
envelopes align stages approximately; Kubelet observations can be cached. Expected absent-key 404 is a valid
outcome. Unexpected content, version, ETag, absent-key 200 or existing-key 404
fails correctness qualification.

## Evidence

`bin/reports/<run>/arrival.md` summarizes stages. `arrival.json` preserves every
client summary, configuration, image/binary identities, process UIDs, repeated
read counts and host pressure. `arrival-*/report.md` separates cohorts. Each
`arrival-*-timeline.jsonl` sample includes:

- Per-cache CPU, RSS, heap/allocations/GC, goroutines, cache occupancy/removals,
  source calls/queue/inflight, admission rejection and Outbox gauges.
- Kubelet resource observations for separate cache, proxy, client and PostgreSQL
  Pods plus node observations; these are sampled/cached, not continuous peaks.
- Before/after cache/client container cgroup v2 CPU throttling and memory-event
  counters (explicitly marked unavailable if the host exposes another layout).
- PostgreSQL connection count, active connections excluding the observer, lock
  waits excluding the observer, and database storage size.

Cache Pod UIDs and process start times must stay unchanged within a trial. Raw
resource specifications are saved before each trial. Faults and restarts are
separate experiments, not silently folded into this stable-topology baseline.
Sampling failures or incomplete request evidence fail qualification.

## Reading results

Check correctness first, then offered/attempted/completed counts and drop reasons.
A scheduler-limited client cannot establish the server's maximum throughput.
HTTP errors remain in measurements rather than being retried away. Compare
baseline and recovery at the same offered rate, checking queue/total latency,
server queues and resource trends. Successful traffic alone does not prove a
recovery from saturation: confirm the load stages actually reached a limit.

A scan that fills and revisits a working set beyond total cache capacity tests
repeated loading and eviction. In hot-scan, inspect the hot cohort independently
to quantify interference. In hit-miss, compare normal and missing cohorts; 404
is expected but its database cost is still observable.

The database uses a bounded, memory-backed temporary volume and normal SQL
settings. This isolates host disk fsync interference and does not validate
physical durability. All Pods still share one host. Host load and I/O pressure
can affect the control plane, network and client scheduler; document such runs
as exploratory. Short trials cannot establish memory-leak freedom, sustained
SLOs or long-duration Outbox throughput. This read-only phase does not create an
Outbox backlog; the worker runs, but replay/cleanup pressure needs write workloads.

## First observed run

The first retained run (`20261104T040352Z-gateway-ef72ff`) emitted 102,539 of
114,000 planned reads with no incorrect values or HTTP/transport errors, but
11,461 client-side late drops. It is classified **client-limited**. Full scans
visited all 2,048 keys and revisited them; capacity eviction/source reload were
actually exercised. At high offered rates, drops biased the planned 50/50
cohorts. Calibrate the client scheduler before interpreting higher-rate server
capacity or cohort interference. The retained `assessment.md` explains the
observed results and limits; artifacts remain local under ignored `bin/reports/`.

Scheduler retest selection (all old reports remain unchanged):

```bash
make arrival-calibration
GATEWAY_ARRIVAL_DISTRIBUTIONS=hot,hot-scan make gateway-arrival
```

Default selection still runs all four behaviours. The selected list is validated
and saved in the new manifest, so a two-behaviour retest cannot be confused with
a full four-behaviour run. Keep old and new rate/cohort counts when comparing.

At 1,500/s, 5ms spans 7.5 arrival slots. A four-dispatch batch can therefore
reject requests still inside the lag budget. The current Kind policy explicitly
uses eight slots, retaining both a finite catch-up cap and the 5ms deadline.
The first calibrated Kind report (48ad76) retains its four-slot configuration
and eight catch-up-limit drops; it has not been relabelled as passed.

The final eight-slot qualification (`20261104T060402Z-gateway-56b575`) retested
hot and hot-scan with identical production image IDs and planned trace hashes.
Both completed all 28,500 requests (57,000 combined), with no client drops,
HTTP/transport errors or incorrect values. At the 1,500/s stage each completed
15,000 requests over ten planned seconds; mixed cohorts each completed 7,500.
The comparison preserves the original 8,198 drops and the intermediate eight
catch-up drops for the same selected behaviours. This qualifies the scheduler
at the tested load and host conditions; it does not establish server saturation
or long-duration production SLOs.
