# SimpleCache design

Current state: reliable reads, static/EndpointSlice membership, atomic snapshots, and the Operator discovery/resource contract are implemented. API-server/race tests validate the controller; the original Kind integration smoke passed locally. Expanded scenarios generate independent run evidence. See [the plan](plan.md) and [README](../README.md).

## Responsibilities

The cache process handles reads, source protection, routing, and member discovery. The Operator expresses desired Kubernetes resources. Kubernetes maintains EndpointSlices; cache nodes consume them directly.

| Package | Responsibility |
| --- | --- |
| cmd/simplecache | CLI, process signals |
| internal/app | Configuration, API, listeners, shutdown |
| internal/cache | LRU/Bloom policy, coalescing, source limits |
| internal/peer | HTTP/Protobuf, routing snapshots, hashing |
| internal/discovery | Service-selected EndpointSlice informer |
| internal/demo | Finite immutable source |
| internal/telemetry | Per-process metrics registry |
| internal/diagnostics | Bounded independent-process investigation tooling |

Engine paths are relative to geecache-engine. The Operator retains Kubebuilder's api/cmd/internal/config layout. Each component has one Go module. cache owns source, peer, and observer interfaces without importing HTTP, Protobuf, or Kubernetes.

## Read path

API: validate key → local LRU/Bloom → routeFlight → preferred peer or local source.

Peer handlers call GetLocal only: local LRU/Bloom → originFlight → second lookup → source slot → Getter → cache result. They never forward to another peer.

Separate routing/source SingleFlight groups avoid recursive same-key waits. Caller cancellation stops its own wait; bounded shared work is not canceled by the first caller. Forget is not used for caller timeouts.

| Budget | Default |
| --- | --- |
| API wait | 1500ms |
| Shared route | 1200ms |
| One peer read | 1000ms |
| Source work, including slot wait | 800ms |
| Concurrent Getter calls | 32 per process |

Fallback waits within the existing route budget. Independent source work may finish later within its own deadline and cache success. Source implementations must honor context cancellation. One limiter is shared across application groups, and coalesced waiters do not each acquire slots.

## Peer errors and size bounds

Success uses Protobuf. Only 404 plus KEY_NOT_FOUND is a terminal business miss. GROUP_NOT_FOUND, untyped 404, transport errors, invalid Protobuf, and oversized responses may fall back to the local source. Invalid input returns 400, source failures 503, and timeouts 504.

Keys contain 1–256 bytes. Values are limited to 1MiB; encoded bodies use a separate 1MiB+1024 bound. Client reads are bounded before decoding, and value size is checked again afterward. Path escaping preserves spaces, plus signs, slashes, and Unicode.

Peer success is not copied into the entry cache. Successful fallback values may remain locally until expiration or eviction.

## Membership and publication

A namespaced informer selects all IPv4 Pod EndpointSlices for the peer Service. It accepts ready=true or unknown readiness, rejects ready=false/terminating endpoints, and validates references, addresses, and the named TCP port.

Identity is namespace/podName; IP/port is transport. Replacing a Pod IP does not change ownership. Inputs are sorted/deduplicated deterministically; equal inputs do not republish. Hash positions and collision handling are deterministic.

A single worker builds a complete immutable ring/client/member snapshot and publishes it through atomic.Pointer. Readers load once and obtain owner and client from that same version. Published maps, slices, and ring data are never mutated.

Initial informer sync and valid publication are readiness dependencies; self-endpoint publication is not. A valid empty set publishes an empty ring and uses local source reads. Discovery failures retain the last valid view while bounded peer fallback covers staleness.

## Cache, lifecycle, and metrics

Bloom is built from the complete immutable key set before serving. Generic sources without that set disable it. Known keys must never be rejected; positive Bloom results still require lookup.

Mutable sources always disable startup Bloom, even if they also expose Keys().
The application only requires Getter; the complete immutable key set is optional.

Default logical capacity is 64MiB and TTL 60s. Expiration is lazy; accounting covers key/value bytes, not RSS. Values are copied to isolate cached storage.

All listeners bind before readiness. Shutdown clears readiness, drains HTTP requests, then cancels shared work.

Metrics distinguish API/peer local lookups, actual peer requests, actual Getter calls, source-slot waits/inflight, Bloom rejection, fallback, and members/publications/errors. Waiters are not extra source loads. Labels do not include keys, arbitrary URLs, or error text.

DEBUG_ADDR is disabled by default and accepts only literal loopback IPs. Its
separate explicit mux serves profiles, never the public API/peer mux. All listeners
bind before startup and drain during shutdown. Mutex/block sampling is opt-in for
profiled fixture runs only. No Operator Service exposes the diagnostic port.

Manual diagnostics use a separate client and fresh server subprocess per trial.
The fixture shares RunWithSource with normal startup, changing only the declared
source padding/capacity for pressure experiments. Unprofiled repeated baselines
and profiled diagnostic trials are separate; process PIDs and the matched binary
digest prevent attribution to the load generator. Reported resource deltas exclude
warmup but include periodic metrics scraping. Logical cache bounds do not constrain
the immutable source's dataset, Go overhead or RSS.

## Operator integration

