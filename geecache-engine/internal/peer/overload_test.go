package peer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

func TestTypedPeerOverloadDoesNotAmplifySourceFallback(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{}, 1)
	defer close(release)
	owner := cache.NewGroup("products", 1024, cache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		started <- struct{}{}
		select {
		case <-release:
			return []byte(key), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), cache.Options{Limiter: cache.NewSourceLimiterWithQueue(1, 0), SourceTimeout: 3 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := owner.GetLocal(ctx, "blocked"); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	server := httptest.NewServer(NewHandler(owner))
	defer server.Close()
	router := NewRouter("entry", time.Second)
	defer router.Close()
	if err := router.Update([]Member{{ID: "owner", Address: strings.TrimPrefix(server.URL, "http://")}}); err != nil {
		t.Fatal(err)
	}
	var fallback atomic.Int32
	entry := cache.NewGroup("products", 1024, cache.GetterFunc(func(context.Context, string) ([]byte, error) { fallback.Add(1); return []byte("fallback"), nil }), cache.Options{Peers: router})
	if _, err := entry.Get(ctx, "excess"); !errors.Is(err, cache.ErrOverloaded) {
		t.Fatal("overload was not preserved", err)
	}
	if fallback.Load() != 0 {
		t.Fatal("owner overload triggered source fallback")
	}
	w := httptest.NewRecorder()
	NewHandler(owner).ServeHTTP(w, httptest.NewRequest(http.MethodGet, BasePath+"products/excess", nil))
	if w.Code != 503 || w.Header().Get(ErrorHeader) != "OVERLOADED" || w.Header().Get("Retry-After") != "1" {
		t.Fatal("untyped overload response", w.Code, w.Header())
	}
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Get(ctx, "blocked"); err != nil {
		t.Fatal("owner did not recover", err)
	}
}
