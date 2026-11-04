// Package validation exercises bounded adversarial scenarios, not production load.
package validation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/app"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/bench/democlient"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/demo"
)

func evidence(t *testing.T, data map[string]any) {
	t.Helper()
	data["evidence_kind"] = "functional-contract"
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("EVIDENCE " + string(b))
}

func burst(t *testing.T, n int, call func(int) error) {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := call(i); err != nil {
				t.Errorf("request %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
}

func TestColdHotKey512Waiters(t *testing.T) {
	var calls atomic.Int64
	g := cache.NewGroup(t.Name(), 1<<20, cache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		select {
		case <-time.After(20 * time.Millisecond):
			return []byte("value"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), cache.Options{MaxMissWaiters: 512})
	burst(t, 512, func(int) error {
		v, e := g.Get(context.Background(), "hot")
		if e == nil && v.String() != "value" {
			return errors.New("wrong value")
		}
		return e
	})
	evidence(t, map[string]any{"requests": 512, "actual_source_calls": calls.Load()})
	if calls.Load() != 1 {
		t.Fatalf("source calls=%d", calls.Load())
	}
}

func TestDistinctKeysRespectSourceLimit(t *testing.T) {
	var active, peak, calls atomic.Int64
	g := cache.NewGroup(t.Name(), 1<<20, cache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-time.After(3 * time.Millisecond):
			return []byte(key), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), cache.Options{MaxMissWaiters: 256, Limiter: cache.NewSourceLimiterWithQueue(8, 256), SourceTimeout: 2 * time.Second})
	burst(t, 256, func(i int) error {
		key := fmt.Sprint(i)
		v, e := g.Get(context.Background(), key)
		if e == nil && v.String() != key {
			return errors.New("wrong value")
		}
		return e
	})
	evidence(t, map[string]any{"requests": 256, "limit": 8, "peak_source_concurrency": peak.Load(), "source_calls": calls.Load()})
	if peak.Load() > 8 || calls.Load() != 256 || active.Load() != 0 {
		t.Fatal("source bound or completion violated")
	}
}

func TestOverloadTimesOutAndRecovers(t *testing.T) {
	var slow atomic.Bool
	slow.Store(true)
	var active, peak atomic.Int64
	g := cache.NewGroup(t.Name(), 1<<20, cache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		if slow.Load() {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []byte(key), nil
	}), cache.Options{Limiter: cache.NewSourceLimiter(2), SourceTimeout: 50 * time.Millisecond})
	start := time.Now()
	burst(t, 128, func(i int) error {
		_, e := g.Get(context.Background(), fmt.Sprint(i))
		if !errors.Is(e, context.DeadlineExceeded) {
			return fmt.Errorf("expected deadline, got %v", e)
		}
		return nil
	})
	slow.Store(false)
	v, e := g.Get(context.Background(), "recovery")
	evidence(t, map[string]any{"requests": 128, "expected_timeouts": 128, "limit": 2, "peak_source_concurrency": peak.Load(), "elapsed_ms": time.Since(start).Milliseconds()})
	if e != nil || v.String() != "recovery" || peak.Load() > 2 {
		t.Fatalf("recovery: %v", e)
	}
}

func TestCapacityEvictionAndReload(t *testing.T) {
	var calls atomic.Int64
	value := strings.Repeat("v", 4096)
	g := cache.NewGroup(t.Name(), 16<<10, cache.GetterFunc(func(context.Context, string) ([]byte, error) { calls.Add(1); return []byte(value), nil }), cache.Options{})
	for pass := 0; pass < 3; pass++ {
		for i := 0; i < 256; i++ {
			v, e := g.Get(context.Background(), fmt.Sprintf("key-%03d", i))
			if e != nil || v.String() != value {
				t.Fatalf("value: %v", e)
			}
			s := g.Stats()
			if s.Bytes > s.Capacity {
				t.Fatalf("capacity exceeded: %+v", s)
			}
		}
	}
	s := g.Stats()
	evidence(t, map[string]any{"working_set_bytes": 256 * (4096 + 7), "passes": 3, "stats": s, "source_calls": calls.Load()})
	if s.Removals == 0 || calls.Load() != 768 {
		t.Fatal("capacity pressure did not exercise reload")
	}
}

// Fixed request counts verify HTTP/cache contracts independently of machine speed.
// Use diagnostic-bench for separate-process latency, throughput and profiles.
func TestHTTPReadContracts(t *testing.T) {
	for _, tc := range []struct {
		name             string
		keys, valueBytes int
		capacity         int64
		wantSource       int64
	}{
		{"warm-hot", 1, 0, 2 << 20, 1},
		{"distinct-keys", 32, 0, 2 << 20, 32},
		{"capacity-pressure", 32, 512, 1024, 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			g := cache.NewGroup("scores", tc.capacity, cache.GetterFunc(func(_ context.Context, key string) ([]byte, error) {
				calls.Add(1)
				return []byte(demo.AutoValue(key, tc.valueBytes)), nil
			}), cache.Options{})
			server := httptest.NewServer(app.APIHandler(g, func() bool { return true }, nil))
			defer server.Close()
			client := democlient.NewClient(1, 2*time.Second)
			defer client.CloseIdleConnections()
			if tc.name == "warm-hot" {
				if _, err := g.Get(context.Background(), "Auto-0"); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 64; i++ {
				key := fmt.Sprintf("Auto-%d", i%tc.keys)
				result := democlient.Read(context.Background(), client, server.URL, key, tc.valueBytes)
				if result.Failed || result.Incorrect {
					t.Fatal(result.Detail)
				}
				if stats := g.Stats(); stats.Bytes > stats.Capacity {
					t.Fatalf("capacity exceeded: %+v", stats)
				}
			}
			stats := g.Stats()
			if calls.Load() != tc.wantSource {
				t.Fatalf("source calls=%d, want %d", calls.Load(), tc.wantSource)
			}
			if tc.name == "capacity-pressure" && stats.Removals == 0 {
				t.Fatal("expected eviction and reload")
			}
			evidence(t, map[string]any{"environment": "in-process loopback HTTP, synthetic source", "scenario": tc.name, "requests": 64, "source_calls": calls.Load(), "cache": stats})
		})
	}
}
