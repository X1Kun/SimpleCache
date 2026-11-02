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

## Reading order

1. `cmd/simplecache/main.go` -> `app/config.go`: environment binding and validation.
2. `app/bootstrap.go`: source selection, component assembly, listeners and drain.
3. `app/api.go` -> `cache/read.go`: API entry, lookup, route flight, Peer attempt,
   fallback, source flight and the single stale-version retry.
4. `cache/load.go` -> `cache/versions.go` -> `cache/cache.go`: admitted source
   work, version/epoch guards, atomic fill and storage/expiry.
5. `app/mutations_http.go` -> `app/mutations.go` -> `peer/invalidation.go`:
   write admission/preconditions, atomic source change and best-effort delivery.

## Configuration contract

`FromEnv` applies defaults, parses strings/units and creates member identities,
then uses `Config.Validate` and the real-source requirements. Numeric syntax and
unit-conversion overflow are rejected during parsing. Explicit environment zero
TTL/waiter/write budgets remain invalid; zero `SOURCE_MAX_PENDING` remains valid.

`Config.Validate` is a pure common-runtime check: it does not read environment,
mutate the receiver, resolve DNS or contact Kubernetes/PostgreSQL. It checks
bounds, listener syntax, required identity fields and combinations. Listener
ports are numeric 0-65535; advertised Peer member ports must be 1-65535.
API address may be empty only when the API is disabled. Debug stays loopback-only.

Both startup entrypoints call it before assembly. `Run` additionally validates
real-source credentials before opening PostgreSQL. `RunWithSource` requires a
non-nil Getter and does not require unused database credentials, even when
SourceMode is postgres. Injected custom source modes also bypass the built-in
factory enum check; postgres selects group products, all other modes select
scores, preserving the previous naming behavior.

Programmatic zero TTL, miss-waiter and write-concurrency values retain their
existing component defaults; zero source pending disables queuing. Positive
programmatic TTL may use subsecond durations; TTL_SECONDS accepts integer
seconds. Cache capacity and executing-source concurrency must be explicitly
valid in programmatic configurations. Invalid direct configurations now return
errors before allocation/binding instead of reaching later component errors or
panics. Source/read routing empty modes retain their existing demo/peer meaning.

## Timeout and cancellation index

| Scope | Symbol / location | Default |
| --- | --- | --- |
| API read wait | `apiReadTimeout`, app/api.go | 1500ms |
| Shared route task | `defaultRouteTimeout`, cache/group.go | 1200ms |
| Peer HTTP read | `peerReadTimeout`, app/bootstrap.go | 1000ms |
| Shared source task, including queue/retry | `defaultSourceTimeout`, cache/group.go | 800ms |
| Mutation coordination | `mutationTimeout`, app/mutations.go | 3s |
| Database write, including pool wait | `mutationSourceTimeout`, app/mutations.go | 800ms |
| Invalidation fan-out / one-node notification | `InvalidationTimeout`, peer/invalidation.go | 1s |
| PostgreSQL startup check | `sourceStartupTimeout`, app/bootstrap.go | 5s |
| Header/body/idle connection | `readHeaderTimeout` / `requestReadTimeout` / `connectionIdleTimeout`, app/bootstrap.go | 3s / 5s / 30s |
| Response write | `responseWriteTimeout`, app/bootstrap.go | 10s |
| HTTP drain | `shutdownGracePeriod`, app/bootstrap.go | 5s |

Identical durations do not imply identical policies: database writes and reads
have independent budgets. These constants introduce no new environment options.
Fixture/client deadlines are separate measurement settings.

The read budgets are not a serial sum. Route and source flights have independent
service-owned contexts. A caller can stop waiting while shared work finishes;
a late Peer failure may leave too little route wait time for fallback to return
to that caller. Caller cancellation does not Forget flights or cancel useful
shared work. Original deadlines/admission bounds still constrain tasks. Shutdown
marks unready and drains HTTP before canceling shared work.

`readEntry` names API, Peer and internal fallback ownership. Internal fallback
already holds outer admission; it must not consume a second API/Peer waiter.
The two SingleFlight layers and version check/fill lock boundaries are unchanged.

An epoch change prevents an older flight from filling cache but does not alone
forbid returning its value to an already waiting caller. Observed version floors
separately reject lower versions; source loads may retry once within the original
source deadline. Rejection may still end as 503. Floor retirement cannot weaken
an active flight's captured minimum. Missed notifications and in-flight reads
still allow old values under the documented weak consistency contract; this is
not a latest-value or linearizable-read API.
