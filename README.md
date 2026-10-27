# SimpleCache

A Go loading cache with a Kubernetes Operator and a PostgreSQL-backed,
read-mostly example. PostgreSQL owns persistent values; cache nodes retain a
bounded subset and use best-effort invalidation after writes. Product JSON is
example data for studying source work, routing cost and recovery.

**Current:** local LRU/TTL, optional short-lived hot copies, consistent-hash
Peer reads, separate route/source SingleFlight, bounded miss/source/write admission,
EndpointSlice routing snapshots and an idempotent Operator. Product reads return
a source-version ETag; updates/deletes require If-Match,
and creation requires If-None-Match: *. PostgreSQL checks conditions atomically;
logical deletions preserve generation across recreation. Active expiration runs
in bounded batches, and separate database-write/fan-out timing metrics expose
write latency. Overload is an explicit HTTP 503 with Retry-After.

**Agreed next direction:** multiple Gateways route keys directly to cache shards;
PostgreSQL remains authoritative; version-bearing invalidation is a follow-up
to the implemented conditional writes.
Start without cache replicas and add them only for measured recovery needs.
Gateway routing and durable/version-bearing invalidation remain planned.

Use the single [project guide](docs/design.md) for
[current architecture](docs/design.md#current-implementation),
[target architecture](docs/design.md#agreed-target-architecture),
[configuration and PostgreSQL walkthrough](docs/design.md#run-and-configure),
[validation and measurements](docs/design.md#validation-and-evidence), and
[delivery order](docs/design.md#delivery-plan).

## Quick start

Use a Go toolchain compatible with Go 1.25.3. Start the demo in one terminal:

```bash
cd geecache-engine
go run -buildvcs=false ./cmd/simplecache -port=8001 -api=true
```

Read from another terminal:

```bash
curl -fsS 'http://localhost:9999/api?key=Auto-666'
curl -fsS http://localhost:9999/readyz
```

For the current three-cache PostgreSQL example, run from the repository root:

```bash
docker compose -f deploy/products/compose.yaml up -d --build
curl -fsS http://127.0.0.1:19999/readyz
curl -fsS 'http://127.0.0.1:19999/api?key=product:42'
```

Existing database volumes need the [explicit schema upgrade](docs/design.md#postgresql-example).
Follow the walkthrough for conditional mutation examples,
partial delivery/retry, database-volume handling and consistency limits.

## Development checks

```bash
make check
make test-operator-ci
make generated-check k8s-render compose-config
make postgres-compare COMPARE_QUICK=1  # Existing functional smoke
```

Next, prioritize Gateway routing, multi-instance behavior, Operator integration
and version-bearing invalidation. Keep necessary development checks and smokes;
full-chain performance experiments follow once these features work together.

Reports, original measurements and matching binaries are saved under ignored
bin/reports. Historical Peer-routing/hot results do not establish performance
for the planned Gateway architecture. See the [evidence and limits](docs/design.md#observed-results-and-their-limits).

The [isolated Kind workflow](docs/design.md#kubernetes-deployment) uses only the
project's dedicated cluster and kubeconfig. Envtest uses a temporary API server
and etcd; it does not deploy cache Pods into an existing cluster.

## Repository

| Path | Contents |
| --- | --- |
| geecache-engine | Cache, source adapter, HTTP API/Peer, discovery, metrics and diagnostic tools |
| simplecache-operator | Kubebuilder API, controller and Kubernetes manifests |
| deploy / scripts | Example deployments and repeatable validation workflows |
| docs/design.md | Architecture, operation, evidence and agreed roadmap |
| .github/workflows | Build and validation CI |
