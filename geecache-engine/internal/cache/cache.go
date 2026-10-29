package cache

import (
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache/lru"
	"sync"
	"time"
)

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
