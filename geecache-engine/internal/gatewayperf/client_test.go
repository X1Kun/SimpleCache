package gatewayperf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func config(endpoint string) Config {
	return Config{Endpoint: endpoint, Host: "benchmark.local", Prefix: "test", Scenario: "capacity", Keys: 4, ValueBytes: 128, Reads: 8, Workers: 2, Seed: 42}
}
func TestPayloadAndTraceReproducibility(t *testing.T) {
	for _, n := range []int{128, 4096} {
		for _, v := range []int64{1, 10} {
			b := Value("key", v, n)
			var p Payload
			if len(b) != n || json.Unmarshal(b, &p) != nil || p.Key != "key" || p.Version != v {
				t.Fatal(len(b), p)
			}
		}
	}
	c := config("http://localhost:8000")
	c.Scenario = "zipf"
	if !reflect.DeepEqual(readTrace(c), readTrace(c)) {
		t.Fatal("nondeterministic trace")
	}
	c.Workers = 129
	if Validate(c) == nil {
		t.Fatal("unbounded workload accepted")
	}
}
func TestAllAttemptsIncludeErrorsAndKeepRawRecords(t *testing.T) {
	var count atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "benchmark.local" {
			t.Error("variant hostname missing", r.Host)
		}
		if count.Add(1)%2 == 0 {
			http.Error(w, "busy", 503)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("X-SimpleCache-Node", "default/test-0")
		_, _ = w.Write(Value(r.URL.Query().Get("key"), 1, 128))
	}))
	defer s.Close()
	dir := t.TempDir()
	result, e := Run(context.Background(), config(s.URL), dir)
	if e == nil || result.Attempted != 8 || result.Errors != 4 || result.Rejections != 4 || result.Success != 4 || result.Skipped != 0 || result.Latency.Count != 8 {
		t.Fatal(result, e)
	}
	b, e := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if e != nil || len(b) == 0 {
		t.Fatal(e)
	}
	values := []float64{9, 1, 5}
	p := Quantiles(values)
	if p.P50 != 5 || p.P95 != 9 || values[0] != 9 {
		t.Fatal(p, values)
	}
}
func TestIncorrectPayloadAndVersionAreNeverCountedAsSuccess(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("X-SimpleCache-Node", "default/test-0")
		_, _ = w.Write(Value("wrong-key", 1, 128))
	}))
	defer s.Close()
	result, e := Run(context.Background(), config(s.URL), t.TempDir())
	if e == nil || result.Incorrect != 8 || result.Success != 0 {
		t.Fatal(result, e)
	}
}

func TestReadBelowConfirmedVersionIsIncorrect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("X-SimpleCache-Node", "node")
		_, _ = w.Write(Value("test-0000", 1, 128))
	}))
	defer server.Close()
	r := runner{c: config(server.URL), client: server.Client(), expected: [][]byte{Value("test-0000", 1, 128)}}
	record := r.request(context.Background(), 0, 0, "GET", 0, 2, 2)
	if !record.StaleAtStart || !record.Incorrect {
		t.Fatal(record)
	}
}

func TestMixedStaleReadsFailWorkloadAndRemainInRawEvidence(t *testing.T) {
	t.Setenv("CACHE_ADMIN_TOKEN", "test-token")
	var version atomic.Int64
	version.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			next := version.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"source_committed": true, "source_version": next,
				"invalidation": map[string]any{"complete": true, "version": next, "results": []map[string]string{
					{"Node": "a", "Status": "ok"}, {"Node": "b", "Status": "ok"}, {"Node": "c", "Status": "ok"}}}})
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("X-SimpleCache-Node", "a")
		_, _ = w.Write(Value(r.URL.Query().Get("key"), 1, 128))
	}))
	defer server.Close()
	c := config(server.URL)
	c.Scenario = "mixed"
	c.Keys = 1
	c.Windows = 2
	dir := t.TempDir()
	result, err := Run(context.Background(), c, dir)
	if err == nil || result.Attempted != 40 || result.StaleAtStart < 19 || result.Incorrect != result.StaleAtStart || result.Success != result.Attempted-result.Incorrect {
		t.Fatal(result, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if err != nil || len(raw) == 0 {
		t.Fatal(err)
	}
}
