package cache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const MaxValueBytes = 1 << 20

var (
	ErrNotFound           = errors.New("key not found")
	ErrInvalidKey         = errors.New("key must contain 1 to 256 bytes")
	ErrValueTooLarge      = errors.New("value exceeds 1MiB")
	ErrPreconditionFailed = errors.New("source version precondition failed")
)

type Getter interface {
	Get(context.Context, string) ([]byte, error)
}

// VersionedGetter reads value and version from the same source snapshot.
// It is optional for immutable/demo sources.
type VersionedGetter interface {
	GetVersioned(context.Context, string) (SourceValue, error)
}

type SourceValue struct {
	Value   []byte
	Version int64
}
type GetterFunc func(context.Context, string) ([]byte, error)

func (f GetterFunc) Get(ctx context.Context, key string) ([]byte, error) { return f(ctx, key) }

type PeerGetter interface {
	Get(context.Context, string, string) ([]byte, error)
}

// ExpiringPeerGetter is optional. Legacy responses remain readable but cannot
// be mirrored without an owner-provided remaining lifetime.
type ExpiringPeerGetter interface {
	GetWithExpiry(context.Context, string, string) (PeerValue, error)
}

type PeerValue struct {
	Value     []byte
	ExpiresAt time.Time
	Version   int64
}
type PeerPicker interface {
	PickPeer(string) (PeerGetter, bool)
}

// Observer is implemented by the process metrics package; the cache owns no registry.
type Observer interface {
	Observe(operation, result string, elapsed time.Duration)
	Inflight(delta float64)
}

type Options struct {
	TTL           time.Duration
	KnownKeys     []string
	RouteTimeout  time.Duration
	SourceTimeout time.Duration
	Limiter       *SourceLimiter
	Peers         PeerPicker
	Lifecycle     context.Context
	Observer      Observer
	// EpochBuckets defaults to 256. One reproduces the former group-wide
	// invalidation barrier for controlled diagnostic comparisons.
	EpochBuckets  int
	HotCacheBytes int64
	HotTTL        time.Duration
}

type Group struct {
	name         string
	getter       Getter
	mainCache    cache
	hotCache     cache
	bloom        *BloomFilter
	routeFlight  singleflight.Group
	originFlight singleflight.Group
	// Fixed stripes bound metadata memory without fencing every unrelated key.
	// Hash collisions conservatively fence other in-flight loads in that stripe.
	epochMu sync.Mutex
	epochs  [256]uint64
	opts    Options
}

func NewGroup(name string, capacity int64, getter Getter, opts Options) *Group {
	if name == "" || capacity <= 0 || getter == nil {
		panic("group requires name, capacity and getter")
	}
	if opts.TTL < 0 || opts.RouteTimeout < 0 || opts.SourceTimeout < 0 {
		panic("negative cache duration")
	}
	if opts.TTL == 0 {
		opts.TTL = time.Minute
	}
	if opts.RouteTimeout == 0 {
		opts.RouteTimeout = 1200 * time.Millisecond
	}
	if opts.SourceTimeout == 0 {
		opts.SourceTimeout = 800 * time.Millisecond
	}
	if opts.Lifecycle == nil {
		opts.Lifecycle = context.Background()
	}
	if opts.Limiter == nil {
		opts.Limiter = NewSourceLimiter(32)
	}
	if opts.EpochBuckets == 0 {
		opts.EpochBuckets = 256
	}
	if opts.EpochBuckets < 1 || opts.EpochBuckets > 256 || opts.EpochBuckets&(opts.EpochBuckets-1) != 0 {
		panic("epoch buckets must be a power of two between 1 and 256")
	}
	if opts.HotCacheBytes < 0 || opts.HotCacheBytes >= capacity || opts.HotTTL < 0 {
		panic("invalid hot-cache budget or TTL")
	}
	if opts.HotTTL == 0 {
		opts.HotTTL = time.Second
	}
	opts.HotTTL = min(opts.HotTTL, opts.TTL, time.Second)
	g := &Group{name: name, getter: getter, mainCache: cache{cacheBytes: capacity - opts.HotCacheBytes}, hotCache: cache{cacheBytes: opts.HotCacheBytes}, opts: opts}
	if opts.KnownKeys != nil {
		g.bloom = newBloomForKeys(opts.KnownKeys)
	}
	return g
}
func (g *Group) Name() string { return g.name }

// Stats returns a synchronized snapshot of logical cache storage.
func (g *Group) Stats() Stats {
	s := g.mainCache.stats()
	if g.opts.HotCacheBytes > 0 {
		h := g.hotCache.stats()
		s.Bytes += h.Bytes
		s.Capacity += h.Capacity
		s.Entries += h.Entries
		s.Removals += h.Removals
		s.HotBytes, s.HotCapacity, s.HotEntries = h.Bytes, h.Capacity, h.Entries
	}
	return s
}

func (g *Group) observe(operation, result string, elapsed time.Duration) {
	if g.opts.Observer != nil {
		g.opts.Observer.Observe(operation, result, elapsed)
	}
}

