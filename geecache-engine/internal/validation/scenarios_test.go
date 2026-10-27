// Package validation exercises bounded adversarial scenarios, not production load.
package validation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/app"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer/hashring"
)

func evidence(t *testing.T, data any) {
	t.Helper()
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
	}), cache.Options{MaxMissWaiters: 256, Limiter: cache.NewSourceLimiterWithQueue(8, 256), SourceTimeout: 2 * time.Second, RouteTimeout: 3 * time.Second})
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
	}), cache.Options{Limiter: cache.NewSourceLimiter(2), SourceTimeout: 50 * time.Millisecond, RouteTimeout: 300 * time.Millisecond})
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

func TestHashMappingExpansion(t *testing.T) {
	a, b := hashring.New(100, nil), hashring.New(100, nil)
	a.Add("test/cache-0", "test/cache-1", "test/cache-2")
	b.Add("test/cache-0", "test/cache-1", "test/cache-2", "test/cache-3")
	before, after := map[string]int{}, map[string]int{}
	moved := 0
	for i := 0; i < 100000; i++ {
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("seed-42-key-%d", i))))
		x, y := a.Get(key), b.Get(key)
		before[x]++
		after[y]++
		if x != y {
			moved++
			if y != "test/cache-3" {
				t.Fatal("existing owners changed among themselves")
			}
		}
	}
	evidence(t, map[string]any{"keys": 100000, "seed": 42, "before": before, "after": after, "moved_fraction": float64(moved) / 100000, "reference_fraction": 0.25})
	if moved == 0 || moved == 100000 {
		t.Fatal("expansion did not move a subset")
	}
}

func TestStalePeerTimeoutFallbackAndRecovery(t *testing.T) {
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer stale.Close()
	// Use the deployed timeout budget; a 40ms healthy-path assertion is noisy
	// when race instrumentation and another local cluster share this host.
	router := peer.NewRouter("entry", 300*time.Millisecond)
	defer router.Close()
	if e := router.Update([]peer.Member{{ID: "owner", Address: strings.TrimPrefix(stale.URL, "http://")}}); e != nil {
		t.Fatal(e)
	}
	var localCalls atomic.Int64
	entry := cache.NewGroup("scores", 1<<20, cache.GetterFunc(func(_ context.Context, key string) ([]byte, error) { localCalls.Add(1); return []byte(key), nil }), cache.Options{Peers: router})
	start := time.Now()
	burst(t, 128, func(int) error {
		v, e := entry.Get(context.Background(), "fallback")
		if e == nil && v.String() != "fallback" {
			return errors.New("wrong value")
		}
		return e
	})
	if localCalls.Load() != 1 {
		t.Fatalf("fallback source calls=%d", localCalls.Load())
	}
	owner := cache.NewGroup("scores", 1<<20, cache.GetterFunc(func(_ context.Context, key string) ([]byte, error) { return []byte(key), nil }), cache.Options{})
	healthy := httptest.NewServer(peer.NewHandler(owner))
	defer healthy.Close()
	if e := router.Update([]peer.Member{{ID: "owner", Address: strings.TrimPrefix(healthy.URL, "http://")}}); e != nil {
		t.Fatal(e)
	}
	v, e := entry.Get(context.Background(), "fresh-after-recovery")
	evidence(t, map[string]any{"waiters": 128, "peer_timeout_ms": 300, "fallback_source_calls": localCalls.Load(), "elapsed_ms": time.Since(start).Milliseconds()})
	if e != nil || v.String() != "fresh-after-recovery" || localCalls.Load() != 1 {
		t.Fatalf("fallback/recovery: %v calls=%d", e, localCalls.Load())
	}
}

// Baselines use real HTTP handlers on loopback with a synthetic 2ms source.
// They are not deployed-Pod throughput numbers and do not include race overhead.
func TestHTTPDiagnosticBaselines(t *testing.T) {
	for _, scenario := range []string{"warm-hot", "distinct-keys", "capacity-pressure"} {
		t.Run(scenario, func(t *testing.T) {
			var sourceCalls atomic.Int64
			capacity := int64(2 << 20)
			if scenario == "capacity-pressure" {
				capacity = 16 << 10
			}
			valueFor := func(key string) string {
				if scenario == "capacity-pressure" {
					return key + strings.Repeat("v", 4096)
				}
				return key
			}
			g := cache.NewGroup("scores", capacity, cache.GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
				sourceCalls.Add(1)
				select {
				case <-time.After(2 * time.Millisecond):
					return []byte(valueFor(key)), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}), cache.Options{})
			s := httptest.NewServer(app.APIHandler(g, func() bool { return true }, nil))
			defer s.Close()
			client := &http.Client{Timeout: 2 * time.Second}
			defer client.CloseIdleConnections()
			if scenario == "warm-hot" {
				if _, e := g.Get(context.Background(), "hot"); e != nil {
					t.Fatal(e)
				}
			}
			var next atomic.Int64
			var mu sync.Mutex
			var latencies []float64
			var failures atomic.Int64
			start := time.Now()
			deadline := start.Add(time.Second)
			burst(t, 16, func(int) error {
				for time.Now().Before(deadline) {
					i := next.Add(1)
					key := "hot"
					if scenario == "distinct-keys" {
						key = fmt.Sprint(i)
					}
					if scenario == "capacity-pressure" {
						key = fmt.Sprint(i % 256)
					}
					began := time.Now()
					r, e := client.Get(s.URL + "/api?key=" + key)
					if e != nil {
						failures.Add(1)
					} else {
						body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
						r.Body.Close()
						if err != nil || r.StatusCode != 200 || string(body) != valueFor(key) {
							failures.Add(1)
						}
					}
					mu.Lock()
					latencies = append(latencies, float64(time.Since(began).Microseconds())/1000)
					mu.Unlock()
				}
				return nil
			})
			sort.Float64s(latencies)
			if len(latencies) == 0 {
				t.Fatal("no samples")
			}
			percentile := func(p float64) float64 { return latencies[int(float64(len(latencies)-1)*p)] }
			evidence(t, map[string]any{"environment": "loopback HTTP, synthetic source", "scenario": scenario, "workers": 16, "source_delay_ms": 2, "requests": len(latencies), "errors": failures.Load(), "elapsed_seconds": time.Since(start).Seconds(), "p50_ms": percentile(.50), "p95_ms": percentile(.95), "p99_ms": percentile(.99), "source_calls": sourceCalls.Load(), "cache": g.Stats()})
			if failures.Load() != 0 {
				t.Fatal("HTTP errors or incorrect values")
			}
		})
	}
}
