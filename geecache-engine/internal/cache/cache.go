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
func (c *cache) add(key string, value ByteView, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// lazy initialization
	if c.lru == nil {
		c.lru = lru.New(c.cacheBytes, func(string, lru.Value) { c.evictions++ })
	}
	c.lru.Add(key, value, ttl)
}

// Stats describes local logical storage; expired entries are removed lazily.
type Stats struct {
	Bytes    int64  `json:"logical_bytes"`
	Capacity int64  `json:"capacity_bytes"`
	Entries  int    `json:"entries"`
	Removals uint64 `json:"removals"`
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
