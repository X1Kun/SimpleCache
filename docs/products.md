# Read-mostly product cache

## Business scope

A product catalog receives repeated detail reads and occasional product edits.
PostgreSQL owns the durable JSON records. Cache Pods hold only the recently
loaded subset. Consistent hashing directs misses toward a preferred node,
reducing duplicated fills under stable membership; SingleFlight combines
concurrent loads there. Each Pod can rebuild a value if its peer is unavailable.

The learning objective is to measure source work avoided and observe failure
semantics. This implementation does not claim a throughput improvement or that
hashing is the only way to cache a dataset larger than one Pod.
The default seed contains 1,000 small products for functional testing.

## Run the complete example

From the repository root:

```bash
docker compose -f deploy/products/compose.yaml up -d --build
curl -fsS http://127.0.0.1:19999/readyz
curl -fsS 'http://127.0.0.1:19999/api?key=product:42'
curl -fsS 'http://127.0.0.1:19999/api?key=product:42'
```

Compose creates PostgreSQL, a persistent named volume, and three cache nodes.
Each cache has a 16MiB logical limit and a 256MiB container memory limit; these
limits measure different things. The PostgreSQL port is dynamically assigned
on loopback. PRODUCT_API_PORT changes the API port.
The normal geecache-engine/docker-compose.yml remains the immutable demo.
The database health check uses TCP: the socket-only server used during first-run
initialization cannot release dependent cache containers before schema/seed setup
has finished.

Example-only credentials are in the Compose file. Set CACHE_ADMIN_TOKEN to
override its learning token. All nodes must use the same token and database.
The sample publishes only loopback ports; use a trusted network and TLS
termination when exposing authenticated mutation endpoints elsewhere.

Update a product through the cache API:

```bash
curl -i -X PUT 'http://127.0.0.1:19999/api?key=product:42' \
  -H 'Authorization: Bearer learning-only-token' \
  -H 'Content-Type: application/json' \
  --data '{"id":42,"name":"Updated product","price_cents":12000}'

curl -fsS 'http://127.0.0.1:19999/api?key=product:42'
docker compose -f deploy/products/compose.yaml exec -T cache-2 \
  wget -qO- 'http://127.0.0.1:9999/api?key=product:42'
docker compose -f deploy/products/compose.yaml exec -T cache-3 \
  wget -qO- 'http://127.0.0.1:9999/api?key=product:42'
```

The write commits to PostgreSQL first. It then clears the initiating node and
notifies every other member in one routing snapshot. HTTP 200 includes:

```json
{
  "source_committed": true,
  "invalidation": {
    "scope": "known_members_snapshot",
    "complete": true,
    "results": [
      {"node": "cache-1:8001", "status": "ok"},
      {"node": "cache-2:8001", "status": "ok"},
      {"node": "cache-3:8001", "status": "ok"}
    ]
  }
}
```

To observe partial delivery, stop cache-3 and repeat the PUT. Static membership
still contains that node, so the response is HTTP 202, source_committed=true,
complete=false, with its failed target reported. The database write is **not**
rolled back. No background retry is scheduled.

```bash
docker compose -f deploy/products/compose.yaml stop cache-3
# Repeat the PUT above, then:
docker compose -f deploy/products/compose.yaml up -d --no-deps cache-3
curl -i -X POST 'http://127.0.0.1:19999/invalidate?key=product:42' \
  -H 'Authorization: Bearer learning-only-token'
```

POST /invalidate is an explicit retry of cache invalidation; it does not modify
the source. It is also the hook for a writer that updates PostgreSQL directly.
Such external writes otherwise become visible after cache expiration/reload.

DELETE /api removes the database row and broadcasts invalidation:

```bash
curl -i -X DELETE 'http://127.0.0.1:19999/api?key=product:42' \
  -H 'Authorization: Bearer learning-only-token'
curl -i 'http://127.0.0.1:19999/api?key=product:42'
```

The subsequent read returns 404 in the quiescent, successfully invalidated case.
Stopping/recreating cache containers does not remove PostgreSQL data.
`docker compose -f deploy/products/compose.yaml down` retains the database volume;
only add `--volumes` when you intend to erase this example's database.

## API and consistency contract

| Operation | Meaning |
| --- | --- |
| GET /api?key=... | Read through local cache / preferred peer / source |
| PUT /api?key=... | Upsert a JSON value, then invalidate cached copies |
| DELETE /api?key=... | Delete the source row idempotently, then invalidate |
| POST /invalidate?key=... | Invalidate cached copies only |
| Peer DELETE /_geecache/products/... | Authenticated local invalidation; no forwarding |

