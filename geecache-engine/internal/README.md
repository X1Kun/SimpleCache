# Runtime module boundaries

| Package | Responsibility |
| --- | --- |
| `app` | Configuration, component assembly, HTTP API and service lifecycle |
| `cache` | Loading-cache policy, admission, storage, expiration and version fencing |
| `peer` | Immutable member snapshots, Peer HTTP/Protobuf protocol and invalidation delivery |
| `discovery` | EndpointSlice observations published to the Peer directory/router |
| `source/postgres`, `demo` | Authoritative loading-source implementations |
| `telemetry` | Metrics implementing cache observers |
| `bench`, `validation` | Workload/validation tools; not production dependencies |

`app` assembles adapters around `cache`. `peer`, sources and telemetry depend on
cache contracts; cache does not import those implementations or HTTP/Kubernetes.
Tests stay alongside the code whose contract they exercise.

## App

- `bootstrap.go`: source selection, component wiring and background tasks.
- `lifecycle.go`: bind all listeners, roll back partial binding, readiness/drain.
- `api.go`: HTTP routes and public API options.
- `mutations_http.go`: authentication, validation, body limits, write admission,
  preconditions and HTTP response encoding.
- `mutations.go`: transport-independent mutation commands/outcomes, source writes
  and post-commit/best-effort notification coordination.

Write admission spans body reading, source work, notification and HTTP response.
Caller disconnection does not cancel a started write's notification; service
shutdown still bounds work. Conflicts are known non-commits and do not broadcast.
Source failures keep unknown-commit semantics and attempt force invalidation.
Existing exported entrypoints and JSON/status contracts remain unchanged.

## Cache

- `group.go`, `interfaces.go`, `options.go`, `errors.go`: aggregate state and contracts.
- `read.go`: lookup, routing, flight coalescing, fallback and bounded retry.
- `load.go`: one admitted source attempt and guarded cache fill.
- `versions.go`: version floors, invalidation, epochs and Peer admission.
- Storage, admission and maintenance remain in their existing focused files.

`readFence` is a value snapshot of epoch/minimum, not another synchronization
owner. `sourceResult.minimum` carries the highest floor observed during a load.
Retry must retain it even when metadata is evicted. Shared state stays in Group;
lock order remains `epochMu -> cache.mu`, and version checking/fill admission is
one critical section. Caller cancellation, original deadlines, source limits,
TTL and hot-copy rules must remain intact when changing these files.

## Peer

`router.go` owns the member snapshot and optional read ring. `client.go` and
`server.go` handle Peer reads. `protocol.go` owns shared headers, version parsing
and authentication; `invalidation_client.go` handles one-node notification/ack;
`invalidation.go` owns bounded fan-out over one captured membership snapshot.
The notification protocol remains best effort; reports do not imply global
reader synchronization or durable event delivery.
