"""Provision the Gateway/PostgreSQL fixture in an owned Kind cluster."""
import json
import pathlib
import secrets

def prepare(ctx):
    ctx.run(['bash', 'scripts/gateway/fetch.sh', '--kind'])
    resume = ctx.env.get('GATEWAY_RESUME_REPORT')
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
    # This is an ephemeral functional-test control plane. Kind mounts /tmp as
    # tmpfs; keeping only its etcd metadata there avoids slow host disk fsyncs.
    # PostgreSQL keeps its ordinary durability settings, and this is no etcd
    # crash-recovery or control-plane durability test.
    kind_config = ctx.report / 'kind.yaml'
    kind_config.write_text('''kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        timeouts:
          kubernetesAPICall: 4m0s
      - |
        kind: ClusterConfiguration
        etcd:
          local:
            dataDir: /tmp/simplecache-etcd
''')
    ctx.created = True  # Also clean up a partially created cluster on failure.
    if not resume:
        ctx.run(['kind', 'create', 'cluster', '--name', ctx.cluster, '--image', ctx.node,
             '--config', str(kind_config), '--kubeconfig', ctx.env['KUBECONFIG'], '--retain', '--wait', '180s'], timeout=600)
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
    config['data']['envoy-gateway.yaml'] = config['data']['envoy-gateway.yaml'].replace('extensionApis: {}', 'extensionApis:\n  enableBackend: true')
    ctx.apply(config)
    ctx.k('-n', 'envoy-gateway-system', 'rollout', 'restart', 'deployment/envoy-gateway')
    ctx.wait_for('gateway controller', lambda: ctx.obj('deployment', 'envoy-gateway', 'envoy-gateway-system').get('status', {}).get('availableReplicas') == 1, 240)
    ctx.k('wait', '-n', 'envoy-gateway-system', '--for=condition=complete', 'job/eg-gateway-helm-certgen', '--timeout=120s')
    ctx.apply({'apiVersion': 'gateway.networking.k8s.io/v1', 'kind': 'GatewayClass',
           'metadata': {'name': 'eg'}, 'spec': {'controllerName': 'gateway.envoyproxy.io/gatewayclass-controller'}})
    ctx.wait_for('GatewayClass accepted', lambda: ctx.condition(ctx.obj('gatewayclass', 'eg'), 'Accepted'))
    rendered = ctx.run(['simplecache-operator/bin/kustomize', 'build', 'deploy/gateway/operator'])
    ctx.k('apply', '-f', '-', data=rendered)
    ctx.wait_for('operator', lambda: ctx.obj('deployment', 'simplecache-operator-controller-manager', 'simplecache-operator-system').get('status', {}).get('availableReplicas') == 1)
    schema = (ctx.root / 'deploy/products/schema.sql').read_text()
    password = secrets.token_hex(16)
    database_objects = [
        {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'product-schema'}, 'data': {'schema.sql': schema}},
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'products'}, 'stringData': {
            'DATABASE_URL': f'postgres://cache:{password}@postgres:5432/products?sslmode=disable', 'CACHE_ADMIN_TOKEN': ctx.token, 'POSTGRES_PASSWORD': password}},
        {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'postgres', 'labels': {'app': 'postgres'}}, 'spec': {
            'containers': [{'name': 'postgres', 'image': 'postgres:17-alpine', 'env': [
                {'name': 'POSTGRES_USER', 'value': 'cache'}, {'name': 'POSTGRES_DB', 'value': 'products'},
                {'name': 'POSTGRES_PASSWORD', 'valueFrom': {'secretKeyRef': {'name': 'products', 'key': 'POSTGRES_PASSWORD'}}}], 'ports': [{'containerPort': 5432}],
                'volumeMounts': [{'name': 'schema', 'mountPath': '/docker-entrypoint-initdb.d', 'readOnly': True}],
                'readinessProbe': {'tcpSocket': {'port': 5432}, 'periodSeconds': 2, 'timeoutSeconds': 1}}],
            'volumes': [{'name': 'schema', 'configMap': {'name': 'product-schema'}}]}},
        {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': 'postgres'}, 'spec': {'selector': {'app': 'postgres'}, 'ports': [{'port': 5432}]}},
    ]
    if ctx.extended or ctx.performance:
        pg = next(o for o in database_objects if o['kind'] == 'Pod')
        pg['spec']['volumes'].append({'name': 'functional-data', 'emptyDir': {'medium': 'Memory', 'sizeLimit': '512Mi'}})
        pg['spec']['containers'][0]['volumeMounts'].append({'name': 'functional-data', 'mountPath': '/var/lib/postgresql/data'})
        ctx.results['postgres_storage'] = 'ephemeral emptyDir memory; SQL/notification semantics, no physical durability claim'
    ctx.apply(database_objects)
    # Provision the external database first. The image's temporary init server
    # has no TCP listener, so Ready cannot precede schema initialization.
    ctx.wait_for('PostgreSQL initialized', lambda: any(c['type'] == 'Ready' and c['status'] == 'True'
             for c in ctx.obj('pod', 'postgres').get('status', {}).get('conditions', [])), 300)
    ctx.apply({'apiVersion': 'cache.x1kun.com/v1', 'kind': 'SimpleCache', 'metadata': {'name': 'products'}, 'spec': {
        'image': 'simplecache-engine:gateway', 'size': 3, 'sourceSecret': 'products', 'ttlSeconds': 300,
        **({'cacheBytes': 1048576} if ctx.performance else {}),
        'gateway': {'className': 'eg', 'replicas': 2}}})
    ctx.wait_for('EnvoyProxy created', lambda: ctx.obj('envoyproxy', 'products'))
    ctx.k('patch', 'envoyproxy', 'products', '--type=merge', '-p', json.dumps({
        'spec': {'provider': {'kubernetes': {'envoyDeployment': {'container': {'image': ctx.proxy_local}}}}}}))
    ctx.wait_for('cache and gateway ready', lambda: ctx.condition(ctx.obj('simplecache', 'products'), 'Ready'), 300)
    selector = 'gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default'
    def ready_proxies():
        pods = [p for p in ctx.obj('pods', namespace='envoy-gateway-system', selector=selector)['items'] if not p['metadata'].get('deletionTimestamp')]
        return pods if len(pods) == 2 and all(any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', [])) for p in pods) else None
    proxies = ctx.wait_for('two proxy replicas', ready_proxies)
    return proxies
