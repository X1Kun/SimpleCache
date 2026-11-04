# Workloads and diagnostics

Production `cmd/simplecache` must not import benchmark or validation packages.
Tools can use runtime contracts; runtime tests stay alongside their implementation.
See the [command roles](../../cmd/README.md) before choosing a tool.

| Package | Responsibility | Executable |
| --- | --- | --- |
| `gateway` | Gateway workloads, bounded arrival scheduling, cohorts and correctness evidence | `cmd/gateway-bench` |
| `gateway` / reference fixture | Independent in-memory client calibration | `cmd/arrival-reference` |
| `diagnostics` | Separate-process synthetic baselines and profiles | `cmd/diagnostic-bench` + `cmd/diagnostic-server` |
| `democlient` / `measurement` | Shared demo HTTP checks and nearest-rank latency summaries | Internal tool helpers |
| `../validation/probe` | Immutable demo continuity checks; no capacity conclusions | `cmd/validation-probe` |
| `../validation/report` | Go test/Ginkgo/profile evidence aggregation | `cmd/validation-report` |
| `../validation/scenarios` | Bounded local-cache adversarial cases | `make validate` |

The retired Peer-routing comparison and its independent PostgreSQL fixture runtime
were removed. RoundRobin and consistent-hash controls both use Gateway and the
same local-read engine. PostgreSQL arrival integration uses the production app
assembly, including the relay. Archived profiles/reports retain their original
binaries and must not be interpreted as results for a currently supported mode.

See [arrival experiments](../../../docs/experiments/arrival.md) and
[Kind experiments](../../../docs/experiments/arrival-kind.md) for scheduling,
fixture ownership, qualification, resource sampling and interpretation limits.
