package cache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"
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
	if g.opts.HotCacheBytes > 0 {
		if v, ok := g.hotCache.get(key); ok {
			g.observe("hot_hit", "ok", 0)
			return v, true, nil
		}
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

// Get routes at most once. Caller cancellation stops waiting, not shared work.
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
	result := g.routeFlight.DoChan(flightKey(fence, key), func() (any, error) {
		work, cancel := context.WithTimeout(g.opts.Lifecycle, g.opts.RouteTimeout)
		defer cancel()
		if v, hit, err := g.lookupAtLeast(work, key, fence.minimum); hit || err != nil {
			return v, err
		}
		// Shared routing outlives canceled callers, so it needs its own bound.
		// Acquire inside the flight: same-key waiters never each take a slot.
		if !g.admit(g.routeTasks, "route_task") {
			return ByteView{}, ErrOverloaded
		}
		defer func() { <-g.routeTasks }()
		if g.opts.Peers != nil {
			if peer, ok := g.opts.Peers.PickPeer(key); ok {
				start := time.Now()
				var value PeerValue
				var err error
				if expiring, ok := peer.(ExpiringPeerGetter); ok {
					value, err = expiring.GetWithExpiry(work, g.name, key)
				} else {
					value.Value, err = peer.Get(work, g.name, key)
				}
				g.observe("peer", Result(err), time.Since(start))
				if err == nil {
					if len(value.Value) > MaxValueBytes {
						err = ErrValueTooLarge
					} else {
						view := ByteView{b: cloneBytes(value.Value), expiresAt: value.ExpiresAt, version: value.Version}
						err = g.acceptPeer(key, view, fence)
						if err == nil {
							return view, nil
						}
					}
				}
				if errors.Is(err, ErrNotFound) && max(fence.minimum, g.knownVersion(key)) == 0 {
					return ByteView{}, err
				}
				if errors.Is(err, ErrOverloaded) {
					return ByteView{}, err
				}
				if work.Err() != nil {
					return ByteView{}, work.Err()
				}
				reason := "error"
				if errors.Is(err, context.DeadlineExceeded) {
					reason = "timeout"
				}
				g.observe("fallback", reason, 0)
				// Transport, protocol and unknown-group failures fall back to the local source.
			}
		}
		return g.getLocal(work, key, "", fence.minimum)
	})
	return await(ctx, result)
}

// GetLocal never selects a peer. Source work has its own bounded lifecycle.
func (g *Group) GetLocal(ctx context.Context, key string) (ByteView, error) {
	return g.getLocal(ctx, key, "peer", 0)
}

// GetDirect is the public Gateway-mode read path: API admission and source
// coalescing, with neither routeFlight nor an outgoing Peer request.
func (g *Group) GetDirect(ctx context.Context, key string) (ByteView, error) {
	return g.getLocal(ctx, key, "api", 0)
}

func (g *Group) getLocal(ctx context.Context, key string, lookupKind string, required int64) (ByteView, error) {
	v, hit, err := g.lookupAtLeast(ctx, key, required)
	if lookupKind != "" {
		result := "miss"
		if hit {
			result = "hit"
		}
		g.observe("lookup_"+lookupKind, result, 0)
	}
	if hit || err != nil {
		return v, err
	}
	if lookupKind != "" {
		lane := g.peerWaiters
		if lookupKind == "api" {
			lane = g.apiWaiters
		}
		if !g.admit(lane, lookupKind+"_waiter") {
			return ByteView{}, ErrOverloaded
		}
		defer func() { <-lane }()
	}
	fence := g.readBarrier(key).withMinimum(required)
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
