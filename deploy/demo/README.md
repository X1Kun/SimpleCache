# Immutable demo deployment

From the repository root:

```bash
docker compose -f deploy/demo/compose.yaml config --quiet
docker compose -f deploy/demo/compose.yaml up -d --build
```

This is the existing three-node immutable demo. Its project name, container
names, ports and notification configuration are retained. The original
`geecache-engine/docker-compose.yml` includes this definition for compatibility.
Build paths are relative to this file and point to `geecache-engine`.

For PostgreSQL-backed conditional writes, use `deploy/products/compose.yaml`.

Replicas serve independent local reads. For key sharding use the Gateway deployment.
