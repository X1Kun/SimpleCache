package cache

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestInvalidatingOtherStripeDoesNotDiscardInFlightFill(t *testing.T) {
	for _, tc := range []struct {
		buckets   int
		collision bool
	}{{1, true}, {256, false}, {256, true}} {
		buckets := tc.buckets
		t.Run(fmt.Sprintf("buckets-%d-collision-%t", buckets, tc.collision), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			g := NewGroup("stripes", 1<<20, GetterFunc(func(ctx context.Context, key string) ([]byte, error) {
				if calls.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return []byte(key), nil
			}), Options{EpochBuckets: buckets})
			key, other := "loading-key", "updated-key"
			found := false
			for i := 0; i < 4096; i++ {
				other = fmt.Sprintf("updated-%d", i)
				if (g.epochBucket(key) == g.epochBucket(other)) == tc.collision {
					found = true
					break
				}
			}
			if !found {
				t.Fatal("could not construct a bounded stripe fixture")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := g.GetLocal(ctx, key); done <- err }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := g.Invalidate(other); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, err := g.GetLocal(ctx, key); err != nil {
				t.Fatal(err)
			}
			expected := int32(1)
			if tc.collision {
				expected = 2
			}
			if calls.Load() != expected {
				t.Fatalf("loads=%d want=%d", calls.Load(), expected)
			}
		})
	}
}

func TestInvalidationSeparatesFlightsAndBlocksStaleFill(t *testing.T) {
	for _, local := range []bool{false, true} {
		name := "routed"
		if local {
			name = "local"
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			var calls atomic.Int32
			g := NewGroup("products", 1024, GetterFunc(func(ctx context.Context, _ string) ([]byte, error) {
				if calls.Add(1) == 1 {
					close(started)
					select {
					case <-release:
						return []byte("old"), nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return []byte("new"), nil
			}), Options{SourceTimeout: 3 * time.Second, RouteTimeout: 4 * time.Second})
			get := g.Get
			if local {
				get = g.GetLocal
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			oldDone := make(chan error, 1)
			go func() { _, err := get(ctx, "key"); oldDone <- err }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := g.Invalidate("key"); err != nil {
				t.Fatal(err)
			}
			// The old source remains blocked. A post-invalidation caller must
			// start a different flight and be able to complete independently.
			fresh, stop := context.WithTimeout(ctx, time.Second)
			v, err := get(fresh, "key")
			stop()
			if err != nil || v.String() != "new" {
				t.Fatalf("new caller joined old flight: %q %v", v.String(), err)
			}
			// Release without closing; deferred close covers failures above.
			release <- struct{}{}
			if err := <-oldDone; err != nil {
				t.Fatal(err)
			}
			v, err = get(ctx, "key")
			if err != nil || v.String() != "new" || calls.Load() != 2 {
				t.Fatalf("stale fill or unnecessary reload: %q %v calls=%d", v.String(), err, calls.Load())
			}
		})
	}
}

type delayedPeer struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (p *delayedPeer) PickPeer(string) (PeerGetter, bool) { return p, true }
func (p *delayedPeer) Get(ctx context.Context, _, _ string) ([]byte, error) {
	if p.calls.Add(1) == 1 {
		close(p.started)
		select {
		case <-p.release:
			return []byte("old"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []byte("new"), nil
}

func TestInvalidationDetachesOldRemoteFlight(t *testing.T) {
	p := &delayedPeer{started: make(chan struct{}), release: make(chan struct{}, 1)}
	defer close(p.release)
	g := NewGroup("remote", 1024, GetterFunc(func(context.Context, string) ([]byte, error) {
		return []byte("unexpected fallback"), nil
	}), Options{Peers: p, RouteTimeout: 3 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := g.Get(ctx, "key"); done <- err }()
	select {
	case <-p.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := g.Invalidate("key"); err != nil {
		t.Fatal(err)
	}
	fresh, stop := context.WithTimeout(ctx, time.Second)
	v, err := g.Get(fresh, "key")
	stop()
	if err != nil || v.String() != "new" || g.Stats().Entries != 0 {
		t.Fatalf("remote flight isolation: %q %v %+v", v.String(), err, g.Stats())
	}
	p.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInvalidateIsIdempotentAndKeepsOtherCachedKeys(t *testing.T) {
	var calls atomic.Int32
	g := NewGroup("products", 1024, GetterFunc(func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("value"), nil
	}), Options{})
	for _, key := range []string{"a", "b"} {
		if _, err := g.Get(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Invalidate(""); err != ErrInvalidKey {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := g.Invalidate("a"); err != nil {
			t.Fatal(err)
		}
	}
	if s := g.Stats(); s.Entries != 1 || s.Bytes != 6 || s.Removals != 1 {
		t.Fatalf("bad accounting: %+v", s)
	}
	_, _ = g.Get(context.Background(), "b")
	if calls.Load() != 2 {
		t.Fatal("unrelated cached value was removed")
	}
}
