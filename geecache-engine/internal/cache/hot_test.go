package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type expiringPeer struct {
	value  string
	expiry time.Time
	calls  atomic.Int32
}

func (p *expiringPeer) PickPeer(string) (PeerGetter, bool) { return p, true }
func (p *expiringPeer) Get(ctx context.Context, group, key string) ([]byte, error) {
	v, e := p.GetWithExpiry(ctx, group, key)
	return v.Value, e
}
func (p *expiringPeer) GetWithExpiry(context.Context, string, string) (PeerValue, error) {
	p.calls.Add(1)
	return PeerValue{Value: []byte(p.value), ExpiresAt: p.expiry}, nil
}

func TestHotCacheUsesSharedBudgetAndInvalidation(t *testing.T) {
	p := &expiringPeer{value: "remote", expiry: time.Now().Add(time.Minute)}
	g := NewGroup("hot", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { return []byte("local"), nil }), Options{Peers: p, HotCacheBytes: 128, HotTTL: time.Minute})
	if g.opts.HotTTL != time.Second {
		t.Fatal("replica TTL exceeded the one-second bound")
	}
	for i := 0; i < 3; i++ {
		v, err := g.Get(context.Background(), "key")
		if err != nil || v.String() != "remote" {
			t.Fatalf("%q %v", v.String(), err)
		}
	}
	if p.calls.Load() != 1 || g.Stats().HotEntries != 1 {
		t.Fatal("remote result was not reused")
	}
	stored, ok := g.hotCache.get("key")
	if !ok || stored.expiresAt.After(p.expiry) {
		t.Fatal("owner expiry was extended")
	}
	_, _ = g.GetLocal(context.Background(), "local-key")
	stats := g.Stats()
	if stats.Capacity != 1024 || stats.HotCapacity != 128 || stats.Bytes > stats.Capacity || stats.Entries != 2 {
		t.Fatalf("bad budget: %+v", stats)
	}
	if err := g.Invalidate("key"); err != nil {
		t.Fatal(err)
	}
	p.value = "updated"
	v, err := g.Get(context.Background(), "key")
	if err != nil || v.String() != "updated" || p.calls.Load() != 2 {
		t.Fatal("hot copy survived invalidation")
	}
}

func TestUncacheableAndOversizedHotValuesDoNotPollute(t *testing.T) {
	for _, expiry := range []time.Time{{}, time.Now().Add(-time.Second)} {
		p := &expiringPeer{value: "value", expiry: expiry}
		g := NewGroup("uncacheable", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { return nil, errors.New("unexpected fallback") }), Options{Peers: p, HotCacheBytes: 128})
		for i := 0; i < 2; i++ {
			if _, err := g.Get(context.Background(), "key"); err != nil {
				t.Fatal(err)
			}
		}
		if p.calls.Load() != 2 || g.Stats().HotEntries != 0 {
			t.Fatal("unknown or expired owner TTL was cached")
		}
	}
	p := &expiringPeer{value: "small", expiry: time.Now().Add(time.Minute)}
	g := NewGroup("oversized", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { return nil, nil }), Options{Peers: p, HotCacheBytes: 128})
	_, _ = g.Get(context.Background(), "small")
	p.value = string(make([]byte, 129))
	_, _ = g.Get(context.Background(), "large")
	if _, ok := g.hotCache.get("small"); !ok {
		t.Fatal("oversized remote value flushed useful hot entries")
	}
}

type delayedExpiringPeer struct{ *delayedPeer }

func (p delayedExpiringPeer) PickPeer(string) (PeerGetter, bool) { return p, true }
func (p delayedExpiringPeer) GetWithExpiry(ctx context.Context, group, key string) (PeerValue, error) {
	v, err := p.delayedPeer.Get(ctx, group, key)
	return PeerValue{Value: v, ExpiresAt: time.Now().Add(time.Minute)}, err
}

func TestLatePeerReplyCannotRefillInvalidatedHotEntry(t *testing.T) {
	p := delayedExpiringPeer{&delayedPeer{started: make(chan struct{}), release: make(chan struct{}, 1)}}
	defer close(p.release)
	g := NewGroup("hot-flight", 1024, GetterFunc(func(context.Context, string) ([]byte, error) { return nil, errors.New("unexpected fallback") }), Options{Peers: p, HotCacheBytes: 128, RouteTimeout: 3 * time.Second})
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
	v, err := g.Get(ctx, "key")
	if err != nil || v.String() != "new" {
		t.Fatal("new flight did not finish")
	}
	p.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	v, err = g.Get(ctx, "key")
	if err != nil || v.String() != "new" || p.calls.Load() != 2 {
		t.Fatal("late reply overwrote hot value")
	}
}

func TestOversizedLocalValueKeepsOtherCachedEntries(t *testing.T) {
	c := cache{cacheBytes: 20}
	c.add("small", ByteView{b: []byte("value")}, time.Minute)
	c.add("large", ByteView{b: make([]byte, 21)}, time.Minute)
	if _, ok := c.get("small"); !ok {
		t.Fatal("oversized value flushed unrelated entries")
	}
	if s := c.stats(); s.Entries != 1 || s.Bytes != 10 {
		t.Fatalf("bad storage: %+v", s)
	}
}
