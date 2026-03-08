package geecache

import (
	"geecache/lru"
	"sync"
	"time"
)

// 利用互斥锁和cacheBytes包装好lru
type cache struct {
	mu         sync.Mutex
	lru        *lru.Cache
	cacheBytes int64
}

// 互斥cache中添加元素
// 1、加锁 2、如果lru是空的，则创建 3、lru添加元素
func (c *cache) add(key string, value ByteView, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// lazy initialization
	if c.lru == nil {
		c.lru = lru.New(c.cacheBytes, nil)
	}
	c.lru.Add(key, value, ttl)
}

// 互斥cache中查找元素
// 1、加锁 2、如果cache中lru还没创建，则返回空结构 3、利用lru取得元素 4、没找到也返回空
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
