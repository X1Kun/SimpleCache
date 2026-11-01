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

- `bootstrap.go`: source selection, component wiring, background tasks, all-listener binding/rollback and readiness/drain.
- `api.go`: HTTP routes, public API options and canonical query-key parsing.
- `mutations_http.go`: authentication, validation, body limits, write admission,
  ETag preconditions and HTTP response encoding.
- `mutations.go`: transport-independent mutation commands/outcomes, source writes
  and post-commit/best-effort notification coordination.

Write admission spans body reading, source work, notification and HTTP response.
Caller disconnection does not cancel a started write's notification; service
shutdown still bounds work. Conflicts are known non-commits and do not broadcast.
Source failures keep unknown-commit semantics and attempt force invalidation.
Existing exported entrypoints and JSON/status contracts remain unchanged.

## Cache

- `group.go`: Group, options/defaults, construction and aggregate statistics.
- `interfaces.go`, `errors.go`: external contracts, errors and boundary validation.
- `read.go`: lookup, routing, flight coalescing, fallback and bounded retry.
- `load.go`: one admitted source attempt and guarded cache fill.
- `versions.go`: version floors, invalidation, epochs and Peer admission.
- `cache.go`: ByteView, local storage and bounded expiration maintenance.
- `admission.go`: API/Peer waiter admission and source concurrency/queue limits.
- `bloomfilter.go`, `lru/`: membership filtering and LRU storage primitives.

`readFence` is a value snapshot of epoch/minimum, not another synchronization
owner. `sourceResult.minimum` carries the highest floor observed during a load.
Retry must retain it even when metadata is evicted. Shared state stays in Group;
lock order remains `epochMu -> cache.mu`, and version checking/fill admission is
one critical section. Caller cancellation, original deadlines, source limits,
TTL and hot-copy rules must remain intact when changing these files.

## Peer

`router.go` owns the member snapshot and optional read ring. `client.go` and
`server.go` handle Peer reads. `protocol.go` owns shared headers, version parsing
and authentication; `invalidation.go` keeps one-node notification/ack alongside
bounded fan-out over one captured membership snapshot.
The notification protocol remains best effort; reports do not imply global
reader synchronization or durable event delivery.

## File organization

Keep code together when readers need it for the same behavior. A short file can
remain separate when its responsibility is independent (for example debug.go).
Read/load/version logic and complex version/overload tests remain separate.

App tests are grouped by behavior: lifecycle tests include listener rollback and
blocked response writes; config tests cover parsing/defaults; API tests cover
routing/input behavior; mutation and conditional tests cover their respective
write contracts. Tests remain next to implementation in the same package.