Keys are 1–256 bytes; JSON values are at most 1MiB. Writes are enabled only for
a mutable source and a configured token. Demo mode remains read-only.
The database schema enforces key/value size bounds too.

HTTP 200 means all targets of this attempt acknowledged, not that every node
that could exist is synchronized. HTTP 202 reports partial delivery and does not
promise eventual processing of that notification. Write errors return 503/504
with source_committed=null because a connection failure can leave commit status
unknown; verify the source before retrying the business operation.

An invalidation epoch protects against old local loads refilling the cache.
Post-invalidation callers use different routing/source flight identities.
Already-running callers may return old results. Epochs are group-wide and do
not grow a map of historical keys: an invalidation can also cause unrelated
active fills to be skipped/repeated, but does not evict unrelated cached entries.

Broadcast is best effort: discovery can omit an unready/disconnected member,
nodes can fail, and the writer can crash between commit and notification.
Concurrent distributed reads can overlap notifications at different nodes;
successful broadcast is not a simultaneous read barrier. TTL remains the
fallback for stale copies. This is a weakly consistent read cache, not a
transactional or read-after-write consistent database.
No durable event log, automatic replay, or replicated cache storage is claimed.

The existing fallback policy can still multiply source loads during peer
failures; the source concurrency limit is per process. The 1s peer read timeout
accommodates the 800ms source budget, but a late fallback can exceed the remaining
1.2s route budget. Independent source work can finish and cache later.

## PostgreSQL and Kubernetes configuration

For an existing dedicated database, apply schema.sql explicitly. Startup checks
connectivity and schema before serving; it never silently falls back to demo.

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f deploy/products/schema.sql
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v rows=10000 -f deploy/products/seed.sql
```

The seed is idempotent and does not overwrite existing product edits.
All values are fetched by parameterized point queries, never loaded into each
Pod at startup. PostgreSQL mode disables the static Bloom filter, so new keys
are readable immediately after a successful write/invalidation.

For the Operator, create a same-namespace Secret with DATABASE_URL and
CACHE_ADMIN_TOKEN, then set spec.sourceSecret to its name:

```yaml
apiVersion: cache.x1kun.com/v1
kind: SimpleCache
metadata:
  name: products
spec:
  size: 3
  image: your-built-simplecache-image
  sourceSecret: product-source
  cacheBytes: 67108864
  ttlSeconds: 60
```

The Operator injects Secret references; it does not read credentials or create
the database/schema. Changing the reference changes the Pod template. Updating
the Secret contents alone requires a controlled Pod restart; hot credential
rotation is not implemented. Removing sourceSecret restores demo mode.

## Validation

```bash
cd geecache-engine
go test -race ./...
SIMPLECACHE_TEST_DATABASE_URL='postgres://.../dedicated_test_db?sslmode=disable' \
  go test -race -count=1 -run '^TestPostgres(OpenChecksExecutionErrors|ProductLifecycle)$' \
    -v ./internal/source/postgres ./internal/app
```

Apply schema.sql to that test database first. Without the environment variable,
the integration test is reported as skipped. The test uses a unique key and
deletes only that key. CI provides an isolated PostgreSQL service.
The startup test creates and removes its own temporary schema, checking both
preparation failures and execution errors delivered through rows.Err(). It needs
permission to create schemas in the dedicated test database.

From the repository root, `bash scripts/products/check-startup.sh` verifies fresh
volume startup while initialization is deliberately paused. It starts only an
isolated PostgreSQL container with a random loopback port and removes its own
container, network and volume on exit. CI runs this check too.

Coverage includes local invalidation during blocked source/peer loads,
new-flight isolation, idempotence and byte accounting, three-node historical
copies, authorization/body limits, committed writes with partial broadcast,
ambiguous source errors, and caller disconnect after commit. The real database
test checks a second independent pool, update/read/delete, and manual invalidation.

Metrics include simplecache_source_loads_total, simplecache_source_writes_total,
simplecache_invalidations_total and simplecache_invalidation_broadcasts_total.
Compare source-read deltas across **all nodes**, not only the API entry node.
Use the same total cache memory and request sequence when comparing independent
caches with hash-coordinated caches. No production hit rate or QPS is implied by
the small functional seed.
