package cache

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Read task defaults belong to the cache; application wait budgets are separate.
const (
	defaultSourceTimeout = 800 * time.Millisecond
)

type Options struct {
	TTL           time.Duration
	KnownKeys     []string
	SourceTimeout time.Duration
	Limiter       *SourceLimiter
	Lifecycle     context.Context
	Observer      Observer
	// EpochBuckets defaults to 256. One reproduces the former group-wide
	// invalidation barrier for controlled diagnostic comparisons.
	EpochBuckets    int
	MaxMissWaiters  int
	CleanupInterval time.Duration
	CleanupBatch    int
}

type Group struct {
	name         string
	getter       Getter
	mainCache    cache
	bloom        *BloomFilter
	originFlight singleflight.Group
	floors       versionFloors
	// Fixed stripes bound metadata memory without fencing every unrelated key.
	// Hash collisions conservatively fence other in-flight loads in that stripe.
	epochMu       sync.Mutex
	epochs        [256]uint64
	opts          Options
	apiWaiters    chan struct{}
	maintenanceMu sync.Mutex
}

func NewGroup(name string, capacity int64, getter Getter, opts Options) *Group {
	if name == "" || capacity <= 0 || getter == nil {
		panic("group requires name, capacity and getter")
	}
	opts = normalizeOptions(opts, capacity)
	g := &Group{name: name, getter: getter, mainCache: cache{cacheBytes: capacity}, opts: opts}
	g.floors.capacity = DefaultVersionRecords
	g.apiWaiters = make(chan struct{}, opts.MaxMissWaiters)
	if opts.KnownKeys != nil {
		g.bloom = newBloomForKeys(opts.KnownKeys)
	}
	return g
}

func normalizeOptions(opts Options, capacity int64) Options {
	if opts.TTL < 0 || opts.SourceTimeout < 0 {
		panic("negative cache duration")
	}
	if opts.TTL == 0 {
		opts.TTL = time.Minute
	}
	if opts.SourceTimeout == 0 {
		opts.SourceTimeout = defaultSourceTimeout
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
	return opts
}
func (g *Group) Name() string { return g.name }

// Stats returns a synchronized snapshot of logical cache storage.
func (g *Group) Stats() Stats {
	s := g.mainCache.stats()
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
