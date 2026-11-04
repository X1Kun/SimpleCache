# Engine entry points

Only `simplecache` is the business service. The other commands run experiments,
check correctness or collect evidence; they are not additional deployment components.
Keep entry points thin and implement reusable behavior under `internal`.

| Role | Command | Purpose and normal entry |
| --- | --- | --- |
| Business service | `simplecache` | Production app assembly: API, PostgreSQL, cache and Outbox. Start the complete local system with `make dev-up` from the repository root. |
| Workload client | `gateway-bench` | Product JSON workloads, correctness checks and bounded arrival scheduling. Used by the Gateway validation scripts. |
| Client calibration fixture | `arrival-reference` | In-memory HTTP responses for checking the load generator's scheduling limits. Does not run the cache or database and does not measure business service capacity. |
| Continuity checker | `validation-probe` | Checks immutable `Auto-*` demo values during the dedicated Kind smoke test. Writes `probe.json` and stops on `probe.stop`; it is not a product JSON client. |
| Local diagnostic server | `diagnostic-server` | Runs the real app with a synthetic source on loopback for optional local profiling. Launched by `diagnostic-bench`. |
| Local diagnostic driver | `diagnostic-bench` | Launches fresh server processes and separates unprofiled trials from server profiles. Use `make perf`; `PERF_QUICK=1 make perf` only checks that the workflow runs. |
| Evidence reporter | `validation-report` | Aggregates test, cluster and profile artifacts. Used by `scripts/validation/run.sh`; generates no workload. |

`validation-probe` and the local diagnostic driver share the bounded demo HTTP
client in `internal/bench/democlient`. Gateway workloads retain their product JSON,
version and conditional-write contracts in `internal/bench/gateway`.

Ordinary tests in `internal/validation/scenarios` use fixed request counts for
HTTP read contracts and bounded bursts for concurrency/timeout checks. Their
`functional-contract` evidence and any test-process profiles explain correctness;
they do not establish throughput, latency SLOs or server resource baselines.
The continuity probe's port-forward latency samples are also observations only.
Use the diagnostic tool pair for local synthetic measurements and the
[Kind experiments](../../../docs/experiments/arrival-kind.md) for deployed behavior.

Tool latency summaries use nearest-rank percentiles. Older diagnostic/probe
artifacts used a floor-index convention; retain their original interpretation
and compare results produced by matching tool versions.