func Result(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "error"
	}
}

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
	epoch := g.currentEpoch(key)
	result := g.routeFlight.DoChan(flightKey(epoch, key), func() (any, error) {
		work, cancel := context.WithTimeout(g.opts.Lifecycle, g.opts.RouteTimeout)
		defer cancel()
		if v, hit, err := g.lookup(work, key); hit || err != nil {
			return v, err
		}
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
						g.populateHot(key, view, epoch)
						return view, nil
					}
				}
				if errors.Is(err, ErrNotFound) {
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
		return g.getLocal(work, key, false)
	})
	return await(ctx, result)
}

// GetLocal never selects a peer. Source work has its own bounded lifecycle.
func (g *Group) GetLocal(ctx context.Context, key string) (ByteView, error) {
	return g.getLocal(ctx, key, true)
}

func (g *Group) getLocal(ctx context.Context, key string, countLookup bool) (ByteView, error) {
	v, hit, err := g.lookup(ctx, key)
	if countLookup {
		result := "miss"
		if hit {
			result = "hit"
		}
		g.observe("lookup_peer", result, 0)
	}
	if hit || err != nil {
		return v, err
	}
	epoch := g.currentEpoch(key)
	result := g.originFlight.DoChan(flightKey(epoch, key), func() (any, error) {
		work, cancel := context.WithTimeout(g.opts.Lifecycle, g.opts.SourceTimeout)
		defer cancel()
		if v, hit, err := g.lookup(work, key); hit || err != nil {
			return v, err
		}
		waitStart := time.Now()
		err := g.opts.Limiter.acquire(work)
		slotResult := "acquired"
		if err != nil {
			slotResult = "timeout"
			if errors.Is(err, context.Canceled) {
				slotResult = "canceled"
			}
		}
		g.observe("slot", slotResult, time.Since(waitStart))
		if err != nil {
			return ByteView{}, err
		}
		defer g.opts.Limiter.release()
		if err := work.Err(); err != nil {
			return ByteView{}, err
		}
		if g.opts.Observer != nil {
			g.opts.Observer.Inflight(1)
			defer g.opts.Observer.Inflight(-1)
		}
		start := time.Now()
		var value []byte
		var version int64
		if source, ok := g.getter.(VersionedGetter); ok {
			loaded, loadErr := source.GetVersioned(work, key)
			value, version, err = loaded.Value, loaded.Version, loadErr
			if err == nil && version <= 0 {
				err = errors.New("invalid source version")
			}
		} else {
			value, err = g.getter.Get(work, key)
		}
		if err == nil {
			err = work.Err()
		}
		if err == nil && len(value) > MaxValueBytes {
			err = ErrValueTooLarge
		}
		g.observe("source", Result(err), time.Since(start))
		if err != nil {
			return ByteView{}, err
		}
		if err := work.Err(); err != nil {
			return ByteView{}, err
		}
		view := ByteView{b: cloneBytes(value), version: version}
		// Check and insert under the same lock as invalidation. A pre-invalidation
		// reader may finish with its old value, but cannot repopulate the cache.
		g.epochMu.Lock()
		valid := epoch == g.epochs[g.epochBucket(key)]
		if valid {
			view = g.mainCache.add(key, view, g.opts.TTL)
			if g.opts.HotCacheBytes > 0 {
				g.hotCache.remove(key)
			}
		}
		g.epochMu.Unlock()
		if !valid {
			g.observe("discard_source", "epoch_changed", 0)
		}
		return view, nil
	})
	return await(ctx, result)
}

func ValidateKey(key string) error {
	if len(key) == 0 || len(key) > 256 {
		return ErrInvalidKey
	}
	return nil
}

// Invalidate removes a local copy and separates subsequent callers from both
// old routing and old source flights. It never changes the source or forwards.
func (g *Group) Invalidate(key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	g.epochMu.Lock()
	g.epochs[g.epochBucket(key)]++
	g.mainCache.remove(key)
	if g.opts.HotCacheBytes > 0 {
		g.hotCache.remove(key)
	}
	g.epochMu.Unlock()
	g.observe("invalidate", "ok", 0)
	return nil
}

func (g *Group) populateHot(key string, value ByteView, epoch uint64) {
	if g.opts.HotCacheBytes == 0 || value.RemainingTTL() <= 0 || int64(len(key)+value.Len()) > g.opts.HotCacheBytes {
		return
	}
	g.epochMu.Lock()
	valid := epoch == g.epochs[g.epochBucket(key)]
	if valid {
		g.hotCache.add(key, value, g.opts.HotTTL)
	}
	g.epochMu.Unlock()
	if !valid {
		g.observe("discard_hot", "epoch_changed", 0)
	}
}

func (g *Group) currentEpoch(key string) uint64 {
	g.epochMu.Lock()
	defer g.epochMu.Unlock()
	return g.epochs[g.epochBucket(key)]
}

func (g *Group) epochBucket(key string) uint64 {
	// FNV-1a over a string avoids allocating a []byte on each cache miss.
	hash := uint64(14695981039346656037)
	for i := 0; i < len(key); i++ {
		hash = (hash ^ uint64(key[i])) * 1099511628211
	}
	return hash & uint64(g.opts.EpochBuckets-1)
}

func flightKey(epoch uint64, key string) string {
	return strconv.FormatUint(epoch, 10) + ":" + key
}
