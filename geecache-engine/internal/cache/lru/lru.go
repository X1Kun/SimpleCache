package lru

import (
	"container/list"
	"time"
)

type Cache struct {
	bytesTotal int64
	byteNow    int64
	ll         *list.List
	cache      map[string]*list.Element
	onEvicted  func(key string, value Value) // Callback invoked when an entry is removed.
}

// entry stores a key, value, and expiration in the LRU list.
type entry struct {
	key      string // Key used to remove the entry from the index.
	value    Value
	expireAt time.Time // A zero timestamp disables expiration.
}

// Value reports its logical byte length.
type Value interface {
	Len() int
}

// New creates an LRU cache.
// Configure logical capacity and an optional removal callback.
func New(bytesTotal int64, onEvicted func(key string, value Value)) *Cache {
	return &Cache{
		bytesTotal: bytesTotal,
		byteNow:    0,
		ll:         list.New(),
		cache:      make(map[string]*list.Element),
		onEvicted:  onEvicted,
	}
}

// Get looks up a key.
// Expired entries are removed; successful reads refresh recency.
func (c *Cache) Get(key string) (Value, bool) {
	if e, ok := c.cache[key]; ok {
		kv := e.Value.(*entry)
		if !kv.expireAt.IsZero() && time.Now().After(kv.expireAt) {
			c.RemoveElement(e)
			return nil, false
		}
		c.ll.MoveToFront(e)

		return kv.value, true
	}
	return nil, false
}

// RemoveOldest removes the least recently used entry.
// Removal updates the list, index, byte count, and optional callback.
func (c *Cache) RemoveOldest() {
	e := c.ll.Back()
	if e != nil {
		c.RemoveElement(e)
	}
}

// Remove is idempotent and preserves logical byte accounting.
func (c *Cache) Remove(key string) {
	if e, ok := c.cache[key]; ok {
		c.RemoveElement(e)
	}
}

func (c *Cache) RemoveElement(ele *list.Element) {
	c.ll.Remove(ele)
	kv := ele.Value.(*entry)
	delete(c.cache, kv.key)
	c.byteNow -= int64(len(kv.key)) + int64(kv.value.Len())
	if c.onEvicted != nil {
		c.onEvicted(kv.key, kv.value)
	}
}

// Add inserts or updates an entry.
// Updates refresh recency, value size, and expiration.
// New entries are inserted at the front and added to the index.
// Least recently used entries are removed until capacity is satisfied.
func (c *Cache) Add(key string, value Value, ttl time.Duration) {

	var expireAt time.Time
	if ttl > 0 {
		expireAt = time.Now().Add(ttl)
	}

	if e, ok := c.cache[key]; ok {
		c.ll.MoveToFront(e)
		kv := e.Value.(*entry)
		c.byteNow -= int64(kv.value.Len()) - int64(value.Len())
		kv.value = value
		kv.expireAt = expireAt
	} else {
		ele := c.ll.PushFront(&entry{key: key, value: value, expireAt: expireAt})
		c.cache[key] = ele
		c.byteNow += int64(len(key)) + int64(value.Len())
	}
	for c.bytesTotal != 0 && c.byteNow > c.bytesTotal {
		c.RemoveOldest()
	}
}

// Len returns the number of stored entries.
func (c *Cache) Len() int {
	return c.ll.Len()
}

// Bytes reports logical key/value bytes, excluding Go object overhead.
func (c *Cache) Bytes() int64 { return c.byteNow }
