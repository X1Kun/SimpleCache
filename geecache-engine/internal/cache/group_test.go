package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalPolicy(t *testing.T) {
	ctx := context.Background()
	input := []byte("12345")
	calls := 0
	g := NewGroup(t.Name(), 12, GetterFunc(func(context.Context, string) ([]byte, error) { calls++; return input, nil }), Options{})
	for _, key := range []string{"a", "b", "c", "a"} {
		v, err := g.Get(ctx, key)
		if err != nil || v.String() != "12345" {
			t.Fatalf("reload: %v", err)
		}
	}
	if calls != 4 {
		t.Fatalf("evicted value must reload: %d", calls)
	}
	v, _ := g.Get(ctx, "a")
	input[0] = 'X'
	output := v.ByteSlice()
	output[0] = 'Y'
	v, _ = g.Get(ctx, "a")
	if v.String() != "12345" {
		t.Fatal("cached value mutated")
	}
	for _, key := range []string{"", strings.Repeat("k", 257)} {
		if _, err := g.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Fatal(err)
		}
	}
}

func TestBloomAndSizeLimits(t *testing.T) {
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = fmt.Sprintf("Auto-%d", i)
	}
	bf := newBloomForKeys(keys)
	for _, key := range keys {
		if !bf.Contains(key) {
			t.Fatal("false negative", key)
		}
	}
	calls := 0
	g := NewGroup(t.Name(), 2<<20, GetterFunc(func(context.Context, string) ([]byte, error) { calls++; return nil, ErrNotFound }), Options{KnownKeys: keys})
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("missing-%d", i)
		if !bf.Contains(key) {
			_, err := g.Get(context.Background(), key)
			if !errors.Is(err, ErrNotFound) || calls != 0 {
				t.Fatalf("Bloom: %d %v", calls, err)
			}
			break
		}
	}
	for _, n := range []int{MaxValueBytes, MaxValueBytes + 1} {
		g := NewGroup(fmt.Sprint(n), 2<<20, GetterFunc(func(context.Context, string) ([]byte, error) { return make([]byte, n), nil }), Options{})
		v, err := g.Get(context.Background(), "key")
		if n == MaxValueBytes && (err != nil || v.Len() != n) {
			t.Fatal(err)
		}
		if n > MaxValueBytes && !errors.Is(err, ErrValueTooLarge) {
			t.Fatal(err)
		}
	}
}

func TestSingleFlightAndCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	g := NewGroup(t.Name(), 1024, GetterFunc(func(ctx context.Context, _ string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return []byte("value"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), Options{SourceTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := g.Get(ctx, "key"); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := g.Get(context.Background(), "key")
			if err != nil || v.String() != "value" {
				t.Errorf("waiter: %v", err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("source calls=%d", calls.Load())
	}
}

func TestSourceSlotWaitUsesDeadline(t *testing.T) {
	limiter := NewSourceLimiter(1)
	if err := limiter.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer limiter.release()
	var calls atomic.Int32
	g := NewGroup(t.Name(), 1024, GetterFunc(func(context.Context, string) ([]byte, error) { calls.Add(1); return nil, nil }),
		Options{Limiter: limiter, SourceTimeout: 30 * time.Millisecond})
	if _, err := g.Get(context.Background(), "key"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("queued work called the getter")
	}
}

func TestSharedSourceConcurrencyLimit(t *testing.T) {
	limiter := NewSourceLimiter(2)
	var active, peak atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	getter := GetterFunc(func(ctx context.Context, _ string) ([]byte, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			return []byte("v"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	a := NewGroup("a", 1024, getter, Options{Limiter: limiter, SourceTimeout: time.Second})
	b := NewGroup("b", 1024, getter, Options{Limiter: limiter, SourceTimeout: time.Second})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g := a
			if i%2 == 1 {
				g = b
			}
			if _, err := g.Get(context.Background(), fmt.Sprint(i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	<-started
	<-started
	close(release)
	wg.Wait()
	if peak.Load() != 2 || len(limiter.slots) != 0 {
		t.Fatalf("peak=%d leaked slots=%d", peak.Load(), len(limiter.slots))
	}
}
