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
	rows, err := pool.Query(ctx, "SELECT key, value FROM simplecache_products LIMIT 0")
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
	if err := cache.ValidateKey(key); err != nil {
		return nil, err
	}
	var value []byte
	err := s.pool.QueryRow(ctx, "SELECT value FROM simplecache_products WHERE key = $1", key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, cache.ErrNotFound
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("PostgreSQL read failed")
	}
	return value, nil
}

func (s *Store) Put(ctx context.Context, key string, value []byte) error {
	if err := cache.ValidateKey(key); err != nil {
		return err
	}
	if len(value) > cache.MaxValueBytes {
		return cache.ErrValueTooLarge
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO simplecache_products (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, value)
	return writeError(ctx, err)
}

// Delete is idempotent, including deletion of an already absent product.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := cache.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM simplecache_products WHERE key = $1", key)
	return writeError(ctx, err)
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
