package app

import (
	"context"
	"fmt"
	"os"
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
	w := productRequest(nodes[0].api, "PUT", "/api?key="+key, `{"price":120}`, "test-token")
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
	w = productRequest(nodes[1].api, "DELETE", "/api?key="+key, "", "test-token")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, node := range nodes {
		if w := productRequest(node.api, "GET", "/api?key="+key, "", ""); w.Code != 404 {
			t.Fatalf("deleted product: %d %s", w.Code, w.Body.String())
		}
	}
}
