# Complete local development system

The primary developer entrypoint is a retained Kind environment containing:
Envoy Gateway and two data-plane proxies, SimpleCache Operator, PostgreSQL 17
with a database PVC and initial product schema/data, and three cache instances.
It uses the same product declarations and provisioning workflow as validation.

## Start and access

Requirements: Linux, Docker, kubectl, Go matching the modules, Python 3.10+,
curl and sha256sum. Fixed Kind/Gateway assets are fetched and verified by the
existing downloader; images are built/loaded locally, never published.

From the repository root:

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r tools/requirements.txt
make dev-up
make dev-status
curl -fsS 'http://127.0.0.1:18080/api?key=product%3A42'
```

`dev-up` creates a unique workspace-owned Kind cluster on first use, waits for
the complete system, initializes PostgreSQL only when its data directory is
empty, and starts a managed loopback port-forward to the shared Gateway Service.
It prints the access URL and private credential file path. An occupied port is
an error, not permission to terminate another process. Override with
`SIMPLECACHE_DEV_PORT=18081 make dev-up`. `PYTHON=/path/to/python` selects a
prepared interpreter if a virtual environment is not activated.

State, kubeconfig, credentials and logs live under ignored `bin/dev/`.
The directory is mode 700; credentials/state files are mode 600. Existing
credentials are reused, not rotated on every start. Missing credentials for an
existing environment fail explicitly rather than silently breaking database
access. Actual Secrets go to kubectl stdin; saved resource renders are redacted.

The API supports conditional JSON writes. Read the current ETag and send an
exact If-Match, or If-None-Match: * for a new key, using the `token` from the
private credentials file. Tokens are not printed by lifecycle commands.

## Stop, restore, migrate and destroy

```bash
make dev-down
make dev-up
```

`dev-down` stops the managed port-forward, deletes the managed SimpleCache CR
and waits for Gateway workloads to disappear while their controllers still run.
It then stops the database Pod and scales the two controllers to zero. It does
not delete the database PVC, credentials, Kind node or control-plane metadata.
Kind itself remains running and consumes some resources. `dev-up` restores the
workloads against the same database volume and credentials. Initial seed data
is not reapplied to existing data. Repeated up refreshes local images and the
managed access forward.

Schema is applied from `deploy/products/schema.sql` at first database init.
If that file changes, up requires an explicit migration instead of pretending
that PostgreSQL init scripts run on an existing volume:

```bash
make dev-down
make dev-schema-upgrade
make dev-up
```

The migration starts the retained database if necessary, requires the managed
cache CR to be absent, and runs schema.sql in one transaction with ON_ERROR_STOP.
It does not reseed values or reset versions. The successful schema hash is
recorded only after completion. This automates the current idempotent schema;
future destructive migrations need their own reviewed SQL and backup plan.

```bash
make dev-destroy
```

Destroy explicitly deletes only the recorded owned Kind cluster and its temporary
proxy tag; the cluster's PVC/data are removed. Normal down/up and Pod replacement
preserve data, while cluster destruction does not. Use pg_dump if data must
survive cluster replacement. Node identity and workspace checks reject a foreign
or recreated same-name cluster. Lifecycle mutations are serialized by a local
lock; status reports an in-progress operation and log path instead of racing it.

## Source of truth and experiment differences

| Definition | Responsibility |
| --- | --- |
| `kind.yaml` | Normal persistent control-plane storage and one Kind node |
| `database.yaml` | PostgreSQL Pod/Service/PVC shape; runtime schema and Secret references |
| `cache.yaml` | GatewayClass and default SimpleCache topology/parameters |
| `../gateway/operator/` | Operator installation with Gateway support |
| `../../tools/system/provision.py` | Shared ordered installation/readiness workflow |
| `../../tools/system/dev.py` | Retained daily lifecycle, credentials and access |

Verification calls the same shared provisioner with explicit differences:
random credentials/cluster per run; no initial business seed; a temporary disk
or 512Mi memory data volume; TTL 300 and capacity overrides where required; and
a test-only tmpfs etcd patch. Its normal report/snapshot/cleanup lifecycle stays
in `tools/validation`. Benchmark Pod shapes and fault actions stay there too.
Daily startup does not invoke a test suite and never auto-deletes its cluster.

The shared default declarations are authoritative. `deploy/gateway/cache.yaml`
was replaced by `deploy/system/cache.yaml`; Compose is still a separate format
for independent local caches and is not the full Gateway development system.
