package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
)

func TestAPIAndReadiness(t *testing.T) {
	ready := false
	g := cache.NewGroup("test", 1024, cache.GetterFunc(func(_ context.Context, key string) ([]byte, error) {
		if key == "missing" {
			return nil, cache.ErrNotFound
		}
		return []byte("value"), nil
	}), cache.Options{})
	handler := APIHandler(g, func() bool { return ready }, nil)
	for _, tc := range []struct {
		url     string
		status  int
		isReady bool
	}{
		{"/healthz", 200, false}, {"/readyz", 503, false}, {"/api?key=x", 503, false},
		{"/readyz", 200, true}, {"/api?key=x", 200, true}, {"/api?key=missing", 404, true}, {"/api", 400, true},
	} {
		ready = tc.isReady
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.url, nil))
		if w.Code != tc.status {
			t.Fatalf("%s: %d", tc.url, w.Code)
		}
	}
}

func TestMetricsCountActualLoadsAndLocalHits(t *testing.T) {
	metrics := telemetry.New()
	calls := 0
	g := cache.NewGroup("metrics", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) { calls++; return []byte("value"), nil }), cache.Options{KnownKeys: []string{"x"}, Observer: metrics})
	handler := APIHandler(g, func() bool { return true }, metrics)
	for _, key := range []string{"x", "x", "missing"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api?key="+key, nil))
	}
	if calls != 1 {
		t.Fatalf("actual getter calls=%d", calls)
	}
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			if metric.Counter != nil {
				totals[family.GetName()] += metric.Counter.GetValue()
			}
		}
	}
	if totals["simplecache_api_requests_total"] != 3 || totals["simplecache_source_loads_total"] != 1 || totals["simplecache_bloom_rejections_total"] != 1 {
		t.Fatalf("metrics do not reflect requests/source/Bloom: %v", totals)
	}
}
func TestConfigRejectsIncompleteKubernetesDiscovery(t *testing.T) {
	t.Setenv("DISCOVERY_MODE", "kubernetes")
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("PEER_SERVICE", "")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("incomplete Kubernetes identity accepted")
	}
	t.Setenv("DISCOVERY_MODE", "static")
	t.Setenv("SELF_ADDR", "")
	t.Setenv("PEERS", "")
	c, err := FromEnv(8001, true)
	if err != nil || len(c.Members) != 1 || c.Members[0].ID != c.SelfID {
		t.Fatal(c, err)
	}
	t.Setenv("SELF_ADDR", "http://user:secret@localhost:8001")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("credential URL accepted")
	}
}

func TestKubernetesIdentityIsIndependentOfTransport(t *testing.T) {
	t.Setenv("DISCOVERY_MODE", "kubernetes")
	t.Setenv("POD_NAME", "cache-1")
	t.Setenv("POD_NAMESPACE", "demo")
	t.Setenv("PEER_SERVICE", "cache-svc")
	t.Setenv("SELF_ADDR", "http://stale-address:8001")
	t.Setenv("PEERS", "http://stale-address:8001")
	c, err := FromEnv(8001, true)
	if err != nil || c.SelfID != "demo/cache-1" || len(c.Members) != 0 {
		t.Fatalf("unexpected Kubernetes configuration: %+v %v", c, err)
	}
	t.Setenv("DISCOVERY_MODE", "unknown")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("unknown discovery mode accepted")
	}
}

func TestRejectMethodBeforeCallingSource(t *testing.T) {
	calls := 0
	g := cache.NewGroup("method", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) { calls++; return nil, nil }), cache.Options{})
	handler := APIHandler(g, func() bool { return true }, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api?key=x", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" || calls != 0 {
		t.Fatalf("method validation: status=%d calls=%d", w.Code, calls)
	}
}

func TestHotCacheConfigurationSharesCapacity(t *testing.T) {
	t.Setenv("CACHE_BYTES", "1048576")
	for _, value := range []string{"-1", "1048576", "not-a-number"} {
		t.Setenv("HOT_CACHE_BYTES", value)
		if _, err := FromEnv(8001, true); err == nil {
			t.Fatal("invalid hot budget accepted")
		}
	}
	t.Setenv("HOT_CACHE_BYTES", "131072")
	c, err := FromEnv(8001, true)
	if err != nil || c.HotCacheBytes != 131072 {
		t.Fatal(c.HotCacheBytes, err)
	}
}

func TestAdmissionConfigurationBoundsAndZeroSourceQueue(t *testing.T) {
	t.Setenv("SOURCE_MODE", "demo")
	t.Setenv("DISCOVERY_MODE", "static")
	for _, name := range []string{"SOURCE_MAX_PENDING", "MAX_MISS_WAITERS", "WRITE_MAX_CONCURRENCY"} {
		t.Setenv(name, "")
	}
	c, err := FromEnv(8001, true)
	if err != nil || c.SourceMaxPending != cache.DefaultSourcePending || c.MaxMissWaiters != cache.DefaultMissWaiters || c.WriteConcurrency != DefaultWriteConcurrency {
		t.Fatal(c, err)
	}
	t.Setenv("SOURCE_MAX_PENDING", "0")
	c, err = FromEnv(8001, true)
	if err != nil || c.SourceMaxPending != 0 {
		t.Fatal("zero queue was defaulted away", c.SourceMaxPending, err)
	}
	for _, tc := range []struct{ name, value string }{
		{"SOURCE_MAX_PENDING", "-1"}, {"SOURCE_MAX_PENDING", "4097"},
		{"MAX_MISS_WAITERS", "0"}, {"MAX_MISS_WAITERS", "4097"},
		{"WRITE_MAX_CONCURRENCY", "0"}, {"WRITE_MAX_CONCURRENCY", "257"},
		{"WRITE_MAX_CONCURRENCY", "invalid"},
	} {
		t.Run(tc.name+"-"+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := FromEnv(8001, true); err == nil {
				t.Fatal("invalid admission accepted", tc)
			}
		})
	}
}
