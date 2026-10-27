package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/jackc/pgx/v5"
)

// Each test owns a schema, including its tombstones; no caller rows are changed.
func conditionalDatabase(t *testing.T) (context.Context, *pgx.Conn, string) {
	t.Helper()
	dsn := os.Getenv("SIMPLECACHE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIMPLECACHE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	schema := fmt.Sprintf("conditional_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("owned schema cleanup: %v", err)
		}
	})
	if _, err := admin.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	return ctx, admin, parsed.String()
}

func applyProductSchema(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	sql, err := os.ReadFile("../../../../deploy/products/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresSchemaMigration(t *testing.T) {
	ctx, admin, dsn := conditionalDatabase(t)
	if _, err := admin.Exec(ctx, `CREATE TABLE simplecache_products (
		key text PRIMARY KEY CHECK (octet_length(key) BETWEEN 1 AND 256),
		value bytea NOT NULL CHECK (octet_length(value) <= 1048576),
		updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO simplecache_products (key,value) VALUES ('legacy',$1)", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(ctx, dsn, 1); err == nil {
		store.Close()
		t.Fatal("unmigrated schema was accepted")
	}
	applyProductSchema(t, ctx, admin)
	applyProductSchema(t, ctx, admin)
	store, err := Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	v, err := store.GetVersioned(ctx, "legacy")
	if err != nil || v.Version != 1 || string(v.Value) != `{"v":1}` {
		t.Fatalf("migration changed legacy row: %+v %v", v, err)
	}
	if version, err := store.PutIfVersion(ctx, "legacy", []byte(`{"v":2}`), 1); err != nil || version != 2 {
		t.Fatal(version, err)
	}
	applyProductSchema(t, ctx, admin)
	v, err = store.GetVersioned(ctx, "legacy")
	if err != nil || v.Version != 2 || string(v.Value) != `{"v":2}` {
		t.Fatal("reapplying migration reset a version", v, err)
	}
}

func TestPostgresConditionalWrites(t *testing.T) {
	ctx, admin, dsn := conditionalDatabase(t)
	applyProductSchema(t, ctx, admin)
	one, err := Open(ctx, dsn, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(one.Close)
	two, err := Open(ctx, dsn, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(two.Close)
	stores := []*Store{one, two}
	if err := one.Put(ctx, "edit", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"create", "edit"} {
		t.Run(key, func(t *testing.T) {
			expected := int64(0)
			if key == "edit" {
				expected = 1
			}
			const writers = 12
			type result struct {
				value   []byte
				version int64
				err     error
			}
			barrier, results := make(chan struct{}), make(chan result, writers)
			var wg sync.WaitGroup
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-barrier
					value := []byte(fmt.Sprintf(`{"writer":%d}`, i))
					version, err := stores[i%2].PutIfVersion(ctx, key, value, expected)
					results <- result{value, version, err}
				}(i)
			}
			close(barrier)
			wg.Wait()
			close(results)
			wins, conflicts := 0, 0
			var winning []byte
			for r := range results {
				if r.err == nil {
					wins++
					winning = r.value
					if r.version != expected+1 {
						t.Fatal("incorrect winning version", r.version)
					}
				} else if errors.Is(r.err, cache.ErrPreconditionFailed) {
					conflicts++
				} else {
					t.Fatal(r.err)
				}
			}
			got, err := two.GetVersioned(ctx, key)
			if wins != 1 || conflicts != writers-1 || err != nil || got.Version != expected+1 || string(got.Value) != string(winning) {
				t.Fatalf("wins=%d conflicts=%d value=%+v error=%v", wins, conflicts, got, err)
			}
		})
	}
	if _, err := one.PutIfVersion(ctx, "edit", []byte(`{"stale":true}`), 1); !errors.Is(err, cache.ErrPreconditionFailed) {
		t.Fatal("stale overwrite accepted", err)
	}
	if _, err := one.DeleteIfVersion(ctx, "edit", 1); !errors.Is(err, cache.ErrPreconditionFailed) {
		t.Fatal("stale deletion accepted", err)
	}
	if v, err := one.DeleteIfVersion(ctx, "edit", 2); err != nil || v != 3 {
		t.Fatal(v, err)
	}
	if _, err := two.GetVersioned(ctx, "edit"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatal("tombstone returned a value", err)
	}
	var deleted bool
	var tombVersion int64
	var size int
	if err := admin.QueryRow(ctx, "SELECT deleted,version,octet_length(value) FROM simplecache_products WHERE key='edit'").Scan(&deleted, &tombVersion, &size); err != nil || !deleted || tombVersion != 3 || size != 0 {
		t.Fatal("deletion lost generation", deleted, tombVersion, size, err)
	}
	if _, err := one.DeleteIfVersion(ctx, "edit", 3); !errors.Is(err, cache.ErrPreconditionFailed) {
		t.Fatal("deleted representation matched", err)
	}
	if v, err := two.PutIfVersion(ctx, "edit", []byte(`{"recreated":true}`), 0); err != nil || v != 4 {
		t.Fatal("recreation reset generation", v, err)
	}
	if _, err := one.PutIfVersion(ctx, "edit", []byte(`{"stale":true}`), 1); !errors.Is(err, cache.ErrPreconditionFailed) {
		t.Fatal("old ETag matched recreated key", err)
	}
	if _, err := one.PutIfVersion(ctx, "edit", []byte(`{}`), 0); !errors.Is(err, cache.ErrPreconditionFailed) {
		t.Fatal("create replaced existing value", err)
	}
}
