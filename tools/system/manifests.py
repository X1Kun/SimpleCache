"""Fresh declarative fixture loading and explicit experiment parameter injection.

No I/O occurs during import. Secrets are applied via stdin; saved renders redact
all Secret payloads. SQL remains sourced from deploy/products/schema.sql.
"""
import copy
import json
import pathlib

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
BASE = ROOT / 'deploy/system'
PROFILES = {
    'performance': {'gomaxprocs': 1, 'cpu_limit': '1', 'admin_token': True},
    'arrival': {'gomaxprocs': 2, 'cpu_limit': '2', 'admin_token': False},
}


def fixture_text(name):
    if name == 'kind.yaml':
        return kind_configuration(validation=True)
    if name == 'benchmark.yaml':
        return (ROOT / 'tools/validation/fixtures' / name).read_text()
    return (BASE / name).read_text()


def load_resources(name):
    documents = [d for d in yaml.safe_load_all(fixture_text(name)) if d is not None]
    if not documents or any(not isinstance(d, dict) or not d.get('apiVersion') or not d.get('kind') for d in documents):
        raise ValueError('invalid fixture resource: ' + name)
    return documents


def unique(values, key, value):
    matches = [item for item in values if item.get(key) == value]
    if len(matches) != 1:
        raise ValueError(f'fixture requires one {key}={value}')
    return matches[0]


def kind_configuration(*, validation=False):
    config = yaml.safe_load((BASE / 'kind.yaml').read_text())
    if validation:
        patch = yaml.safe_load((ROOT / 'tools/validation/fixtures/kind-overrides.yaml').read_text())
        config['nodes'][0]['kubeadmConfigPatches'].extend(patch['kubeadmConfigPatches'])
    return yaml.safe_dump(config, sort_keys=False)


def cache_resources(*, ttl_seconds=None, cache_bytes=None):
    resources = load_resources('cache.yaml')
    cache = unique(resources, 'kind', 'SimpleCache')
    if ttl_seconds is not None: cache['spec']['ttlSeconds'] = ttl_seconds
    if cache_bytes is not None: cache['spec']['cacheBytes'] = cache_bytes
    return resources


def database_resources(root, *, password, token, storage='persistent', seed=False):
    if storage not in ['persistent', 'ephemeral', 'memory']:
        raise ValueError('unknown database storage profile')
    resources = load_resources('database.yaml')
    pod = unique(resources, 'kind', 'Pod')
    if storage != 'persistent':
        resources = [r for r in resources if r['kind'] != 'PersistentVolumeClaim']
        volume = unique(pod['spec']['volumes'], 'name', 'data')
        volume.pop('persistentVolumeClaim')
        volume['emptyDir'] = {'medium': 'Memory', 'sizeLimit': '512Mi'} if storage == 'memory' else {}
    data = {'schema.sql': (pathlib.Path(root) / 'deploy/products/schema.sql').read_text()}
    if seed: data['seed.sql'] = (pathlib.Path(root) / 'deploy/products/seed.sql').read_text()
    dynamic = [
        {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'product-schema'}, 'data': data},
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'products'}, 'stringData': {
            'DATABASE_URL': f'postgres://cache:{password}@postgres:5432/products?sslmode=disable',
            'CACHE_ADMIN_TOKEN': token, 'POSTGRES_PASSWORD': password}},
    ]
    return [*dynamic, *resources]


def benchmark_pod(profile, *, image='simplecache-engine:gateway'):
    if profile not in PROFILES:
        raise ValueError('unknown benchmark profile: ' + profile)
    parameters = PROFILES[profile]
    pod = unique(load_resources('benchmark.yaml'), 'kind', 'Pod')
    client = unique(pod['spec']['containers'], 'name', 'client')
    client['image'] = image
    unique(client['env'], 'name', 'GOMAXPROCS')['value'] = str(parameters['gomaxprocs'])
    client['resources']['limits']['cpu'] = parameters['cpu_limit']
    if not parameters['admin_token']:
        client['env'] = [e for e in client['env'] if e['name'] != 'CACHE_ADMIN_TOKEN']
    return pod


def apply_fixture(context, name, resources):
    """Keep the rendered environment reviewable without persisting credentials."""
    rendered = copy.deepcopy(resources)
    for obj in rendered if isinstance(rendered, list) else [rendered]:
        if obj.get('kind') == 'Secret':
            for field in ['data', 'stringData']:
                if field in obj:
                    obj[field] = {key: '<redacted>' for key in obj[field]}
    (context.report / (name + '-resources.json')).write_text(json.dumps(rendered, indent=2))
    context.apply(resources)


def deploy_benchmark(context, profile):
    pod = benchmark_pod(profile)
    apply_fixture(context, 'benchmark', pod)
    client = unique(pod['spec']['containers'], 'name', 'client')
    context.results['benchmark_profile'] = {
        'name': profile, 'image': client['image'],
        'gomaxprocs': unique(client['env'], 'name', 'GOMAXPROCS')['value'],
        'resources': copy.deepcopy(client['resources']),
        'admin_token_reference': PROFILES[profile]['admin_token'],
    }
    context.k('wait', '--for=condition=Ready', 'pod/gateway-bench', '--timeout=120s')


def enable_backend(raw):
    configuration = yaml.safe_load(raw)
    if not isinstance(configuration, dict):
        raise ValueError('Envoy Gateway configuration must be a YAML mapping')
    extensions = configuration.get('extensionApis')
    if extensions is None:
        extensions = configuration['extensionApis'] = {}
    if not isinstance(extensions, dict):
        raise ValueError('Envoy Gateway extensionApis must be a YAML mapping')
    extensions['enableBackend'] = True
    return yaml.safe_dump(configuration, sort_keys=False)
