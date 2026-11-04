"""Complete shared product provisioning; lifecycle policy belongs to the caller."""
import json
import pathlib
import secrets
from .manifests import apply_fixture, cache_resources, database_resources, enable_backend, kind_configuration, unique

def prepare(ctx, *, development=False):
    ctx.run(['bash', 'scripts/gateway/fetch.sh', '--kind'])
    resume = None if development else ctx.env.get('GATEWAY_RESUME_REPORT')
    if resume:
        previous_path = pathlib.Path(resume).resolve()
        assert previous_path.is_relative_to((ctx.root / 'bin/reports').resolve()), 'resume report must belong to this project'
        previous = json.loads((previous_path / 'result.json').read_text())
        assert previous.get('status') == 'failed' and previous.get('phase') == 'setup'
        assert previous.get('cleanup', {}).get('status') == 'retained-for-debug'
        ctx.cluster = previous['cluster']
        assert ctx.cluster.startswith('simplecache-gateway-')
        labels = json.loads(ctx.run(['docker', 'inspect', ctx.cluster + '-control-plane', '--format={{json .Config.Labels}}']))
        assert labels.get('io.x-k8s.kind.cluster') == ctx.cluster and labels.get('io.x-k8s.kind.role') == 'control-plane'
        ctx.proxy_local = ctx.cluster + ':envoy'
        ctx.results['cluster'] = ctx.cluster
        ctx.results['resumed_from'] = str(previous_path)
        ctx.run(['kind', 'export', 'kubeconfig', '--name', ctx.cluster, '--kubeconfig', ctx.env['KUBECONFIG']])
        default_pods = ctx.obj('pods')['items']
        assert not any(p['metadata']['name'] == 'postgres' or p['metadata'].get('labels', {}).get('app') == 'products' for p in default_pods), 'resume only supports a retained pre-application setup'
        ctx.k('wait', '--for=condition=Ready', 'node/' + ctx.cluster + '-control-plane', '--timeout=180s')
    elif development and ctx.existing:
        ctx.run(['kind', 'export', 'kubeconfig', '--name', ctx.cluster, '--kubeconfig', ctx.env['KUBECONFIG']])
        ctx.k('wait', '--for=condition=Ready', 'node/' + ctx.cluster + '-control-plane', '--timeout=180s')
    else:
        # Never reuse any existing user cluster by name.
        assert ctx.cluster not in ctx.run(['kind', 'get', 'clusters']).split()
    for image in [ctx.node, ctx.gateway, ctx.proxy, 'postgres:17-alpine']:
        try:
            ctx.run(['docker', 'image', 'inspect', image], timeout=30)
        except RuntimeError:
            ctx.run(['docker', 'pull', image], timeout=600)
    # Build the exact production Dockerfiles; images are only loaded locally.
    for module, tag in [('geecache-engine', 'simplecache-engine:gateway'), ('simplecache-operator', 'simplecache-operator:gateway')]:
        ctx.run(['docker', 'build', '--build-arg', 'GOPROXY=' + ctx.env.get('GOPROXY', 'https://proxy.golang.org,direct'), '-t', tag, module], timeout=900)
    # Development keeps ordinary etcd storage. Validation explicitly overlays
    # tmpfs etcd for disposable functional tests, not crash-durability claims.
    kind_config = ctx.report / 'kind.yaml'
    kind_config.write_text(kind_configuration(validation=not development))
    ctx.created = True  # Also clean up a partially created cluster on failure.
    if not resume and not (development and ctx.existing):
        if development: ctx.mark_creating()
        ctx.run(['kind', 'create', 'cluster', '--name', ctx.cluster, '--image', ctx.node,
             '--config', str(kind_config), '--kubeconfig', ctx.env['KUBECONFIG'], '--retain', '--wait', '180s'], timeout=600)
    if development: ctx.capture_node()
    # Docker/Kind archives lose the multi-platform registry index reference.
    # Retag the exact digest-verified platform artifact only for this offline
    # smoke; the Operator's ordinary Envoy deployment keeps EG's pinned default.
    ctx.run(['docker', 'tag', ctx.proxy, ctx.proxy_local])
    ctx.local_proxy_tag_created = True
    ctx.results['images'] = {}
    for image in ['simplecache-engine:gateway', 'simplecache-operator:gateway', ctx.gateway, 'postgres:17-alpine', ctx.proxy_local]:
        ctx.results['images'][image] = ctx.run(['docker', 'image', 'inspect', '--format={{.Id}}', image]).strip()
        ctx.run(['kind', 'load', 'docker-image', '--name', ctx.cluster, image], timeout=600)
    ctx.k('apply', '--server-side', '-f', str(ctx.root / 'simplecache-operator/bin/gateway-v1.9.2/install.yaml'), timeout=180)
    config = ctx.obj('configmap', 'envoy-gateway-config', 'envoy-gateway-system')
    config['data']['envoy-gateway.yaml'] = enable_backend(config['data']['envoy-gateway.yaml'])
    ctx.apply(config)
    ctx.k('-n', 'envoy-gateway-system', 'rollout', 'restart', 'deployment/envoy-gateway')
    if development: ctx.k('-n', 'envoy-gateway-system', 'scale', 'deployment/envoy-gateway', '--replicas=1')
    ctx.wait_for('gateway controller', lambda: ctx.obj('deployment', 'envoy-gateway', 'envoy-gateway-system').get('status', {}).get('availableReplicas') == 1, 240)
    ctx.k('wait', '-n', 'envoy-gateway-system', '--for=condition=complete', 'job/eg-gateway-helm-certgen', '--timeout=120s')
    cache = cache_resources(ttl_seconds=None if development else 300, cache_bytes=1048576 if ctx.performance or ctx.arrival else None)
    gateway_class = unique(cache, 'kind', 'GatewayClass')
    desired_proxies = unique(cache, 'kind', 'SimpleCache')['spec']['gateway']['replicas']
    ctx.apply(gateway_class)
    ctx.wait_for('GatewayClass accepted', lambda: ctx.condition(ctx.obj('gatewayclass', gateway_class['metadata']['name']), 'Accepted'))
    rendered = ctx.run(['simplecache-operator/bin/kustomize', 'build', 'deploy/gateway/operator'])
    ctx.k('apply', '-f', '-', data=rendered)
    if development:
        ctx.k('-n', 'simplecache-operator-system', 'scale', 'deployment/simplecache-operator-controller-manager', '--replicas=1')
        ctx.k('-n', 'envoy-gateway-system', 'scale', 'deployment/envoy-gateway', '--replicas=1')
        ctx.k('-n', 'simplecache-operator-system', 'rollout', 'restart', 'deployment/simplecache-operator-controller-manager')
        ctx.k('-n', 'simplecache-operator-system', 'rollout', 'status', 'deployment/simplecache-operator-controller-manager', '--timeout=240s')
    ctx.wait_for('operator', lambda: ctx.obj('deployment', 'simplecache-operator-controller-manager', 'simplecache-operator-system').get('status', {}).get('availableReplicas') == 1)
    if development: ctx.ensure_storage_helper()
    memory_data = ctx.extended or ctx.performance or ctx.arrival
    database = database_resources(ctx.root, password=ctx.password if development else secrets.token_hex(16), token=ctx.token, storage='persistent' if development else ('memory' if memory_data else 'ephemeral'), seed=development)
    if memory_data:
        ctx.results['postgres_storage'] = 'ephemeral emptyDir memory; SQL/notification semantics, no physical durability claim'
    apply_fixture(ctx, 'database', database)
    # Provision the external database first. The image's temporary init server
    # has no TCP listener, so Ready cannot precede schema initialization.
    ctx.wait_for('PostgreSQL initialized', lambda: any(c['type'] == 'Ready' and c['status'] == 'True'
             for c in ctx.obj('pod', 'postgres').get('status', {}).get('conditions', [])), 300)
    ctx.apply(unique(cache, 'kind', 'SimpleCache'))
    ctx.wait_for('EnvoyProxy created', lambda: ctx.obj('envoyproxy', 'products'))
    ctx.k('patch', 'envoyproxy', 'products', '--type=merge', '-p', json.dumps({
        'spec': {'provider': {'kubernetes': {'envoyDeployment': {'container': {'image': ctx.proxy_local}}}}}}))
    ctx.wait_for('cache and gateway ready', lambda: ctx.condition(ctx.obj('simplecache', 'products'), 'Ready'), 300)
    if development:
        ctx.k('rollout', 'restart', 'statefulset/products')
        ctx.k('rollout', 'status', 'statefulset/products', '--timeout=240s')
        ctx.wait_for('cache rollout ready', lambda: ctx.condition(ctx.obj('simplecache', 'products'), 'Ready'), 240)
    selector = 'gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default'
    def ready_proxies():
        pods = [p for p in ctx.obj('pods', namespace='envoy-gateway-system', selector=selector)['items'] if not p['metadata'].get('deletionTimestamp')]
        return pods if len(pods) == desired_proxies and all(any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', [])) for p in pods) else None
    proxies = ctx.wait_for('desired proxy replicas', ready_proxies)
    return proxies
