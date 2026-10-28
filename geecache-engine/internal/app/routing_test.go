package app

import (
	"context"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"net/http/httptest"
	"testing"
)

func TestGatewayCanonicalKeyAndDirectReads(t *testing.T) {
	calls := 0
	g := cache.NewGroup(t.Name(), 1024, cache.GetterFunc(func(_ context.Context, key string) ([]byte, error) { calls++; return []byte(key), nil }), cache.Options{})
	handler := APIHandler(g, func() bool { return true }, nil, APIOptions{DirectReads: true, NodeID: "demo/cache-0"})
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

func TestReadRoutingConfiguration(t *testing.T) {
	t.Setenv("READ_ROUTING_MODE", "gateway")
	t.Setenv("HOT_CACHE_BYTES", "0")
	c, err := FromEnv(8001, true)
	if err != nil || c.ReadRoutingMode != "gateway" {
		t.Fatal(c, err)
	}
	t.Setenv("HOT_CACHE_BYTES", "1024")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("gateway accepted hot cache")
	}
	t.Setenv("READ_ROUTING_MODE", "other")
	if _, err := FromEnv(8001, true); err == nil {
		t.Fatal("invalid routing accepted")
	}
}
