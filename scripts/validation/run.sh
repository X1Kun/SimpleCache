#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
mode="${1:-local}"
case "$mode" in local|unit|race|operator|kind|perf) ;; *) echo "Usage: $0 [local|unit|race|operator|kind|perf]" >&2; exit 2;; esac
cd -- "$project_root"
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"
mkdir -p bin/reports
report_dir="$(mktemp -d "$project_root/bin/reports/$(date -u +%Y%m%dT%H%M%SZ)-$mode-XXXXXX")"
echo "Report directory: $report_dir"
# Build the reporter before any disruptive scenario starts.
(cd geecache-engine && go build -o "$report_dir/reporter" ./cmd/validation-report)
active_stage=""; started=0; recorded=false
finish() {
  local code="$?"
  trap - EXIT
  if [[ -n "$active_stage" && "$recorded" == false ]]; then
    printf '%s\t%s\t%s\n' "$active_stage" "${code:-1}" "$((SECONDS-started))" >>"$report_dir/stages.tsv"
  fi
  if ! "$report_dir/reporter" -dir "$report_dir"; then code=1; fi
  echo "Report: $report_dir/report.md"
  exit "$code"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
stage() {
  active_stage="$1"; shift; started=$SECONDS; recorded=false
  local code=0
  "$@" 2>&1 | tee "$report_dir/$active_stage.log" || code=$?
  printf '%s\t%s\t%s\n' "$active_stage" "$code" "$((SECONDS-started))" >>"$report_dir/stages.tsv"
  recorded=true
  return "$code"
}
local_race() {
  cd "$project_root/geecache-engine"
  go test -json -race -count=1 -timeout=3m -coverprofile="$report_dir/coverage.out" ./... | tee "$report_dir/engine-race.jsonl"
}
local_unit() {
  cd "$project_root/geecache-engine"
  go test -json -count=1 -timeout=3m ./... | tee "$report_dir/engine-unit.jsonl"
}
operator_tests() {
  # Distinguish fresh runs requiring structured evidence from legacy logs.
  touch "$report_dir/ginkgo-required"
  make -C "$project_root/simplecache-operator" test-ci GINKGO_JSON_REPORT="$report_dir/ginkgo.json" || return "$?"
  cp "$project_root/simplecache-operator/cover.out" "$report_dir/operator-coverage.out"
}
diagnostics() {
  # Functional scenarios: profiles include the test client and assertions.
  # Use the separate-process perf mode for server measurements.
  cd "$project_root/geecache-engine"
  go test -json -count=1 -timeout=2m -o "$report_dir/scenarios.test" \
    -cpuprofile="$report_dir/cpu.pprof" -memprofile="$report_dir/heap.pprof" \
    -mutexprofile="$report_dir/mutex.pprof" -blockprofile="$report_dir/block.pprof" \
    ./internal/validation/scenarios | tee "$report_dir/diagnostics.jsonl"
}
performance_build() {
  cd "$project_root/geecache-engine" || return "$?"
  go build -o "$report_dir/diagnostic-server" ./cmd/diagnostic-server || return "$?"
  go build -o "$report_dir/diagnostic-bench" ./cmd/diagnostic-bench
}
performance() {
  local quick=()
  case "${PERF_QUICK:-0}" in 0) ;; 1) quick=(-quick);; *) echo "PERF_QUICK must be 0 or 1" >&2; return 2;; esac
  "$report_dir/diagnostic-bench" -server "$report_dir/diagnostic-server" -dir "$report_dir" \
    -seconds "${PERF_SECONDS:-10}" -repeats "${PERF_REPEATS:-3}" -workers "${PERF_WORKERS:-8}" "${quick[@]}"
}
performance_tops() {
  local scenario profile
  local sample=()
  for scenario in warm-hot distinct-keys capacity-pressure; do
    for profile in cpu heap allocs mutex block; do
      sample=()
      case "$profile" in allocs) sample=(-alloc_space);; heap) sample=(-inuse_space);; esac
      go tool pprof -top "${sample[@]}" -nodecount=15 "$report_dir/diagnostic-server" \
        "$report_dir/profile-$scenario-1/$profile.pprof" >"$report_dir/profile-$scenario-1/$profile-top.log" 2>&1 || return "$?"
    done
    # Heap retention and allocation churn are different questions.
    go tool pprof -top -inuse_space -nodecount=15 "$report_dir/diagnostic-server" \
      "$report_dir/profile-$scenario-1/heap.pprof" >"$report_dir/profile-$scenario-1/heap-inuse-top.log" 2>&1 || return "$?"
  done
}
if [[ "$mode" == unit ]]; then
  stage engine-unit local_unit
elif [[ "$mode" == race ]]; then
  stage engine-race local_race
elif [[ "$mode" == operator ]]; then
  stage operator-tests operator_tests
elif [[ "$mode" == perf ]]; then
  stage performance-build performance_build
  stage performance performance
  stage performance-tops performance_tops
elif [[ "$mode" == local ]]; then
  stage engine-race local_race
  stage diagnostics diagnostics
  stage cpu-top go tool pprof -top -nodecount=15 "$report_dir/scenarios.test" "$report_dir/cpu.pprof"
  stage heap-top go tool pprof -top -nodecount=15 "$report_dir/scenarios.test" "$report_dir/heap.pprof"
else
  # Raw cluster diagnostics are collected even when the smoke fails.
  export SIMPLECACHE_REPORT_DIR="$report_dir"
  stage probe-build bash -c 'cd "$1/geecache-engine" && go build -o "$2/probe" ./cmd/validation-probe' _ "$project_root" "$report_dir"
  stage kind-smoke bash "$project_root/scripts/kind/smoke.sh"
fi
