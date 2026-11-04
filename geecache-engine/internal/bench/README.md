# Workloads and diagnostics

These packages are tools within the existing engine Go module. The production
`cmd/simplecache` dependency graph must not import `internal/bench` or
`internal/validation`. Workload/fixture processes can use runtime packages to
exercise real behavior. Runtime tests stay next to their implementation.

| Package | Responsibility | Executable |
| --- | --- | --- |
| `gateway` | Deterministic Gateway workload client and response validation | `cmd/gateway-bench` |
| `postgres` | Paired PostgreSQL/cache subprocess experiments | `cmd/postgres-compare` |
| `diagnostics` | Separate-process synthetic baselines and server profiles | `cmd/diagnostic-bench` |
| `../validation/report` | Go test/Ginkgo/profile evidence parsing and reports | `cmd/validation-report` |
| `../validation/scenarios` | Bounded adversarial engine tests and profiles | `go test` via `make validate` |

Gateway files separate configuration (`config.go`), deterministic values/traces
(`dataset.go`), request validation (`client.go`), measurement structures and
quantiles (`records.go`), and run scheduling/evidence (`run.go`).

The old package paths were `internal/gatewayperf`, `internal/productcompare`,
`internal/diagnostics`, and `internal/validationreport`. These are internal Go
imports; callers in this repository have moved. Executable names, CLI options,
workload values, result schemas and report locations remain compatible.
Historical pprof files must continue to use their original matching binaries.

Workload-specific correctness rules and latency scopes remain distinct.
`arrival.go` implements bounded open-loop scheduling and stage/cohort reports.
See [arrival experiments](../../../docs/experiments/arrival.md) for fixture
ownership, short real-runtime qualification, outcome rules and remaining work.
