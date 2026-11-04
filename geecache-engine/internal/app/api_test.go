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

func TestCanonicalAPIKey(t *testing.T) {
	calls := 0
	g := cache.NewGroup(t.Name(), 1024, cache.GetterFunc(func(_ context.Context, key string) ([]byte, error) { calls++; return []byte(key), nil }), cache.Options{})
	handler := APIHandler(g, func() bool { return true }, nil, APIOptions{NodeID: "demo/cache-0"})
	for _, query := range []string{"key=plain", "key=product%3A42", "key=a%20b", "key=%E4%B8%AD%E6%96%87", "key=a%2Bb", "key=a~b"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/api?"+query, nil))
		if w.Code != 200 || w.Header().Get("X-SimpleCache-Node") != "demo/cache-0" {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
		}
	}
	before := calls
	for _, query := range []string{"", "key=", "key=product:42", "key=product%3a42", "key=%70lain", "key=a+b", "key=a&key=b", "key=a&other=b", "key=%FF", "key=%ZZ"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/api?"+query, nil))
		if w.Code != 400 {
			t.Fatalf("accepted noncanonical %q: %d", query, w.Code)
		}
	}
	if calls != before {
		t.Fatal("invalid key reached source")
	}
}
