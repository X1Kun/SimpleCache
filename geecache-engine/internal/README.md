# Runtime boundaries

| Package | Responsibility |
| --- | --- |
| `app` | Pure configuration validation, assembly, public API and graceful lifecycle |
| `cache` | Local loading cache, bounded admission/source queue, LRU/TTL and version fencing |
| `cluster` | Immutable notification-member snapshots, authenticated invalidation delivery and acknowledgements |
| `discovery` | EndpointSlice observations published to the notification directory |
| `source/postgres`, `demo` | Authoritative versioned source and immutable demo source |
| `outbox` | Durable claim/send/finish loop; source adapter owns source/event atomic SQL |
| `telemetry` | Cache and background-work observers, process and queue metrics |
| `bench`, `validation` | Experiment clients, diagnostics and evidence processing |

The cache imports no HTTP, Kubernetes, notification transport or concrete source.
`Group.Get` is its sole value-read entrypoint. Every instance reads locally;
Gateway performs key-based sharding before requests reach the instance. Direct
access to an arbitrary instance is safe under the weak consistency contract but
can create independent copies and lose sharding efficiency.

## App source and tests

App tests use their primary source file as a prefix. Larger HTTP and integration
scenarios keep a descriptive suffix; test functions and shared fixtures remain
in the same package.

| Source | Tests |
| --- | --- |
| `api.go` | `api_test.go` (routes and key validation), `api_protection_test.go` (admission and metrics), `api_response_order_test.go` (lost responses and client version guards) |
| `bootstrap.go` | `bootstrap_test.go` (listeners and shutdown), `bootstrap_outbox_test.go` (relay sender readiness) |
| `config.go` | `config_test.go` (environment parsing, validation and source requirements) |
| `debug.go` | `debug_test.go` (debug address and profile isolation) |
| `mutations.go` | `mutations_test.go` (coordination and shared product fixtures), `mutations_postgres_integration_test.go` (real source lifecycle), `mutations_delivery_integration_test.go` (response loss, crash recovery and Outbox replay) |
| `mutations_http.go` | `mutations_http_test.go` (preconditions and concurrent edits), `mutations_http_invalidation_test.go` (version notifications and write validation) |

PostgreSQL integration tests require `SIMPLECACHE_TEST_DATABASE_URL`; they skip
when no disposable database is configured. The crash-writer subprocess is still
selected by its test function name, independent of its containing file name.

## Read and version invariants

Read: canonical API key -> local lookup -> API miss admission -> source flight ->
source limiter/queue -> one bounded source task -> guarded fill. Hits bypass miss
admission. Source tasks use an 800ms default deadline and a service-owned context;
a caller can cancel its wait without canceling useful shared work. A stale-version
load retries once without extending the original deadline or losing an observed
minimum version. Source executing/pending work has independent finite bounds.

`readFence` is an epoch/minimum snapshot, not a synchronization owner. Version
checking and fill share `epochMu`, with lock order `epochMu -> cache.mu`.
Version metadata is bounded to 1,024 records. Retiring a floor advances its epoch
stripe; an active flight retains its captured minimum even after retirement.
Both linked lists use Front for newest and Back for oldest: cache reads/writes
refresh LRU recency; only insertion or a higher floor refreshes version retention.
Invalidation prevents an old fill but is not a global reader barrier. An older
already waiting caller can still receive a value under the documented contract.

## Notifications and lifecycle

`cluster.Directory` publishes immutable member/client snapshots and never picks
an owner for reads. Its HTTP handler accepts authenticated DELETE invalidations
only, at `/_simplecache/invalidate/<group>/<escaped-key>`. GET cannot fetch values.
EndpointSlice port `cluster` identifies the notification port (normally 8001).
`CLUSTER_SERVICE` and static `MEMBERS` configure notification recipients only.

The `cluster` files follow the notification flow:

- `directory.go`: member normalization and immutable snapshot publication;
  `directory_test.go` covers ownership and concurrent reads.
- `invalidation.go`: the per-member `Client`, bounded broadcast and acknowledgements;
  `invalidation_test.go` covers sending and delivery reports.
- `server.go`: authenticated local invalidation handling;
  `server_test.go` covers request validation, version acknowledgements and read rejection.
- `protocol.go`: shared paths, headers, authentication and HTTP error mapping.

The API mutation coordinator depends on narrow invalidator/observer interfaces.
It validates preconditions, writes the source, then invalidates known members.
Conflicts are known non-commits and do not broadcast. Source errors retain unknown
commit semantics and attempt force invalidation. Caller disconnect cannot cancel
started post-validation coordination; shutdown and source budgets still bound it.
Write admission spans body reading, source, fan-out and response delivery.

All listeners bind before readiness. Discovery and maintenance then start; an
Outbox-capable authenticated source starts a relay. Shutdown marks unready and
drains HTTP before stopping/joining the relay; directory/store close afterwards.
See [Outbox](outbox/README.md) for lease, crash and at-least-once semantics.

## Configuration and testing

`FromEnv` parses defaults/units and identities. `Config.Validate` is pure and does
not contact DNS, PostgreSQL or Kubernetes. `Run` selects/opens sources;
`RunWithSource` validates common runtime constraints without requiring unused
credentials for an injected source. Listener port zero is allowed for fixtures;
advertised member ports must be nonzero. Debug binds literal loopback only.

Obsolete `READ_ROUTING_MODE`, `HOT_CACHE_BYTES`, `PEERS`, and `PEER_SERVICE`
settings fail startup with migration guidance. Runtime, source, notification,
lease, race and transport-boundary tests stay next to their contracts. Old
Peer-routing/hash-ring/remote-copy tests and the separate PostgreSQL routing
comparison runtime were removed. Source-version, admission, cancellation, late
fill, mutation, delivery and crash-recovery tests remain on the local read path.
