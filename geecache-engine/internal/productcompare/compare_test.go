package productcompare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func defaults() Options {
	return Options{Server: "fixture", Dir: "report", DatabaseURL: "postgres://test:secret@127.0.0.1/test", Repeats: 3, Workers: 12, Keys: 512, ValueBytes: 4096, CacheBytes: 1 << 20, Passes: 2, Seed: 42, ZipfReads: 8192, MixedWindows: 30}
}

func TestWorkloadFitsAggregateCapacityAndPreservesTrace(t *testing.T) {
	o := defaults()
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	keys := keysFor(o.Seed, o.Keys)
	placement, total := owners(keys, o.ValueBytes)
	if total <= o.CacheBytes || total > 3*o.CacheBytes {
		t.Fatal("default dataset does not exercise aggregate capacity")
	}
	for _, id := range nodeIDs {
		if placement[id] > o.CacheBytes || placement[id] == 0 {
			t.Fatalf("bad placement: %v", placement)
		}
	}
	for _, version := range []int{1, 2} {
		value := productValue(keys[0], version, o.ValueBytes)
		if len(value) != o.ValueBytes || !json.Valid(value) {
			t.Fatal("wrong value shape or size")
		}
	}
	if string(productValue(keys[0], 1, o.ValueBytes)) == string(productValue(keys[0], 2, o.ValueBytes)) {
		t.Fatal("updates do not change expected content")
	}
	a := capacityTrace(keys, 2)
	b := capacityTrace(keysFor(o.Seed, o.Keys), 2)
	if len(a) != o.Keys*3*2 || traceHash(a) != traceHash(b) {
		t.Fatal("trace is not repeatable")
	}
	b[0].Node = 1
	if traceHash(a) == traceHash(b) {
		t.Fatal("trace digest ignores request routing")
	}
	if failureKey(keys) == "" {
		t.Fatal("no key for stopped owner")
	}
	o.Repeats = 1
	if o.Validate() == nil {
		t.Fatal("one repeat accepted without quick label")
	}
	o.Quick = true
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	o.CacheBytes = 8 << 20
	if o.Validate() == nil {
		t.Fatal("dataset fits a single cache, comparison would miss capacity pressure")
	}
}

const metricFixture = `process_cpu_seconds_total 1.5
process_resident_memory_bytes 2048
go_memstats_heap_alloc_bytes 1024
go_memstats_alloc_bytes_total 4096
simplecache_cache_bytes 512
simplecache_cache_capacity_bytes 1048576
simplecache_cache_entries 2
simplecache_cache_removals_total 3
simplecache_source_loads_total{result="ok"} 7
simplecache_source_loads_total{result="error"} 1
simplecache_source_writes_total{result="ok"} 5
simplecache_local_lookups_total{kind="api",result="hit"} 4
simplecache_local_lookups_total{kind="peer",result="hit"} 6
`

func TestMetricsCountLabelsSeparatelyAndRejectCounterResets(t *testing.T) {
	m, err := parseMetrics(metricFixture)
	if err != nil {
		t.Fatal(err)
	}
	if m["source"] != 8 || m["writes"] != 5 || m["api_hit"] != 4 || m["peer_hit"] != 6 {
		t.Fatalf("bad metrics: %v", m)
	}
	for _, body := range []string{"", strings.ReplaceAll(metricFixture, "1.5", "NaN"), strings.ReplaceAll(metricFixture, "1.5", "-1")} {
		if _, err := parseMetrics(body); err == nil {
			t.Fatal("invalid metrics accepted")
		}
	}
	before := []Snapshot{{Node: "node-0", PID: 10, Values: m}}
	after := []Snapshot{{Node: "node-0", PID: 11, Values: m}}
	if _, err := metricDelta(before, after); err == nil {
		t.Fatal("subtracted reset counters across restart")
	}
	after[0].PID = 10
	copy := map[string]float64{}
	for key, value := range m {
		copy[key] = value
	}
	after[0].Values = copy
	copy["source"] = 0
	if _, err := metricDelta(before, after); err == nil {
		t.Fatal("negative source loads accepted")
	}
	copy["source"] = 9
	copy["cache_bytes"] = copy["capacity"] + 1
	if _, err := metricDelta(before, after); err == nil {
		t.Fatal("capacity violation accepted")
	}
}

func TestTrafficDetectsWrongValuesAndHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("key") {
		case "ok":
			_, _ = w.Write(productValue("ok", 1, 512))
		case "wrong":
			_, _ = w.Write(productValue("wrong", 2, 512))
		default:
			http.Error(w, "failed", 503)
		}
	}))
	defer server.Close()
	client := newClient(3)
	defer client.CloseIdleConnections()
	nodes := []*node{{api: server.URL}, {api: server.URL}, {api: server.URL}}
	trace := []Operation{{0, "ok", 1}, {1, "wrong", 1}, {2, "failed", 1}}
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	stats, err := traffic(context.Background(), client, nodes, trace, 512, 3, path)
	if err == nil || stats.Attempted != 3 || stats.Errors != 1 || stats.Incorrect != 1 || stats.Skipped != 0 {
		t.Fatalf("wrong results: %+v %v", stats, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Count(string(data), "\n") != 3 {
		t.Fatal("raw request evidence not saved")
	}
}

func TestFailedRunSavesReportWithoutCredentialsOrOverwrite(t *testing.T) {
	o := defaults()
	o.Dir = t.TempDir()
	o.Server = filepath.Join(o.Dir, "missing-fixture")
	summary, err := Run(context.Background(), o)
	if err == nil || summary.Status != "failed" || summary.Failure == "" {
		t.Fatalf("failed run reported success: %+v %v", summary, err)
	}
	data, err := os.ReadFile(filepath.Join(o.Dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "postgres://") {
		t.Fatal("credentials written to report")
	}
	_, err = Run(context.Background(), o)
	if err == nil {
		t.Fatal("existing run directory reused")
	}
	after, _ := os.ReadFile(filepath.Join(o.Dir, "report.json"))
	if string(after) != string(data) {
		t.Fatal("existing report overwritten")
	}
}

func TestMedianUsesTrialObservations(t *testing.T) {
	if median([]float64{1, 100, 2}) != 2 || median([]float64{4, 2}) != 3 {
		t.Fatal("incorrect trial median")
	}
	if l := latency([]float64{5, 1, 4, 2, 3}); l.P50 != 3 || l.P95 != 5 || l.P99 != 5 {
		t.Fatalf("bad percentiles: %+v", l)
	}
}

func TestSkewedAndMixedTracesAreRepeatable(t *testing.T) {
	keys := keysFor(42, 512)
	a, b := zipfTrace(keys, 8192, 42), zipfTrace(keys, 8192, 42)
	if traceHash(a) != traceHash(b) {
		t.Fatal("Zipf trace changed")
	}
	counts := map[string]int{}
	for _, op := range a {
		counts[op.Key]++
	}
	if counts[keys[0]] <= 8192/10 {
		t.Fatal("trace did not exercise a concentrated hot key")
	}
	plan := mixedTrace(keys, 30, 42)
	if valueHash(plan) != valueHash(mixedTrace(keys, 30, 42)) {
		t.Fatal("mixed trace changed")
	}
	for i, window := range plan {
		if len(window.Reads) != 19 || window.Write.Version != i+2 {
			t.Fatal("not a serialized 95/5 trace")
		}
	}
}

func TestMixedReaderSeparatesStalenessFromCorruption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		version := 1
		if key == "future" {
			version = 4
		}
		_, _ = w.Write(productValue(key, version, 512))
	}))
	defer server.Close()
	client := newClient(3)
	defer client.CloseIdleConnections()
	n := &node{api: server.URL}
	stale := readMixed(context.Background(), client, n, Operation{Key: "old"}, 512, 3, 2)
	if stale.Incorrect || stale.Error != "" || stale.ObservedVersion != 1 || stale.CommittedAtStart != 2 {
		t.Fatalf("old declared version misclassified: %+v", stale)
	}
	if r := readMixed(context.Background(), client, n, Operation{Key: "future"}, 512, 3, 2); !r.Incorrect {
		t.Fatal("future version accepted")
	}
}

func TestCanceledStartupStopsItsChild(t *testing.T) {
	dir := t.TempDir()
	started, stopped := filepath.Join(dir, "started"), filepath.Join(dir, "stopped")
	t.Setenv("COMPARE_TEST_STARTED", started)
	t.Setenv("COMPARE_TEST_STOPPED", stopped)
	script := filepath.Join(dir, "blocked-fixture")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap 'printf stopped > \"$COMPARE_TEST_STOPPED\"; exit 0' TERM\nprintf started > \"$COMPARE_TEST_STARTED\"\nwhile :; do sleep 0.05; done\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := defaults()
	o.Server = script
	done := make(chan error, 1)
	go func() {
		_, err := startCluster(ctx, o, "independent", dir, o.DatabaseURL, "test-token", "test-application")
		done <- err
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("child did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected startup result: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled child did not stop")
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Fatal("owned child did not receive termination")
	}
}
