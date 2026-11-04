package cache

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sync/singleflight"
	"strconv"
)

func (g *Group) lookup(ctx context.Context, key string) (ByteView, bool, error) {
	if err := ctx.Err(); err != nil {
		return ByteView{}, false, err
	}
	if err := ValidateKey(key); err != nil {
		return ByteView{}, false, err
	}
	if v, ok := g.mainCache.get(key); ok {
		return v, true, nil
	}
	if g.bloom != nil && !g.bloom.Contains(key) {
		g.observe("bloom", "rejected", 0)
		return ByteView{}, false, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return ByteView{}, false, nil
}

// A caller retains its observed floor even if bounded metadata is retired while
// it waits. A weaker flight/cache hit cannot satisfy that caller's value read.
func (g *Group) lookupAtLeast(ctx context.Context, key string, minimum int64) (ByteView, bool, error) {
	value, hit, err := g.lookup(ctx, key)
	if hit && value.Version() < minimum {
		return ByteView{}, false, nil
	}
	return value, hit, err
}

func await(ctx context.Context, result <-chan singleflight.Result) (ByteView, error) {
	select {
	case <-ctx.Done():
		return ByteView{}, ctx.Err()
	case r := <-result:
		if err := ctx.Err(); err != nil {
			return ByteView{}, err
		}
		if r.Err != nil {
			return ByteView{}, r.Err
		}
		return r.Val.(ByteView), nil
	}
}

// Get loads locally; Gateway owns distributed key routing. Caller cancellation
// stops waiting without canceling shared bounded source work.
func (g *Group) Get(ctx context.Context, key string) (ByteView, error) {
	v, hit, err := g.lookup(ctx, key)
	lookupResult := "miss"
	if hit {
		lookupResult = "hit"
	}
	g.observe("lookup_api", lookupResult, 0)
	if hit || err != nil {
		return v, err
	}
	if !g.admit(g.apiWaiters, "api_waiter") {
		return ByteView{}, ErrOverloaded
	}
	defer func() { <-g.apiWaiters }()
	fence := g.readBarrier(key)
	result := g.originFlight.DoChan(flightKey(fence, key), func() (any, error) {
		work, cancel := context.WithTimeout(g.opts.Lifecycle, g.opts.SourceTimeout)
		defer cancel()
		if v, hit, err := g.lookupAtLeast(work, key, fence.minimum); hit || err != nil {
			return v, err
		}
		retryFence := fence
		for attempt := 0; ; attempt++ {
			loaded, err := g.loadSource(work, key, retryFence)
			retryFence = retryFence.withMinimum(loaded.minimum)
			if !errors.Is(err, ErrStaleVersion) || attempt == 1 || work.Err() != nil {
				return loaded.value, err
			}
			g.observe("source_retry", "stale_version", 0)
			retryFence = g.readBarrier(key).withMinimum(retryFence.minimum)
			if value, hit, err := g.lookupAtLeast(work, key, retryFence.minimum); hit || err != nil {
				return value, err
			}
		}
	})
	return await(ctx, result)
}

func flightKey(fence readFence, key string) string {
	return strconv.FormatUint(fence.epoch, 10) + ":" + strconv.FormatInt(fence.minimum, 10) + ":" + key
}
