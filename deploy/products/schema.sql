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

-- Durable source-version notifications; upgrade alongside the product schema.
CREATE TABLE IF NOT EXISTS simplecache_outbox (
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
CREATE INDEX IF NOT EXISTS simplecache_outbox_delivered ON simplecache_outbox (delivered_at, id) WHERE delivered_at IS NOT NULL;
