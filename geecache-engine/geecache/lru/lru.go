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
	onEvicted  func(key string, value Value) // 相当于回收站
}

// cache中双向链表的结构
type entry struct {
	key      string // 双向链表溯源回map
	value    Value
	expireAt time.Time // 引入过期时间，如果是0，永不过期
}

// 双向链表储存的元素都要实现Len()
type Value interface {
	Len() int
}

// 创建cache
// 指定总容量和回调函数，初始化
func New(bytesTotal int64, onEvicted func(key string, value Value)) *Cache {
	return &Cache{
		bytesTotal: bytesTotal,
		byteNow:    0,
		ll:         list.New(),
		cache:      make(map[string]*list.Element),
		onEvicted:  onEvicted,
	}
}

// 按键查找
// 1、在cache的map中查找key对应元素 2、返回元素 3、将元素移到队首
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

// 移除最不经常使用的元素
// 1、将Cache双向链表中队尾的元素移除 2、删除map中对应元素 3、并计算空间变化 4、执行回调函数（例如：关闭资源、统计监控、联动删除）
func (c *Cache) RemoveOldest() {
	e := c.ll.Back()
	if e != nil {
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

// cache中添加元素
// 1、如果cache的map中找到了对应key，则将元素移动到队头，并更改其值和空间占用
// 2、如果没找到，则新建一个元素放到队头，并写入map，计算空间变化
// 3、移除超过空间的最不经常访问的元素
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
		ele := c.ll.PushFront(&entry{key: key, value: value})
		c.cache[key] = ele
		c.byteNow += int64(len(key)) + int64(value.Len())
	}
	for c.bytesTotal != 0 && c.byteNow > c.bytesTotal {
		c.RemoveOldest()
	}
}

// 返回cache中元素个数
func (c *Cache) Len() int {
	return c.ll.Len()
}
