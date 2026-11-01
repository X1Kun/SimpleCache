package cache

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

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
	opts = normalizeOptions(opts, capacity)
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
