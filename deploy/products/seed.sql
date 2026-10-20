-- Small functional dataset. Override with: psql -v rows=100000 -f seed.sql
\if :{?rows}
\else
\set rows 1000
\endif
INSERT INTO simplecache_products (key, value)
SELECT 'product:' || n,
       convert_to(json_build_object(
           'id', n, 'name', 'Product ' || n,
           'price_cents', 10000 + n,
           'description', repeat('sample product details ', 100)
       )::text, 'UTF8')
FROM generate_series(1, :rows) AS n
ON CONFLICT (key) DO NOTHING;
