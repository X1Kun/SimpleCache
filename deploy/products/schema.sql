CREATE TABLE IF NOT EXISTS simplecache_products (
    key text PRIMARY KEY CHECK (octet_length(key) BETWEEN 1 AND 256),
    value bytea NOT NULL CHECK (octet_length(value) <= 1048576),
    updated_at timestamptz NOT NULL DEFAULT now()
);
