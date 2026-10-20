#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d /tmp/simplecache-startup.XXXXXXXX)"
project="$(basename "$scratch" | tr '[:upper:].' '[:lower:]-')"
compose=(docker compose -p "$project" -f "$project_root/deploy/products/compose.yaml" -f "$scratch/override.json")
cleanup() {
  local code="$?"
  trap - EXIT
  if ((code != 0)); then "${compose[@]}" logs --tail=60 postgres >&2 || true; fi
  "${compose[@]}" down --volumes >/dev/null || code=1
  rm -f -- "$scratch/pause.sh" "$scratch/override.json"
  rmdir "$scratch"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cat >"$scratch/pause.sh" <<'SCRIPT'
# Hold the temporary socket-only server before schema/seed initialization.
touch /tmp/simplecache-init-paused
while [ ! -f /tmp/simplecache-release-init ]; do sleep 0.2; done
SCRIPT
cat >"$scratch/override.json" <<JSON
{"services":{"postgres":{"volumes":[{"type":"bind","source":"$scratch/pause.sh","target":"/docker-entrypoint-initdb.d/00-startup-pause.sh","read_only":true}]}}}
JSON
export PRODUCT_DB_PORT=0
"${compose[@]}" up -d postgres
container="$("${compose[@]}" ps -a -q postgres)"
deadline=$((SECONDS + 60))
until "${compose[@]}" exec -T postgres test -f /tmp/simplecache-init-paused >/dev/null 2>&1; do
  if ((SECONDS >= deadline)); then echo 'Initialization did not reach the pause' >&2; exit 1; fi
  sleep 0.2
done
# Prove the old socket probe would already pass during this pause.
"${compose[@]}" exec -T postgres pg_isready -U simplecache -d simplecache
# Observe two completed health checks while initialization remains blocked.
# Two avoids counting a check that had started before the pause was observed.
previous="$(docker inspect --format '{{range .State.Health.Log}}{{.Start}}{{end}}' "$container")"
for check in 1 2; do
  while true; do
    current="$(docker inspect --format '{{range .State.Health.Log}}{{.Start}}{{end}}' "$container")"
    if [[ "$current" != "$previous" ]]; then previous="$current"; break; fi
    if ((SECONDS >= deadline)); then echo 'Health check did not run' >&2; exit 1; fi
    sleep 0.2
  done
done
if [[ "$(docker inspect --format '{{.State.Health.Status}}' "$container")" == healthy ]]; then
  echo 'Database became healthy before initialization completed' >&2
  exit 1
fi
echo 'PASS: temporary socket server is available, but database is not healthy'
"${compose[@]}" exec -T postgres touch /tmp/simplecache-release-init
"${compose[@]}" up -d --wait --wait-timeout 60 postgres
rows="$("${compose[@]}" exec -T -e PGPASSWORD=learning-only-password postgres \
  psql -h 127.0.0.1 -U simplecache -d simplecache -Atc 'SELECT count(*) FROM simplecache_products')"
test "$rows" = 1000
echo 'PASS: database is healthy over TCP after schema and seed initialization'
