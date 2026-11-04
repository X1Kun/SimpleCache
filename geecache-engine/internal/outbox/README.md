# Durable version notification Outbox

PostgreSQL adapter mutations (`PutIfVersion`, `DeleteIfVersion`, and trusted
`Put`/`Delete`) change the product row and insert `(key, committed version)` into
`simplecache_outbox` in one SQL statement/implicit transaction. A failed condition
inserts no event. Failed event insertion rolls the product change back too.
Events contain no product payload, token or client response. Raw SQL/external
writers must explicitly maintain the same data/version/event transaction;
there is no trigger/CDC and no retroactive event backfill.

## Delivery scope

**One queue in one PostgreSQL search_path serves one cache invalidation domain.**
All workers claiming it must have the same group, credentials and membership
domain. Sharing the queue between independent SimpleCache deployments is not
supported; separate subscriptions/per-target progress would be needed.

API synchronous invalidation remains the low-latency path. HTTP 200/202 and
`source_committed` retain their existing meaning for that attempt's snapshot.
The queued event is still delivered independently, so even a successful
synchronous broadcast can be repeated. Conditional-write conflicts remain 412;
there is no idempotent result replay.

Production `RunWithSource` starts one worker when the source implements Queue
and the node has an admin token. Normal PostgreSQL Run supplies both. Read-only
injected sources without notification credentials do not consume the queue.
Workers wait for application/discovery readiness. Completion requires all
members in one captured **nonempty** directory snapshot to acknowledge the
version; legacy unversioned acknowledgements are incomplete. Unseen/offline
holders, membership changes after completion, and newly joining warm holders
still rely on TTL/recovery. This is not global reader synchronization.

## Lease protocol

- Claim one eligible event using `FOR UPDATE SKIP LOCKED`, incrementing its
  attempt/lease token. Claim is one short database statement.
- Send outside database transactions with the existing bounded cluster notification fan-out.
- Finish only if id/token still match and the lease has not expired. Successful
  delivery sets delivered_at; failure releases the lease with delayed eligibility.
- Expired or unacknowledged claims are reclaimable. A stale worker cannot finish
  or reschedule work owned by a newer token. SQL timeouts can be ambiguous; lease
  recovery covers a claim/ack whose result was lost.

Defaults: one worker per process, 1s idle/error poll, 10s lease, 2s send budget,
1s per storage operation, retry delay 1s doubling to a maximum 60s. Events are
never discarded for exceeding an attempt count. Sender/Queue implementations
must honor context cancellation. Worker claims continue immediately while work
is available. Row IDs are identities, not commit-order watermarks; delivery can
be out of order and repeated, using the cache's existing version-aware rules.

## Lifecycle and maintenance

The worker uses the existing Store pool, so its queries share that pool's budget.
It is canceled/joined after HTTP drain and before Directory/Store close. Cancellation
can leave a lease; another worker recovers it after expiry. No environment knobs
or distributed scheduler are introduced in this version.

Every 30s, each worker samples pending count/oldest age and deletes completed events older than seven days in 1,000-row batches
within one shared 1s cleanup budget. Pending events are never purged. Seven
days is deletion eligibility, not an exact retention deadline; a large delivered
backlog can take longer to remove. Maintenance SQL has a 1s context deadline;
pending counts scan the partial pending index and are not constant-time under
large backlog. Monitor database storage/queue age as well as operation errors.

Metrics: `simplecache_outbox_operations_total{operation,result}`,
`simplecache_outbox_pending_events`, and
`simplecache_outbox_oldest_pending_seconds`. Backlog gauges are per-process
sampled views of the same queue; do not sum them across workers. Labels contain
no keys, versions, IDs, credentials or error text. Getter counters exclude relay
polling/maintenance SQL, so zero source loads does not mean zero DB requests.

## Upgrade and validation

Apply `deploy/products/schema.sql` with old writers stopped, then start upgraded
nodes. Existing volumes do not auto-run init scripts. Startup checks require the
queue columns, and all adapter writers need queue permissions. Completed product
rows and versions are retained on migration/reapplication.

Tests cover source/event atomic rollback, conflicts and delete/recreate versions;
concurrent claims, retry eligibility, token fencing and retention; worker cancel;
a real SIGKILL after SQL commit followed by a new application runtime's replay;
and lost post-send acknowledgement followed by lease takeover/duplicate delivery
without evicting a newer value. The no-relay TTL contract remains tested separately.

Monitor `simplecache_outbox_purge_eligible_events` and `simplecache_outbox_table_bytes` (table plus indexes); these are sampled shared-queue views and must not be summed across workers.