The controller now reconciles the dynamic discovery contract:

- Manage peer/API Services, StatefulSet, cache account, and namespace discovery Role/Binding.
- Handle conflicts/errors, repair managed drift, and watch children.
- Add resources, startup/readiness/liveness probes, and meaningful Status.
- Preserve allocated/immutable fields and avoid default-induced update loops.
- Remove PEERS from the template so scaling changes replicas only.
- Regenerate CRD/RBAC/DeepCopy and update envtest assertions.

Ready Status describes observed Kubernetes rollout, not simultaneous ring agreement. InvalidSpec, ResourceConflict and ReconcileFailed expose failures. Status patches occur only on actual changes. No external resources require a custom finalizer. Keep manual CR scaling until there is one authoritative autoscaling interface.

New StatefulSets use Parallel management; existing compatible immutable policy/identity fields are preserved. Resource defaults respect explicitly smaller limits, and invalid request/limit combinations prevent workload creation. Managed Pod fields are merged without erasing API defaults or unrelated fields. Static PEERS and SELF_ADDR are removed from the cache environment. Optional sourceSecret injects PostgreSQL mode and two Secret key references; removing it restores demo mode.

## Product writes and invalidation

PostgreSQL is the authoritative product source. PUT /api persists JSON and
DELETE /api removes the database row; POST /invalidate only clears cached
copies. All mutations require the configured shared bearer token.
The write handler uses a lifecycle-owned 3s context: source work has an 800ms
budget and broadcast a 1s budget. Caller disconnect does not cancel the
post-commit broadcast. A write error can have an ambiguous commit outcome,
so the handler attempts invalidation even then.

Broadcast takes one immutable member snapshot, invalidates the initiating node
even if absent from that snapshot, and uses up to eight workers to contact
other members. Each peer performs local invalidation only; it never rebroadcasts.
Results identify failed/timed-out targets. HTTP 200 means every target in this
snapshot acknowledged; 202 means incomplete best-effort invalidation, with no
background delivery promise. Neither response establishes global consensus.

Each group has 256 fixed, mutex-protected epoch stripes. Invalidation increments
the requested key's stripe and removes its main/hot entries under the same lock
used to validate source and peer-result insertion. Both SingleFlight keys include
the stripe epoch, so calls beginning after local invalidation do not join older
flights. Earlier callers may still receive earlier results. Fixed stripes use
2KiB of generation counters per group, without a historical-key map. Collisions
conservatively fence unrelated active loads in the same stripe; unrelated cached
values remain intact. A single-stripe diagnostic option reproduces the former
behavior for comparisons, not as an application configuration.

Optional HOT_CACHE_BYTES reserves part of CACHE_BYTES for short-lived peer
replicas; zero disables it. The remainder stays available to source fills.
Replicas use LRU admission, must fit their reserved budget, and expire at the
minimum of their inherited owner lifetime and one second. Peer responses carry
remaining TTL; the client conservatively consumes the entire request duration.
Missing TTL metadata permits reads but prevents replica admission. This avoids
extending owner expiry or requiring synchronized clocks. Both caches participate
in local invalidation and generation checks. A successful source fill removes
any older hot copy. Oversized values are returned without flushing other entries.
The extra replicas trade owner capacity and bounded temporary duplication for
fewer peer RPCs; they do not add a global read barrier.

ByteView.WriteTo writes its immutable bytes directly to the HTTP writer under
the io.Writer contract. ByteSlice retains its defensive-copy behavior.

Notifications are not durable. Offline/undiscovered nodes, a crash between DB
commit and broadcast, and overlapping reads/writes can expose stale data.
In particular, per-node invalidation is not a simultaneous cluster barrier:
an in-flight routed read can overlap another node's notification.
TTL and bounded in-flight work limit retention of old results when writes stop,
the source is current/reachable, and subsequent loads succeed. No read-after-write
or linearizability guarantee is made. See [the walkthrough](products.md).

## Boundaries and evidence

The demo source is read-only and identical across nodes, with 100ms simulated latency. PostgreSQL mode supplies persistent mutable products, independently of cache memory. There is no durable replication, active migration, durable invalidation, or strong consistency; optional hot replicas are disposable read accelerators. SingleFlight and source limits are process-local. Membership converges eventually.

Unit/race tests and fake discovery validate local contracts. Envtest additionally checks real API defaulting, allocated-field preservation, Status, conflicts, scaling without template changes, and Manager child-watch repair. It does not prove deployed permissions or Pod networking. The original dedicated Kind smoke passed locally. Expanded Kind tests require error-free reads during scaling/Pod replacement and recovery from infeasible resource requests; each run reports actual outcomes. Local diagnostics measure logical capacity, hash movement, source limits and latency, with test-process profiles rather than a publicly exposed admin endpoint. Grafana provisioning and deployed-process profiling remain separate follow-up work.

No throughput, GPU, scheduler-plugin, or production-availability claim follows from these tests. Keep advanced scheduling and AI workloads as separate follow-up work.

References: [EndpointSlices](https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/), [SingleFlight](https://pkg.go.dev/golang.org/x/sync/singleflight), [envtest](https://book.kubebuilder.io/reference/envtest).
