#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
mkdir -p "$project_root/bin/reports"
report_dir="$(mktemp -d "$project_root/bin/reports/$(date -u +%Y%m%dT%H%M%SZ)-postgres-compare-XXXXXXXX")"
project="sc-compare-$(basename "$report_dir" | tr '[:upper:]' '[:lower:]')"
compose=(docker compose -p "$project" -f "$project_root/deploy/products/compose.yaml")
owned_database=false
stage=build
cleanup() {
  local code="$?"
  trap - EXIT
  if [[ "$owned_database" == true ]]; then
    "${compose[@]}" logs --tail=100 postgres >"$report_dir/postgres.log" 2>&1 || true
    "${compose[@]}" down --volumes >"$report_dir/cleanup.log" 2>&1 || code=1
  fi
  printf '{"exit_code":%d,"last_stage":"%s"}\n' "$code" "$stage" >"$report_dir/wrapper-status.json"
  if [[ ! -f "$report_dir/report.json" ]]; then
    printf '{"status":"failed","failure":"wrapper stopped during %s; inspect build/database logs"}\n' "$stage" >"$report_dir/report.json"
    printf '# PostgreSQL comparison\n\nStatus: **failed** during %s.\n' "$stage" >"$report_dir/report.md"
  fi
  echo "Comparison artifacts: $report_dir"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cd "$project_root/geecache-engine"
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"
go build -o "$report_dir/postgres-fixture" ./cmd/postgres-fixture >"$report_dir/build-fixture.log" 2>&1
go build -o "$report_dir/postgres-compare" ./cmd/postgres-compare >"$report_dir/build-driver.log" 2>&1
stage=database
if [[ -z "${SIMPLECACHE_COMPARE_DATABASE_URL:-}" ]]; then
  owned_database=true
  export PRODUCT_DB_PORT=0
  "${compose[@]}" up -d --wait --wait-timeout 180 postgres >"$report_dir/database-start.log" 2>&1
  address="$("${compose[@]}" port postgres 5432)"
  export SIMPLECACHE_COMPARE_DATABASE_URL="postgres://simplecache:learning-only-password@$address/simplecache?sslmode=disable"
fi
repeats="${COMPARE_REPEATS:-3}"
passes="${COMPARE_PASSES:-2}"
quick=false
if [[ "${COMPARE_QUICK:-0}" == 1 ]]; then
  quick=true
  repeats="${COMPARE_REPEATS:-1}"
  passes="${COMPARE_PASSES:-1}"
fi
stage=comparison
"$report_dir/postgres-compare" -server "$report_dir/postgres-fixture" -dir "$report_dir" \
  -repeats "$repeats" -passes "$passes" -workers "${COMPARE_WORKERS:-12}" \
  -keys "${COMPARE_KEYS:-512}" -value-bytes "${COMPARE_VALUE_BYTES:-4096}" \
  -cache-bytes "${COMPARE_CACHE_BYTES:-1048576}" -seed "${COMPARE_SEED:-42}" \
  -zipf-reads "${COMPARE_ZIPF_READS:-8192}" -mixed-windows "${COMPARE_MIXED_WINDOWS:-30}" \
  -quick="$quick" | tee "$report_dir/run.log"
stage=complete
