package postgres

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/X1Kun/simpleCache/geecache-engine/internal/outbox"
)

func TestOutboxSchemaMatchesDeployment(t *testing.T) {
	sql, err := os.ReadFile("../../../../deploy/products/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sql), OutboxSchemaSQL) {
		t.Fatal("deployed outbox schema differs from fixture schema")
	}
}

func TestPostgresOutboxAtomicityAndVersions(t *testing.T) {
	ctx, admin, dsn := conditionalDatabase(t)
	applyProductSchema(t, ctx, admin)
	store, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.PutIfVersion(ctx, "key", []byte(`{}`), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutIfVersion(ctx, "key", []byte(`{}`), 0); !errors.Is(err, cache.ErrPreconditionFailed) {
		t.Fatal(err)
	}
	if _, err := store.PutIfVersion(ctx, "key", []byte(`{"v":2}`), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteIfVersion(ctx, "key", 1); !errors.Is(err, cache.ErrPreconditionFailed) {
		t.Fatal(err)
	}
	if _, err := store.DeleteIfVersion(ctx, "key", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutIfVersion(ctx, "key", []byte(`{}`), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "key", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "key"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "key"); err != nil {
		t.Fatal(err)
	}
	var versions []int64
	if err := admin.QueryRow(ctx, "SELECT array_agg(version ORDER BY id) FROM simplecache_outbox").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 6 {
		t.Fatal("missing/extra events or conflict emitted an event", versions)
	}
	for i, v := range versions {
		if v != int64(i+1) {
			t.Fatal(versions)
		}
	}
	// Inject an event-insertion failure. The source update must roll back too.
	if _, err := admin.Exec(ctx, `CREATE FUNCTION fail_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected event failure'; END $$;
 CREATE TRIGGER fail_event BEFORE INSERT ON simplecache_outbox FOR EACH ROW EXECUTE FUNCTION fail_event()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutIfVersion(ctx, "key", []byte(`{"bad":true}`), 0); err == nil {
		t.Fatal("event failure accepted")
	}
	var version int64
	var deleted bool
	if err := admin.QueryRow(ctx, "SELECT version,deleted FROM simplecache_products WHERE key='key'").Scan(&version, &deleted); err != nil || version != 6 || !deleted {
		t.Fatal("source mutation escaped rollback", version, deleted, err)
	}
	var count int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM simplecache_outbox").Scan(&count); err != nil || count != 6 {
		t.Fatal(count, err)
	}
}

func TestPostgresOutboxLeasesReplayAndRetention(t *testing.T) {
	ctx, admin, dsn := conditionalDatabase(t)
	applyProductSchema(t, ctx, admin)
	first, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := first.PutIfVersion(ctx, "key", []byte(`{}`), 0); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	events := []outbox.Event{}
	var wg sync.WaitGroup
	for _, store := range []*Store{first, second} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			event, found, err := s.Claim(ctx, outbox.LeaseDuration)
			if err != nil {
				t.Error(err)
			}
			if found {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}
		}(store)
	}
	wg.Wait()
	if len(events) != 1 {
		t.Fatal("concurrent claims were not exclusive", events)
	}
	event := events[0]
	if _, err := admin.Exec(ctx, "UPDATE simplecache_outbox SET leased_until=clock_timestamp()-interval '1 second' WHERE id=$1", event.ID); err != nil {
		t.Fatal(err)
	}
	if owned, err := first.Finish(ctx, event, true, 0); err != nil || owned {
		t.Fatal("expired lease acknowledged", owned, err)
	}
	reclaimed, found, err := second.Claim(ctx, outbox.LeaseDuration)
	if err != nil || !found || reclaimed.ID != event.ID || reclaimed.Attempt != event.Attempt+1 {
		t.Fatal(reclaimed, found, err)
	}
	if owned, err := first.Finish(ctx, event, false, time.Second); err != nil || owned {
		t.Fatal("old worker overwrote new lease", owned, err)
	}
	if owned, err := second.Finish(ctx, reclaimed, false, time.Minute); err != nil || !owned {
		t.Fatal(owned, err)
	}
	if _, found, err := first.Claim(ctx, outbox.LeaseDuration); err != nil || found {
		t.Fatal("retry ignored available_at", found, err)
	}
	if _, err := admin.Exec(ctx, "UPDATE simplecache_outbox SET available_at=clock_timestamp()-interval '1 second' WHERE id=$1", event.ID); err != nil {
		t.Fatal(err)
	}
	next, found, err := first.Claim(ctx, outbox.LeaseDuration)
	if err != nil || !found {
		t.Fatal(next, found, err)
	}
	if owned, err := first.Finish(ctx, next, true, 0); err != nil || !owned {
		t.Fatal(owned, err)
	}
	if _, found, err := second.Claim(ctx, outbox.LeaseDuration); err != nil || found {
		t.Fatal("completed event was claimed again", found, err)
	}
	if _, err := admin.Exec(ctx, "UPDATE simplecache_outbox SET delivered_at=clock_timestamp()-interval '8 days'"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutIfVersion(ctx, "pending", []byte(`{}`), 0); err != nil {
		t.Fatal(err)
	}
	removed, err := first.PurgeDelivered(ctx, outbox.Retention, 1)
	if err != nil || removed != 1 {
		t.Fatal(removed, err)
	}
	stats, err := first.Stats(ctx)
	if err != nil || stats.Pending != 1 {
		t.Fatal("purge removed pending event", stats, err)
	}
}
