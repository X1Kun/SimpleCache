package cache

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache/lru"
)

// ByteView wraps an immutable cached byte slice.
type ByteView struct {
	b         []byte
	expiresAt time.Time
	version   int64
}

// Version is the authoritative source version, or zero for an unversioned source.
func (bv ByteView) Version() int64 { return bv.version }

// RemainingTTL is zero for uncacheable/legacy values and expired results.
func (bv ByteView) RemainingTTL() time.Duration {
	if bv.expiresAt.IsZero() {
		return 0
	}
	return max(0, time.Until(bv.expiresAt))
}

// Len implements the LRU Value interface.
func (bv ByteView) Len() int {
	return len(bv.b)
}

// ByteSlice returns a copy.
func (bv ByteView) ByteSlice() []byte {
	return cloneBytes(bv.b)
}

// WriteTo implements io.WriterTo without making a defensive slice copy.
// As required by io.Writer, w must not modify or retain the supplied bytes.
func (bv ByteView) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(bv.b)
	if err == nil && n < len(bv.b) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

// String returns the cached bytes as a string.
func (bv ByteView) String() string {
	return string(bv.b)
}

// cloneBytes copies the input slice.
func cloneBytes(input []byte) []byte {
	output := make([]byte, len(input))
	copy(output, input)
	return output
}

// cache adds synchronization and a byte limit to the LRU.
type cache struct {
	mu         sync.Mutex
	lru        *lru.Cache
	cacheBytes int64
	evictions  uint64
}

func (c *cache) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lru != nil {
		c.lru.Remove(key)
	}
}

// add inserts an entry while holding the cache lock.
// Initialize the LRU lazily, then insert the value.
func (c *cache) add(key string, value ByteView, ttl time.Duration) ByteView {
	c.mu.Lock()
	defer c.mu.Unlock()
	if int64(len(key)+value.Len()) > c.cacheBytes {
		if c.lru != nil {
			c.lru.Remove(key)
		}
		value.expiresAt = time.Time{}
		return value
	}
	deadline := time.Now().Add(ttl)
	if !value.expiresAt.IsZero() && value.expiresAt.Before(deadline) {
		deadline = value.expiresAt
	}
	if !deadline.After(time.Now()) {
		if c.lru != nil {
			c.lru.Remove(key)
		}
		return value
	}
	value.expiresAt = deadline
	// lazy initialization
	if c.lru == nil {
		c.lru = lru.New(c.cacheBytes, func(string, lru.Value) { c.evictions++ })
	}
	c.lru.AddUntil(key, value, deadline)
	return value
}

// Stats describes local logical storage; expiry may be removed on lookup or by
// bounded maintenance. Entries can include expiry not reached by either yet.
type Stats struct {
	VersionRecords   int    `json:"version_records"`
	VersionCapacity  int    `json:"version_capacity"`
	VersionEvictions uint64 `json:"version_evictions"`
	Bytes            int64  `json:"logical_bytes"`
	Capacity         int64  `json:"capacity_bytes"`
	Entries          int    `json:"entries"`
	Removals         uint64 `json:"removals"`
	HotBytes         int64  `json:"hot_bytes"`
	HotCapacity      int64  `json:"hot_capacity_bytes"`
	HotEntries       int    `json:"hot_entries"`
}

func (c *cache) stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Stats{Capacity: c.cacheBytes, Removals: c.evictions}
	if c.lru != nil {
		s.Bytes, s.Entries = c.lru.Bytes(), c.lru.Len()
	}
	return s
}

// get retrieves an entry while holding the cache lock.
// An uninitialized LRU is empty; successful reads update recency.
func (c *cache) get(key string) (ByteView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lru == nil {
		return ByteView{}, false
	}
	if value, ok := c.lru.Get(key); ok {
		return value.(ByteView), true
	}
	return ByteView{}, false
}

// version observes a resident value without touching its recency or deadline.
func (c *cache) version(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lru != nil {
		if v, ok := c.lru.Peek(key); ok {
			return v.(ByteView).Version()
		}
	}
	return 0
}

const cleanupBudget = 2 * time.Millisecond

type cleanupObserver interface {
	Cleanup(checked, removed int, elapsed time.Duration, busy bool)
}

// CleanExpired performs at most CleanupBatch checks and a 2ms best-effort work
// budget per area. Skip a busy lock instead of queueing behind foreground work.
// It does not change source versions, local epochs or key ownership.
func (g *Group) CleanExpired() {
	areas := []*cache{&g.mainCache}
	if g.opts.HotCacheBytes > 0 {
		areas = append(areas, &g.hotCache)
	}
	for _, area := range areas {
		start := time.Now()
		checked, removed, busy := area.cleanExpired(g.opts.CleanupBatch)
		if observer, ok := g.opts.Observer.(cleanupObserver); ok {
			observer.Cleanup(checked, removed, time.Since(start), busy)
		}
	}
}

func (c *cache) cleanExpired(limit int) (checked, removed int, busy bool) {
	if !c.mu.TryLock() {
		return 0, 0, true
	}
	defer c.mu.Unlock()
	if c.lru != nil {
		checked, removed = c.lru.PruneExpired(time.Now(), limit, cleanupBudget)
	}
	return
}

// RunMaintenance is explicit so constructing Groups in tests/libraries does not
// create immortal goroutines. The application starts one worker with its shared
// lifecycle; caller owns cancellation and may wait for this method to return.
func (g *Group) RunMaintenance(ctx context.Context) {
	if !g.maintenanceMu.TryLock() {
		return
	}
	defer g.maintenanceMu.Unlock()
	ticker := time.NewTicker(g.opts.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			g.CleanExpired()
		}
	}
}
