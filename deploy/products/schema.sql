CREATE TABLE IF NOT EXISTS simplecache_products (
    key text PRIMARY KEY CHECK (octet_length(key) BETWEEN 1 AND 256),
    value bytea NOT NULL CHECK (octet_length(value) <= 1048576),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    deleted boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Explicit, repeatable upgrade for existing example volumes. Existing values
-- begin at version 1; no values are overwritten and tombstones are retained.
ALTER TABLE simplecache_products
    ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 1 CHECK (version > 0);
ALTER TABLE simplecache_products
    ADD COLUMN IF NOT EXISTS deleted boolean NOT NULL DEFAULT false;
