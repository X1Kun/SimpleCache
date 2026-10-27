// Package postgres provides the authoritative persistent source for the
// read-mostly product-detail example. Schema changes are applied explicitly.
package postgres

import (
	"context"
	"errors"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string, maxConnections int32) (*Store, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		// Do not expose credentials from malformed connection strings.
		return nil, errors.New("invalid DATABASE_URL")
	}
	if maxConnections < 1 {
		return nil, errors.New("PostgreSQL connection limit must be positive")
	}
	config.MaxConns = maxConnections
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("could not create PostgreSQL pool")
	}
	// Unlike NewWithConfig, this actually connects and checks the schema.
	rows, err := pool.Query(ctx, "SELECT key, value, version, deleted FROM simplecache_products LIMIT 0")
	if err == nil {
		// Execution errors can arrive after Query returns its row description.
		rows.Close()
		err = rows.Err()
	}
	if err != nil {
		pool.Close()
		return nil, errors.New("PostgreSQL startup check failed; check connectivity and apply schema.sql")
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := s.GetVersioned(ctx, key)
	return value.Value, err
}

func (s *Store) GetVersioned(ctx context.Context, key string) (cache.SourceValue, error) {
	if err := cache.ValidateKey(key); err != nil {
		return cache.SourceValue{}, err
	}
	var value cache.SourceValue
	err := s.pool.QueryRow(ctx, "SELECT value, version FROM simplecache_products WHERE key = $1 AND NOT deleted", key).Scan(&value.Value, &value.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return cache.SourceValue{}, cache.ErrNotFound
	}
	if err != nil {
		if ctx.Err() != nil {
			return cache.SourceValue{}, ctx.Err()
		}
		return cache.SourceValue{}, errors.New("PostgreSQL read failed")
	}
	return value, nil
}

// Put is for trusted source-side writes/fixtures, not the public mutation API.
// It still advances the source version, including after a logical deletion.
func (s *Store) Put(ctx context.Context, key string, value []byte) error {
	if err := validateValue(key, value); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO simplecache_products (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value,
		version = simplecache_products.version + 1, deleted = false, updated_at = now()`, key, value)
	return writeError(ctx, err)
}

// PutIfVersion uses zero only for create-if-absent. A tombstone is absent to
// readers, but its generation survives so recreation cannot match an old ETag.
func (s *Store) PutIfVersion(ctx context.Context, key string, value []byte, expected int64) (int64, error) {
	if err := validateValue(key, value); err != nil {
		return 0, err
	}
	if expected < 0 {
		return 0, errors.New("invalid expected version")
	}
	var version int64
	var err error
	if expected == 0 {
		err = s.pool.QueryRow(ctx, `INSERT INTO simplecache_products (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value,
			version = simplecache_products.version + 1, deleted = false, updated_at = now()
			WHERE simplecache_products.deleted RETURNING version`, key, value).Scan(&version)
	} else {
		err = s.pool.QueryRow(ctx, `UPDATE simplecache_products SET value = $2,
			version = version + 1, updated_at = now()
			WHERE key = $1 AND version = $3 AND NOT deleted RETURNING version`, key, value, expected).Scan(&version)
	}
	return mutationResult(ctx, version, err)
}

// Delete is a trusted, idempotent source-side deletion. Keep the generation
// instead of physically removing a row, otherwise an old client could match a
// recreated key whose version restarted at one.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := cache.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE simplecache_products SET value = ''::bytea,
		version = version + 1, deleted = true, updated_at = now()
		WHERE key = $1 AND NOT deleted`, key)
	return writeError(ctx, err)
}

func (s *Store) DeleteIfVersion(ctx context.Context, key string, expected int64) (int64, error) {
	if err := cache.ValidateKey(key); err != nil {
		return 0, err
	}
	if expected <= 0 {
		return 0, errors.New("invalid expected version")
	}
	var version int64
	err := s.pool.QueryRow(ctx, `UPDATE simplecache_products SET value = ''::bytea,
		version = version + 1, deleted = true, updated_at = now()
		WHERE key = $1 AND version = $2 AND NOT deleted RETURNING version`, key, expected).Scan(&version)
	return mutationResult(ctx, version, err)
}

func validateValue(key string, value []byte) error {
	if err := cache.ValidateKey(key); err != nil {
		return err
	}
	if len(value) > cache.MaxValueBytes {
		return cache.ErrValueTooLarge
	}
	return nil
}

func mutationResult(ctx context.Context, version int64, err error) (int64, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, cache.ErrPreconditionFailed
	}
	if err != nil {
		return 0, writeError(ctx, err)
	}
	return version, nil
}

func writeError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("PostgreSQL write failed")
}
