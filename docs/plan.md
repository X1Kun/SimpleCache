# SimpleCache implementation plan

Focus: Go distributed-system reliability and Kubernetes platform engineering. Retain the current project and existing English documentation/CI. The current scope is a one-to-two-week functional and diagnostic pass.

## Product cache learning loop (2026-10-20)

The [product walkthrough](products.md) now connects an external PostgreSQL source
to the existing cache: read → update/delete source → best-effort HTTP invalidation
→ reload. The immutable demo and its diagnostics remain available independently.
An optional Operator sourceSecret wires the database and shared mutation token.

Local validation passed: full engine race tests, real PostgreSQL with three HTTP
peers, Operator envtest (controller statement coverage 86.9%), Operator lint,
engine vet, generated-file comparison, Kubernetes rendering and Compose parsing.
The separate CI PostgreSQL job is configured; hosted CI was not run in this session.

A dedicated three-cache-container plus PostgreSQL smoke also passed: three
sequential entry-node reads of one cold key caused one actual source load;
committed updates invalidated all known copies; a stopped peer produced HTTP 202
with source_committed=true; restarting it and explicitly retrying invalidation
recovered; deletion removed the database row and subsequent reads returned 404.
These are functional observations, not throughput or strong-consistency claims.

Remaining boundaries are intentional: group-wide invalidation epochs can suppress
unrelated in-flight fills; notifications are not durable; discovery is eventually
consistent; concurrent readers may observe old results; TTL remains the fallback.
Further work should measure a representative workload before adding durable event
delivery, hot replicas, or a more granular invalidation mechanism.

## Progress

| Work | State |
| --- | --- |
| Module/package organization | Complete |
| Reliable peer/source reads and graceful lifecycle | Complete |
| EndpointSlice membership and atomic snapshots | Complete; local contracts verified |
| Root CI and English documentation | Complete; hosted runs remain separate |
| Complete Operator resource/discovery contract | Implemented; envtest/race passed |
| Original dedicated Kind deployment smoke | Passed in the user's local terminal |
| Adversarial validation and per-run reports | Implemented; consult each run's results |
| Local diagnostic baseline and test-process profiles | Automated by make validate |
| Expanded Kind continuity/resource recovery | Passed locally; each run generates independent evidence |
| Separate-process baseline and server-only profiles | Complete; final 12-trial run passed locally |

## Completed: validate the expanded suite

Operator Services/StatefulSet/account/permissions, child watches, resources/probes/Status and conflict/default handling are implemented. Static PEERS is removed and controller tests reflect the new contract.

Acceptance: envtest plus dedicated Kind startup/read; child repair; 3→5→2 membership convergence without restarting existing Pods just to publish peers. Retain app.serve, existing cancellation/boundary tests, and current CI.

Envtest/race and the original local Kind smoke passed. Run make validate for engine race checks and bounded non-race diagnostics/profiles. Run make kind-smoke for continuous reads during two scaling cycles/Pod repair, then an impossible CPU request with Unschedulable/Ready=False and recovery. Each run saves report.md/report.json, logs and artifacts under ignored bin/reports. The expanded local Kind run on 2026-10-09 passed: 914 concurrent reads, zero errors/incorrect values, P99 105.204ms during the continuity window. This is a functional observation, not a throughput claim. Resource rollback explicitly removes Pending test Pods; it does not claim fully autonomous invalid-rollout recovery.

Deterministic local evidence: the fixed 100,000-key dataset moved 25.364% of owners
on 3→4 expansion, all toward the new node. The 16KiB cache reloaded all 768
sequential reads of a roughly 1MiB working set over three passes and stayed within
its logical byte bound. Same-key bursts performed one actual source load;
different-key bursts stayed within the configured source concurrency limit.

## Completed: isolate performance attribution

`make perf` runs warm-hot, fresh distinct-key and 1MiB capacity scenarios for
10 seconds each, three unprofiled repetitions, then separate profiled trials.
Fresh owned server subprocesses reuse the real server/cache lifecycle. Reports
include process identities, the server binary digest, fixed workload settings,
warmup, percentiles, source/CPU deltas and sampled RSS/heap. Profile overhead is
excluded from baseline aggregation. Quick mode is explicitly not a baseline.

