package lru

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestPruneExpiredResumesAndDoesNotDeleteReplacements(t *testing.T) {
	c := New(1024, nil)
	now := time.Now()
	for i := 0; i < 3; i++ {
		c.AddUntil(fmt.Sprint(i), String("old"), now.Add(-time.Second))
	}
	checked, removed := c.PruneExpired(now, 1, time.Second)
	if checked != 1 || removed != 1 || c.Len() != 2 {
		t.Fatal("unbounded or incomplete first batch", checked, removed, c.Len())
	}
	// The next cursor entry is explicitly evicted and recreated at a new expiry.
	c.Remove("1")
	c.AddUntil("1", String("new"), now.Add(time.Hour))
	c.Add("never", String("keep"), 0)
	for i := 0; i < 8; i++ {
		n, _ := c.PruneExpired(now, 1, time.Second)
		if n > 1 {
			t.Fatal("scan exceeded its entry budget")
		}
	}
	if c.Len() != 2 || c.Bytes() != int64(len("1newneverkeep")) {
		t.Fatal("cursor removed a new value or leaked bytes", c.Len(), c.Bytes())
	}
	if v, ok := c.Get("1"); !ok || v.(String) != "new" {
		t.Fatal("replacement was removed")
	}
	if _, ok := c.Get("2"); ok {
		t.Fatal("cold expiry was not reclaimed")
	}
}

func TestPruneExpiredDoesNotRefreshRecency(t *testing.T) {
	c := New(16, nil)
	c.Add("a", String("111"), time.Hour)
	c.Add("b", String("222"), time.Hour)
	c.Add("c", String("333"), time.Hour)
	for i := 0; i < 5; i++ {
		c.PruneExpired(time.Now(), 1, time.Second)
	}
	c.Add("d", String("44444"), time.Hour)
	if _, ok := c.Get("a"); ok {
		t.Fatal("maintenance refreshed the oldest entry")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("wrong entry evicted after maintenance")
	}
}

type String string

func (d String) Len() int {
	return len(d)
}

func TestCache(t *testing.T) {

	t.Run("Basic Operations", func(t *testing.T) {
		lru := New(int64(0), nil)
		lru.Add("key1", String("1234"), 0)
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
		// Capacity is 10 logical bytes.
		cap := int64(len(k1 + v1 + k2 + v2 + "xx"))
		lru := New(cap, nil)

		lru.Add(k1, String(v1), 0)
		lru.Add(k2, String(v2), 0)
		lru.Add(k3, String(v3), 0)

		if _, ok := lru.Get("k1"); ok || lru.Len() != 2 {
			t.Fatalf("Removeoldest key1 failed")
		}
	})

	t.Run("Update Existing Key", func(t *testing.T) {

		lru := New(int64(12), nil)      // Capacity is 12 logical bytes.
		lru.Add("key1", String("1"), 0) // 5
		lru.Add("key2", String("2"), 0) // 5. Total 10.

		lru.Add("key1", String("val"), 0) // 4+3=7. Total 12; key2 still fits.

		lru.Add("key3", String("3"), 0) // 5. Total 17; capacity is exceeded by 5.
		// Updating key1 makes it most recent; key2 is oldest.
		// Therefore key2 should be evicted.

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
		lru.Add("key1", String("123456"), 0) // 10 bytes; capacity is full.
		lru.Add("k2", String("k2"), 0)       // 4 bytes; evict key1 and retain k2 (4).
		lru.Add("k3", String("k3"), 0)       // 4 bytes; retain k2 and k3, totaling 8.
		lru.Add("k4", String("k4"), 0)       // 4 bytes; evict k2 and retain k3 and k4, totaling 8.

		// k3 remains cached and must not appear in the eviction list.
		expect := []string{"key1", "k2"}

		if !reflect.DeepEqual(expect, evictedKeys) {
			t.Fatalf("Call OnEvicted failed, expect keys %s, but got %s", expect, evictedKeys)
		}
	})
}

func TestTTLInsertAndUpdate(t *testing.T) {
	c := New(64, nil)
	c.Add("key", String("value"), time.Minute)
	e := c.cache["key"].Value.(*entry)
	if e.expireAt.IsZero() {
		t.Fatal("first insertion must set expiry")
	}
	// Force expiry without sleeping; Get must remove both entry and bytes.
	e.expireAt = time.Now().Add(-time.Second)
	if _, ok := c.Get("key"); ok || c.Len() != 0 || c.byteNow != 0 {
		t.Fatal("expired insertion was not fully removed")
	}
	c.Add("key", String("value"), time.Minute)
	e = c.cache["key"].Value.(*entry)
	e.expireAt = time.Now().Add(-time.Second)
	c.Add("key", String("new"), time.Hour)
	if v, ok := c.Get("key"); !ok || v.(String) != "new" || c.byteNow != 6 {
		t.Fatal("update must refresh expiry and account for the new value size")
	}
	c.Add("key", String("new"), 0)
	if !e.expireAt.IsZero() {
		t.Fatal("zero TTL must clear previous expiry")
	}
}

func TestCapacityPressure(t *testing.T) {
	c := New(12, nil)
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		c.Add(key, String("12345"), time.Hour)
		if c.byteNow > 12 {
			t.Fatalf("logical bytes exceed capacity: %d", c.byteNow)
		}
	}
	if _, ok := c.Get("a"); ok || c.Len() != 2 {
		t.Fatal("capacity pressure must evict old entries")
	}
	c.Add("e", String("1"), time.Hour)
	if c.byteNow != 8 {
		t.Fatalf("shrinking a value must reclaim bytes, got %d", c.byteNow)
	}
}
