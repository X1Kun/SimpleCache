package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/outbox"
	"github.com/jackc/pgx/v5"
)

// OutboxSchemaSQL is also installed by deploy/products/schema.sql. It creates
// the queue in the same search_path as products. One queue serves one cache domain.
const OutboxSchemaSQL = `CREATE TABLE IF NOT EXISTS simplecache_outbox (
    id bigserial PRIMARY KEY,
    key text NOT NULL CHECK (octet_length(key) BETWEEN 1 AND 256),
    version bigint NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    leased_until timestamptz,
    attempts bigint NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    delivered_at timestamptz
);
CREATE INDEX IF NOT EXISTS simplecache_outbox_pending ON simplecache_outbox (available_at, id) WHERE delivered_at IS NULL;
CREATE INDEX IF NOT EXISTS simplecache_outbox_age ON simplecache_outbox (created_at) WHERE delivered_at IS NULL;
CREATE INDEX IF NOT EXISTS simplecache_outbox_delivered ON simplecache_outbox (delivered_at, id) WHERE delivered_at IS NOT NULL;`

func (s *Store) Claim(ctx context.Context, lease time.Duration) (outbox.Event, bool, error) {
	if lease <= 0 || lease > time.Minute {
		return outbox.Event{}, false, errors.New("invalid outbox lease")
	}
	var event outbox.Event
	err := s.pool.QueryRow(ctx, `WITH candidate AS (
 SELECT id FROM simplecache_outbox WHERE delivered_at IS NULL AND available_at<=clock_timestamp()
 AND (leased_until IS NULL OR leased_until<=clock_timestamp()) ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1
 ) UPDATE simplecache_outbox e SET attempts=e.attempts+1, leased_until=clock_timestamp()+$1*interval '1 second'
 FROM candidate c WHERE e.id=c.id RETURNING e.id,e.key,e.version,e.attempts`, lease.Seconds()).Scan(&event.ID, &event.Key, &event.Version, &event.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return outbox.Event{}, false, nil
	}
	if err != nil {
		return outbox.Event{}, false, databaseError("outbox claim failed", err)
	}
	return event, true, nil
}
func (s *Store) Finish(ctx context.Context, event outbox.Event, delivered bool, retry time.Duration) (bool, error) {
	if event.ID <= 0 || event.Attempt <= 0 || retry < 0 || retry > time.Minute {
		return false, errors.New("invalid outbox completion")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE simplecache_outbox SET
 delivered_at=CASE WHEN $3 THEN clock_timestamp() ELSE NULL END,
 available_at=CASE WHEN $3 THEN available_at ELSE clock_timestamp()+$4*interval '1 second' END,
 leased_until=NULL WHERE id=$1 AND attempts=$2 AND delivered_at IS NULL AND leased_until>clock_timestamp()`, event.ID, event.Attempt, delivered, retry.Seconds())
	if err != nil {
		return false, databaseError("outbox completion failed", err)
	}
	return tag.RowsAffected() == 1, nil
}
func (s *Store) Stats(ctx context.Context) (outbox.Stats, error) {
	var stats outbox.Stats
	var age float64
	err := s.pool.QueryRow(ctx, `SELECT count(*),COALESCE(EXTRACT(EPOCH FROM (clock_timestamp()-min(created_at))),0) FROM simplecache_outbox WHERE delivered_at IS NULL`).Scan(&stats.Pending, &age)
	if err != nil {
		return stats, databaseError("outbox stats failed", err)
	}
	err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM simplecache_outbox WHERE delivered_at < statement_timestamp() - $1*interval '1 second'), pg_total_relation_size('simplecache_outbox'::regclass)`, outbox.Retention.Seconds()).Scan(&stats.PurgeEligible, &stats.TableBytes)
	if err != nil {
		return stats, databaseError("outbox storage stats failed", err)
	}
	stats.OldestAge = time.Duration(max(0, age) * float64(time.Second))
	return stats, nil
}
func (s *Store) PurgeDelivered(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	if retention < time.Hour || limit < 1 || limit > 1000 {
		return 0, errors.New("invalid outbox retention")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM simplecache_outbox WHERE id IN (
 SELECT id FROM simplecache_outbox WHERE delivered_at<clock_timestamp()-$1*interval '1 second'
 ORDER BY delivered_at,id FOR UPDATE SKIP LOCKED LIMIT $2)`, retention.Seconds(), limit)
	if err != nil {
		return 0, databaseError("outbox retention failed", err)
	}
	return tag.RowsAffected(), nil
}
