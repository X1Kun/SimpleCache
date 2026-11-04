// Package validation exercises bounded adversarial scenarios, not production load.
package validation

import (
	"context"

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

// Use the deployed timeout budget; a 40ms healthy-path assertion is noisy
// when race instrumentation and another local cluster share this host.

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
