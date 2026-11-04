package app

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/telemetry"
)

func TestInvalidTextKeysNeverReachSource(t *testing.T) {
	for _, direct := range []bool{false, true} {
		calls := 0
		group := cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) { calls++; return []byte("ok"), nil }), cache.Options{})
		handler := APIHandler(group, func() bool { return true }, telemetry.New(), APIOptions{DirectReads: direct})
		for _, key := range []string{"%00", "a%00b", "%FF"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("GET", "/api?key="+key, nil))
			if response.Code != 400 {
				t.Fatalf("direct=%v key=%s status=%d", direct, key, response.Code)
			}
		}
		if calls != 0 {
			t.Fatal("invalid key reached source", calls)
		}
	}
}
