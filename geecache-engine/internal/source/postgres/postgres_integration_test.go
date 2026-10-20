package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresOpenChecksExecutionErrors(t *testing.T) {
	dsn := os.Getenv("SIMPLECACHE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SIMPLECACHE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("startup_check_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	params := parsed.Query()
	params.Set("search_path", schema)
	params.Set("default_query_exec_mode", "cache_statement")
	parsed.RawQuery = params.Encode()
	isolatedDSN := parsed.String()
	assertRejected := func() {
		t.Helper()
		store, err := Open(ctx, isolatedDSN, 1)
		if store != nil {
			store.Close()
		}
		if err == nil || store != nil {
			t.Fatalf("startup check accepted a failed query: store=%v err=%v", store, err)
		}
	}
	// Preparation failure remains a rejected startup too.
	assertRejected()
	table := quotedSchema + ".simplecache_products"
	if _, err := admin.Exec(ctx, "CREATE TABLE "+table+" (key text, value bytea)"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, isolatedDSN, 1)
	if err != nil {
		t.Fatalf("valid empty source failed startup: %v", err)
	}
	store.Close()
	if _, err := admin.Exec(ctx, "DROP TABLE "+table); err != nil {
		t.Fatal(err)
	}
	// The view can be prepared/described, but planning its expression during
	// execution fails even though the outer query asks for zero rows.
	if _, err := admin.Exec(ctx, "CREATE VIEW "+table+" AS SELECT (1 / 0)::text AS key, ''::bytea AS value"); err != nil {
		t.Fatal(err)
	}
	probe, err := pgx.Connect(ctx, isolatedDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close(context.Background())
	rows, err := probe.Query(ctx, "SELECT key, value FROM simplecache_products LIMIT 0")
	if err != nil {
		t.Fatalf("expected a deferred execution error, Query returned: %v", err)
	}
	rows.Close()
	var executionError *pgconn.PgError
	if !errors.As(rows.Err(), &executionError) || executionError.Code != "22012" {
		t.Fatalf("expected deferred division-by-zero error, got %v", rows.Err())
	}
	assertRejected()
}
