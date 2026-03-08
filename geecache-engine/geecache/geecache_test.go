package geecache

import (
	"fmt"
	"log"
	"reflect"
	"sync"
	"testing"
)

type Dog struct {
	wang int
}

func (d *Dog) Get(key string) ([]byte, error) {
	fmt.Println(d.wang)
	return nil, nil
}

func TestGetter(t *testing.T) {
	var f Getter = GetterFunc(func(key string) ([]byte, error) {
		return []byte(key), nil
	})

	expect := []byte("key")
	if v, _ := f.Get("key"); !reflect.DeepEqual(v, expect) {
		t.Errorf("callback failed")
	}
	dog := &Dog{wang: 100}
	var d Getter = GetterFunc(dog.Get)
	d.Get("123")
}

// var db = map[string]string{
// 	"Tom":  "630",
// 	"Jack": "589",
// 	"Sam":  "567",
// }

func TestGet(t *testing.T) {
	loadCounts := make(map[string]int, len(db))
	gee := NewGroup("scores", 2<<10, GetterFunc(
		func(key string) ([]byte, error) {
			log.Println("[SlowDB] search key", key)
			if v, ok := db[key]; ok {
				if _, ok := loadCounts[key]; !ok {
					loadCounts[key] = 0
				}
				loadCounts[key] += 1
				return []byte(v), nil
			}
			return nil, fmt.Errorf("%s not exist", key)
		}))

	for k, v := range db {
		if view, err := gee.Get(k); err != nil || view.String() != v {
			t.Fatal("failed to get value of Tom")
		} // load from callback function
		if _, err := gee.Get(k); err != nil || loadCounts[k] > 1 {
			t.Fatalf("cache %s miss", k)
		} // cache hit
	}

	if view, err := gee.Get("unknown"); err == nil {
		t.Fatalf("the value of unknow should be empty, but %s got", view)
	}
}

// 模拟数据库
var db = map[string]string{
	"Tom":  "630",
	"Jack": "589",
	"Sam":  "567",
}

func TestEviction(t *testing.T) {
	loadCounts := make(map[string]int)

	// 设置 25，刚好够存 2 个 (9+9=18)，不够存 3 个 (27)
	gee := NewGroup("eviction_test", 25, GetterFunc(
		func(key string) ([]byte, error) {
			loadCounts[key]++
			return []byte("12345"), nil
		}))

	// 1. 塞入数据
	gee.Get("key1")
	gee.Get("key2")
	gee.Get("key3")
	// 此时状态：[key3, key2]，key1 被淘汰

	// --- 核心修改：交换验证顺序 ---

	// 2. 先验证 key2 还在！
	// 预期：key2 应该还在缓存里
	// 这一步 Get 会把 key2 提到队头，状态变为 [key2, key3]
	if _, err := gee.Get("key2"); err != nil || loadCounts["key2"] > 1 {
		t.Fatalf("Key2 should be in cache, but missed. count: %d", loadCounts["key2"])
	}

	// 3. 再验证 key1 确实丢了
	// 预期：key1 已经被淘汰了，必须回源
	// 这一步加载 key1，会把队尾的 key3 挤出去，状态变为 [key1, key2] (key2 安全！)
	if _, err := gee.Get("key1"); err != nil || loadCounts["key1"] != 2 {
		t.Fatalf("Key1 should be evicted, but it seems hit cache. count: %d", loadCounts["key1"])
	}

	fmt.Println("TestEviction Passed!")
}

// 2. 测试高并发场景（验证锁的可靠性）
func TestGet_Concurrent(t *testing.T) {
	gee := NewGroup("concurrent_test", 2<<10, GetterFunc(
		func(key string) ([]byte, error) {
			// 模拟一点耗时
			// time.Sleep(time.Millisecond * 10)
			return []byte(db[key]), nil
		}))

	var wg sync.WaitGroup
	// 模拟 100 个并发请求
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 所有人同时抢 "Tom"
			view, err := gee.Get("Tom")
			if err != nil {
				t.Error(err)
			}
			if view.String() != "630" {
				t.Errorf("expect 630, but got %s", view.String())
			}
		}()
	}
	wg.Wait()
	fmt.Println("TestConcurrent Passed!")
}