Acceptance: zero errors/wrong values; warm traffic performs no source loads;
cold distinct requests match source loads; capacity runs evict/reload within their
logical bound; CPU/heap/allocs/mutex/block profiles parse against the retained
server binary; repeated baselines remain separate from profiled trials. No cache
optimization is required without an actionable measured bottleneck.

Final local run: 20261009T143114Z-perf-UiFnYx, 12 trials passed with zero request
errors or wrong values. Unprofiled P95 medians were 0.291ms (warm hot), 101.109ms
(fresh distinct keys), and 101.158ms (capacity pressure); each is the median of
three 10-second runs, not a production SLO. Warm traffic performed no source
loads, cold distinct loads matched requests, and logical cache bytes stayed
within the configured bound during repeated eviction/reload.

Investigation: the previous mixed-process profiles could not attribute client
costs to the server. Isolated profiles now show HTTP/syscall work as the largest
hot CPU component, with HTTP header/context work leading sampled allocations.
The sampled cache mutex delay was about 0.239ms in an 11-second hot capture;
this does not justify replacing the LRU lock at the tested load. Cold latency
tracks the intentionally simulated 100ms source. Most retained heap in the
capacity fixture belongs to the roughly 40MiB immutable source dataset, not
the 1MiB cache. Sampling/instrumentation overhead and shared-host limits remain.

Decision: retain the cache implementation and immutable value-copy contract.
No cache-path optimization or before/after improvement is claimed. Repeat with
representative workload constraints before choosing any subsequent optimization.

## Then: monitoring and diagnosis

Logical cache bytes/capacity/entries/removals and test-process CPU/heap/mutex/block profiles are implemented. Use profiles to identify a real bottleneck before optimizing. A small dynamic-scrape dashboard and private deployed-process profiling remain optional follow-up; no public pprof endpoint is added.

Acceptance: all active Pods are visible; counters match actual operations; CPU/heap/mutex/block captures are reproducible. Identify a real bottleneck before choosing an optimization.

Initial diagnostic finding (2026-10-09): the combined loopback client/server CPU
profile is dominated by syscalls, and allocation leaders include HTTP buffers and
io.ReadAll. This profile includes the load generator and synthetic source, so it
does not justify a cache-path optimization. Separate client/server profiling or
scenario-specific captures before attributing costs or claiming improvement.

## Then: bounded experiments, about 2–3 days

| Scenario | Evidence |
| --- | --- |
| Cold hot key / slow peer | Correct values, actual source calls, fallback and elapsed time |
| Scale / owner Pod loss | Convergence, Pod UIDs/restarts, routing changes |
| Small cache | Logical byte bounds, eviction and successful reload |
| CPU/memory limits | Test-process heap and latency; deployed throttling remains follow-up |
| Infeasible resource request | Pending/events, controller condition, recovery |

Use a fixed dataset/seed, explicit resources, warmup and value checks. Automated HTTP diagnostics use one-second samples per scenario to keep the default suite short; repeat/extend samples for performance investigations. Record P50/P95/P99 only for actual runs; do not invent QPS or improvement percentages. No OOM drill is included; any future controlled OOM belongs to one named Pod in the dedicated cluster, never host-wide pressure.

## Delivery, about 1 day

Current CI uploads local adversarial reports/profiles, including failures. Add lightweight Kubernetes CI only after the expanded local smoke is stable. Keep fault scenarios manual. Publish one investigation: symptom → hypothesis → evidence/profile → change → measured comparison. If no actionable bottleneck appears, preserve the implementation and state that. Loopback results are not deployed throughput; logical bytes are not RSS and Pending is not OOM validation.

Complete means active code, repeatable cluster behavior, useful metrics, one resource/failure demonstration, and accurate limitations. Performance proof is separate from functional tests.

Do not add HPA against an Operator-owned StatefulSet without defining a single replica authority. Scheduler plugins, GPU management, and AI service orchestration are separate later specialization, not cache feature additions.
