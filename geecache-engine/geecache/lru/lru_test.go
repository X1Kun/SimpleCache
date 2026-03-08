package lru

import (
	"reflect"
	"testing"
)

type String string

func (d String) Len() int {
	return len(d)
}

func TestCache(t *testing.T) {

	t.Run("Basic Operations", func(t *testing.T) {
		lru := New(int64(0), nil)
		lru.Add("key1", String("1234"))
		if v, ok := lru.Get("key1"); !ok || string(v.(String)) != "1234" {
			t.Fatalf("cache hit key1=1234 failed")
		}
		if _, ok := lru.Get("key2"); ok {
			t.Fatalf("cache miss key2 failed")
		}
	})

	t.Run("Auto Eviction", func(t *testing.T) {
		k1, k2, k3 := "k1", "k2", "k3"
		v1, v2, v3 := "v1", "v2", "v3"
		// 容量 = 10字节
		cap := int64(len(k1 + v1 + k2 + v2 + "xx"))
		lru := New(cap, nil)

		lru.Add(k1, String(v1))
		lru.Add(k2, String(v2))
		lru.Add(k3, String(v3))

		if _, ok := lru.Get("k1"); ok || lru.Len() != 2 {
			t.Fatalf("Removeoldest key1 failed")
		}
	})

	t.Run("Update Existing Key", func(t *testing.T) {

		lru := New(int64(12), nil)   // 容量给大点：12
		lru.Add("key1", String("1")) // 5
		lru.Add("key2", String("2")) // 5. Total 10.

		lru.Add("key1", String("val")) // 4+3=7. Total 12. (还没超，key2不用死)

		lru.Add("key3", String("3")) // 5. Total 17. 超了 5.
		// key1 刚被访问过(Update)，它是最新的。key2 是最老的。
		// 所以应该踢掉 key2。

		if _, ok := lru.Get("key1"); !ok {
			t.Fatalf("key1 should be kept")
		}
		if _, ok := lru.Get("key2"); ok {
			t.Fatalf("key2 should be evicted")
		}
	})

	t.Run("OnEvicted Callback", func(t *testing.T) {
		evictedKeys := make([]string, 0)
		callback := func(key string, value Value) {
			evictedKeys = append(evictedKeys, key)
		}

		lru := New(int64(10), callback)
		lru.Add("key1", String("123456")) // 10 (满)
		lru.Add("k2", String("k2"))       // 4. 踢 key1. 剩 k2(4)
		lru.Add("k3", String("k3"))       // 4. 剩 k2(4), k3(4). 总 8.
		lru.Add("k4", String("k4"))       // 4. 总 12. 踢 k2. 剩 k3(4), k4(4).

		// 预期里把 k3 去掉，因为 k3 确实还在缓存里
		expect := []string{"key1", "k2"}

		if !reflect.DeepEqual(expect, evictedKeys) {
			t.Fatalf("Call OnEvicted failed, expect keys %s, but got %s", expect, evictedKeys)
		}
	})
}
