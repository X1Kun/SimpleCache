package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/peer"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/source/postgres"
)

// Run against a dedicated database initialized with deploy/products/schema.sql.
// Each run uses a unique key and removes only that key.
func TestPostgresProductLifecycle(t *testing.T) {
	dsn := os.Getenv("SIMPLECACHE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIMPLECACHE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("integration:%d", time.Now().UnixNano())
	defer store.Close()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := store.Delete(cleanup, key); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	if err := store.Put(ctx, key, []byte(`{"price":100}`)); err != nil {
		t.Fatal(err)
	}
	nodes := productCluster(t, store)
	for _, node := range nodes {
		if _, err := node.group.GetLocal(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	w := productRequest(nodes[0].api, "PUT", "/api?key="+key, `{"price":120}`, "test-token", `"v1"`)
	if w.Code != 200 {
		t.Fatalf("write/broadcast: %d %s", w.Code, w.Body.String())
	}
	for _, node := range nodes {
		w := productRequest(node.api, "GET", "/api?key="+key, "", "")
		if w.Code != 200 || w.Body.String() != `{"price":120}` {
			t.Fatalf("post-write read: %d %s", w.Code, w.Body.String())
		}
	}
	// A second independent pool verifies persistence outside cache memory.
	independent, err := postgres.Open(ctx, dsn, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Close()
	v, err := independent.Get(ctx, key)
	if err != nil || string(v) != `{"price":120}` {
		t.Fatalf("persistent value: %q %v", v, err)
	}
	// Writes outside the API require explicit invalidation or waiting for TTL.
	if err := independent.Put(ctx, key, []byte(`{"price":130}`)); err != nil {
		t.Fatal(err)
	}
	w = productRequest(nodes[2].api, "POST", "/invalidate?key="+key, "", "test-token")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, node := range nodes {
		w := productRequest(node.api, "GET", "/api?key="+key, "", "")
		if w.Code != 200 || w.Body.String() != `{"price":130}` {
			t.Fatalf("manual invalidation: %d %s", w.Code, w.Body.String())
		}
	}
	w = productRequest(nodes[1].api, "DELETE", "/api?key="+key, "", "test-token", `"v3"`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, node := range nodes {
		if w := productRequest(node.api, "GET", "/api?key="+key, "", ""); w.Code != 404 {
			t.Fatalf("deleted product: %d %s", w.Code, w.Body.String())
		}
	}
}

type countingProducts struct {
	MutableSource
	reads atomic.Int64
}

func (s *countingProducts) GetVersioned(ctx context.Context, key string) (cache.SourceValue, error) {
	s.reads.Add(1)
	return s.MutableSource.GetVersioned(ctx, key)
}

// This exercises the real SQL version sequence and authenticated HTTP fan-out
// using the same direct-read directory as Gateway-mode Pods, without a cluster.
func TestPostgresVersionedInvalidation(t *testing.T) {
	dsn := os.Getenv("SIMPLECACHE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIMPLECACHE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := fmt.Sprintf("version-invalidation-%d", time.Now().UnixNano())
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := store.Delete(cleanup, key); err != nil {
			t.Error(err)
		}
	}()
	if err := store.Put(ctx, key, []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	counted := &countingProducts{MutableSource: store}
	nodes := productClusterWithRouting(t, counted, 0, true)
	readAll := func(version int64) {
		t.Helper()
		for _, n := range nodes {
			w := productRequest(n.api, "GET", "/api?key="+key, "", "")
			if w.Code != 200 || w.Header().Get("ETag") != fmt.Sprintf(`"v%d"`, version) {
				t.Fatal(w.Code, w.Body.String(), w.Header())
			}
		}
	}
	write := func(method, payload, condition string, version int64) {
		t.Helper()
		w := productRequest(nodes[1].api, method, "/api?key="+key, payload, "test-token", condition)
		var reply mutationResponse
		if e := json.Unmarshal(w.Body.Bytes(), &reply); e != nil {
			t.Fatal(e)
		}
		if w.Code != 200 || reply.SourceVersion == nil || *reply.SourceVersion != version || reply.Invalidation.Version != version || !reply.Invalidation.Complete || len(reply.Invalidation.Results) != 3 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	notify := func(version int64) {
		t.Helper()
		r := httptest.NewRequest("POST", "/invalidate?key="+key, nil)
		r.Header.Set("Authorization", "Bearer test-token")
		r.Header.Set(peer.InvalidationVersionHeader, strconv.FormatInt(version, 10))
		w := httptest.NewRecorder()
		nodes[2].api.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	readAll(1)
	write("PUT", `{"v":2}`, `"v1"`, 2)
	readAll(2)
	write("PUT", `{"v":3}`, `"v2"`, 3)
	readAll(3)
	before := counted.reads.Load()
	notify(2)
	notify(3)
	notify(1)
	readAll(3)
	if counted.reads.Load() != before {
		t.Fatal("out-of-order notifications forced real PG reloads")
	}
	write("DELETE", "", `"v3"`, 4)
	for _, n := range nodes {
		if w := productRequest(n.api, "GET", "/api?key="+key, "", ""); w.Code != 404 {
			t.Fatal(w.Code)
		}
	}
	write("PUT", `{"v":5}`, "*", 5)
	readAll(5)
	before = counted.reads.Load()
	notify(4)
	readAll(5)
	if counted.reads.Load() != before {
		t.Fatal("late deletion evicted recreated SQL generation")
	}
	independent, e := postgres.Open(ctx, dsn, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer independent.Close()
	value, e := independent.GetVersioned(ctx, key)
	if e != nil || value.Version != 5 || string(value.Value) != `{"v":5}` {
		t.Fatal(value, e)
	}
}
