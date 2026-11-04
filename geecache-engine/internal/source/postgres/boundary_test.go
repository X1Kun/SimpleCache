package postgres

import (
	"context"
	"errors"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"testing"
)

func TestInvalidKeysAreRejectedBeforeDatabaseAccess(t *testing.T) {
	store := &Store{} // No connection pool: rejection must happen before any I/O.
	for _, key := range []string{"a\x00b", string([]byte{255})} {
		if _, err := store.GetVersioned(context.Background(), key); !errors.Is(err, cache.ErrInvalidKey) {
			t.Fatal(err)
		}
		if err := store.Put(context.Background(), key, []byte(`{}`)); !errors.Is(err, cache.ErrInvalidKey) {
			t.Fatal(err)
		}
		if _, err := store.DeleteIfVersion(context.Background(), key, 1); !errors.Is(err, cache.ErrInvalidKey) {
			t.Fatal(err)
		}
	}
}
func TestDatabaseErrorPreservesCauseWithoutExposingIt(t *testing.T) {
	cause := errors.New("private driver diagnostic")
	err := databaseError("PostgreSQL read failed", cause)
	if err.Error() != "PostgreSQL read failed" || !errors.Is(err, cause) {
		t.Fatal(err)
	}
}
