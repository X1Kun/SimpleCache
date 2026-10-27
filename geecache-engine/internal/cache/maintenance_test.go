package cache

import (
	"context"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache/lru"
)

type cleanupEvents struct{ events chan struct{} }

func (*cleanupEvents) Observe(string, string, time.Duration) {}
func (*cleanupEvents) Inflight(float64)                      {}
func (o *cleanupEvents) Cleanup(int, int, time.Duration, bool) {
	select {
	case o.events <- struct{}{}:
	default:
	}
}

func TestCleanupReclaimsExpiredCopiesWithoutSourceOrEpochChanges(t *testing.T) {
	g := NewGroup("cleanup", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { t.Error("cleanup queried source"); return nil, nil }), Options{HotCacheBytes: 128, CleanupBatch: 1})
	for _, c := range []*cache{&g.mainCache, &g.hotCache} {
		c.lru = lru.New(c.cacheBytes, func(string, lru.Value) { c.evictions++ })
		c.lru.AddUntil("expired", ByteView{b: []byte("old"), version: 1}, time.Now().Add(-time.Second))
		c.lru.AddUntil("live", ByteView{b: []byte("new"), version: 2}, time.Now().Add(time.Hour))
	}
	epoch := g.currentEpoch("expired")
	g.CleanExpired()
	s := g.Stats()
	if s.Entries != 2 || s.Removals != 2 || g.currentEpoch("expired") != epoch {
		t.Fatal("cleanup accounting/fencing mismatch", s)
	}
	for _, c := range []*cache{&g.mainCache, &g.hotCache} {
		v, ok := c.get("live")
		if !ok || v.Version() != 2 || v.String() != "new" {
			t.Fatal("cleanup damaged live value")
		}
	}
}

func TestCleanupSkipsBusyCacheAndStopsWithLifecycle(t *testing.T) {
	o := &cleanupEvents{events: make(chan struct{}, 16)}
	g := NewGroup("worker", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { return nil, nil }), Options{Observer: o, CleanupInterval: time.Millisecond})
	g.mainCache.mu.Lock()
	done := make(chan struct{})
	go func() { g.CleanExpired(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		g.mainCache.mu.Unlock()
		t.Fatal("cleanup blocked behind foreground lock")
	}
	g.mainCache.mu.Unlock()
	// Drain the direct invocation before observing the worker.
	<-o.events
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := make(chan struct{})
	go func() { defer close(worker); g.RunMaintenance(ctx) }()
	select {
	case <-o.events:
	case <-time.After(time.Second):
		t.Fatal("worker did not run")
	}
	second := make(chan struct{})
	go func() { g.RunMaintenance(ctx); close(second) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("second maintenance worker did not exit")
	}
	cancel()
	select {
	case <-worker:
	case <-time.After(time.Second):
		t.Fatal("maintenance outlived lifecycle")
	}
}
