"""Validation compatibility API; shared definitions live in deploy/system."""
from tools.system.manifests import (
    apply_fixture, benchmark_pod, cache_resources, deploy_benchmark,
    enable_backend, fixture_text, load_resources, unique,
)
from tools.system.manifests import database_resources as _database_resources


def database_resources(root, *, password, token, memory_data):
    return _database_resources(root, password=password, token=token,
                               storage='memory' if memory_data else 'ephemeral')
