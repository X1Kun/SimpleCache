# Validation tooling

Run from the repository root:

```bash
make gateway-test         # Tool regression tests, no Docker
make gateway-smoke        # Basic real-cluster behavior
make gateway-experiments  # Basic + extended fault scenarios
make gateway-perf         # Paired routing measurements
python3 -m tools.validation --help
```

| Module | Responsibility |
| --- | --- |
| `__main__` | Explicit suite selection and signal-handler lifetime |
| `reporting/run` | Per-run paths, identity, settings, logs and final report |
| `environment/kind` | Commands, scoped kubeconfig, HTTP/port-forward transport, snapshots and cleanup |
| `environment/gateway` | Provision the owned Gateway/PostgreSQL fixture |
| `gateway/runner` | Basic scenarios and retained functional-run continuation |
| `gateway/checks` | Cross-proxy routing and worker metrics assertions |
| `gateway/faults` | Extended fault injection and recovery assertions |
| `gateway/performance` | Paired workload orchestration and measurements |
| `gateway/helpers` | Shared SQL fixture access and Prometheus parsing |
| `reporting/outcome` | Aggregate current and retained failures |

Imports and context construction have no filesystem or cluster side effects.
Entering a `GatewayRun` opens its evidence files. The run context owns its
unique cluster name, kubeconfig, credentials and temporary proxy tag. Exiting
records failures, captures diagnostics, cleans owned resources, writes
`result.json` and closes files. Cleanup failure makes a successful suite fail.
`GATEWAY_KEEP_FAILED=1` retains only failed fixtures for explicit diagnosis.

Existing environment variables, report paths/schema and continuation ownership
checks remain supported. `scripts/gateway/smoke.py` is a compatibility CLI;
`performance.py`, `scenarios.py` and the old unittest entrypoint re-export the
new modules. New implementation belongs here. The fixed-release downloader
remains `scripts/gateway/fetch.sh`, shared with Operator envtest.

Go workload clients remain in the engine module under `internal/bench`
(`gateway`, `postgres`, `diagnostics`). Go test/profile evidence aggregation
lives in `internal/validation/report`, and adversarial tests in
`internal/validation/scenarios`. The existing `cmd` binary names remain stable.
This refactor does not change workload generation, request validation, cache
behavior or experiment budgets.
Capacity scheduling and dataset diversification are subsequent changes.

Fixed-rate workloads and their one-node PostgreSQL/runtime qualification are
documented in [arrival experiments](../../docs/experiments/arrival.md).

`make gateway-arrival` runs fixed-rate workloads through an owned Kind Gateway
with separate client, cache and PostgreSQL Pods. See
[Kind arrival experiments](../../docs/experiments/arrival-kind.md).
