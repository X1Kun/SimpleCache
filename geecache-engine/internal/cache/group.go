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
	ErrInvalidVersion     = errors.New("invalid source invalidation version")
	ErrStaleVersion       = errors.New("value is older than an observed source version")
	ErrOverloaded         = errors.New("cache admission capacity exhausted")
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
	EpochBuckets    int
	HotCacheBytes   int64
	HotTTL          time.Duration
	MaxMissWaiters  int
	CleanupInterval time.Duration
	CleanupBatch    int
}

type Group struct {
	name         string
	getter       Getter
	mainCache    cache
	hotCache     cache
	bloom        *BloomFilter
	routeFlight  singleflight.Group
	originFlight singleflight.Group
	floors       versionFloors
	// Fixed stripes bound metadata memory without fencing every unrelated key.
	// Hash collisions conservatively fence other in-flight loads in that stripe.
	epochMu       sync.Mutex
	epochs        [256]uint64
	opts          Options
	apiWaiters    chan struct{}
	peerWaiters   chan struct{}
	routeTasks    chan struct{}
	maintenanceMu sync.Mutex
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
	if opts.MaxMissWaiters == 0 {
		opts.MaxMissWaiters = DefaultMissWaiters
	}
	if opts.CleanupInterval == 0 {
		opts.CleanupInterval = time.Second
	}
	if opts.CleanupBatch == 0 {
		opts.CleanupBatch = 128
	}
	if opts.MaxMissWaiters < 1 || opts.CleanupInterval < 0 || opts.CleanupBatch < 1 {
		panic("invalid admission or cleanup options")
	}
	g := &Group{name: name, getter: getter, mainCache: cache{cacheBytes: capacity - opts.HotCacheBytes}, hotCache: cache{cacheBytes: opts.HotCacheBytes}, opts: opts}
	g.floors.capacity = DefaultVersionRecords
	g.apiWaiters = make(chan struct{}, opts.MaxMissWaiters)
	g.peerWaiters = make(chan struct{}, opts.MaxMissWaiters)
	g.routeTasks = make(chan struct{}, opts.MaxMissWaiters)
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
	g.epochMu.Lock()
	s.VersionRecords = len(g.floors.entries)
	s.VersionCapacity = g.floors.capacity
	s.VersionEvictions = g.floors.evictions
	g.epochMu.Unlock()
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
	case errors.Is(err, ErrOverloaded):
		return "overloaded"
	case errors.Is(err, ErrStaleVersion):
		return "stale_version"
	case errors.Is(err, ErrPreconditionFailed):
		return "precondition_failed"
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

// A caller retains its observed floor even if bounded metadata is retired while
// it waits. A weaker flight/cache hit cannot satisfy that caller's value read.
func (g *Group) lookupAtLeast(ctx context.Context, key string, minimum int64) (ByteView, bool, error) {
	value, hit, err := g.lookup(ctx, key)
	if hit && value.Version() < minimum {
		return ByteView{}, false, nil
	}
	return value, hit, err
}
func (g *Group) readBarrier(key string) (uint64, int64) {
	g.epochMu.Lock()
	defer g.epochMu.Unlock()
	return g.epochs[g.epochBucket(key)], g.knownVersionLocked(key)
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
	epoch, minimum := g.readBarrier(key)
	result := g.routeFlight.DoChan(flightKey(epoch, minimum, key), func() (any, error) {
		work, cancel := context.WithTimeout(g.opts.Lifecycle, g.opts.RouteTimeout)
		defer cancel()
		if v, hit, err := g.lookupAtLeast(work, key, minimum); hit || err != nil {
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
						err = g.acceptPeer(key, view, epoch, minimum)
						if err == nil {
							return view, nil
						}
					}
				}
				if errors.Is(err, ErrNotFound) && max(minimum, g.knownVersion(key)) == 0 {
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
		return g.getLocal(work, key, "", minimum)
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
	epoch, minimum := g.readBarrier(key)
	minimum = max(minimum, required)
	result := g.originFlight.DoChan(flightKey(epoch, minimum, key), func() (any, error) {
		work, cancel := context.WithTimeout(g.opts.Lifecycle, g.opts.SourceTimeout)
		defer cancel()
		if v, hit, err := g.lookupAtLeast(work, key, minimum); hit || err != nil {
			return v, err
		}
		waitStart := time.Now()
		err := g.opts.Limiter.acquire(work)
		slotResult := "acquired"
		if err != nil {
			slotResult = "timeout"
			if errors.Is(err, ErrOverloaded) {
				slotResult = "overloaded"
				g.observe("rejected", "source_queue", 0)
			}
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
		stale := version < max(minimum, g.knownVersionLocked(key))
		valid := epoch == g.epochs[g.epochBucket(key)] && !stale
		if valid {
			view = g.mainCache.add(key, view, g.opts.TTL)
			if g.opts.HotCacheBytes > 0 {
				g.hotCache.remove(key)
			}
		}
		g.epochMu.Unlock()
		if stale {
			g.observe("version_rejected", "source", 0)
			g.observe("discard_source", "source_version", 0)
			return ByteView{}, ErrStaleVersion
		}
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
	return g.InvalidateVersion(key, 0)
}

// acceptPeer validates known source versions even when hot caching is disabled.
// Unknown/older Peer data falls back to the primary source through Get.
func (g *Group) acceptPeer(key string, value ByteView, epoch uint64, minimum int64) error {
	g.epochMu.Lock()
	if value.Version() < max(minimum, g.knownVersionLocked(key)) {
		g.epochMu.Unlock()
		g.observe("version_rejected", "peer", 0)
		return ErrStaleVersion
	}
	eligible := g.opts.HotCacheBytes > 0 && value.RemainingTTL() > 0 && int64(len(key)+value.Len()) <= g.opts.HotCacheBytes
	valid := epoch == g.epochs[g.epochBucket(key)]
	if eligible && valid {
		if value.Version() > g.mainCache.version(key) {
			g.mainCache.remove(key)
		}
		g.hotCache.add(key, value, g.opts.HotTTL)
	}
	g.epochMu.Unlock()
	if eligible && !valid {
		g.observe("discard_hot", "epoch_changed", 0)
	}
	return nil
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

func flightKey(epoch uint64, minimum int64, key string) string {
	return strconv.FormatUint(epoch, 10) + ":" + strconv.FormatInt(minimum, 10) + ":" + key
}
