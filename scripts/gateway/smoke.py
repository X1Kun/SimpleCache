#!/usr/bin/env python3
"""Isolated real-chain checks and exploratory routing measurements."""
import contextlib
import base64
import datetime
import json
import hashlib
import sys
import threading
import os
import pathlib
import secrets
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[2]
NODE = 'kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed'
GATEWAY = 'envoyproxy/gateway:v1.9.2'
KIND = str(ROOT / 'simplecache-operator/bin/gateway-v1.9.2/kind-v0.33.0')
PROXY = 'envoyproxy/envoy:distroless-v1.39.1@sha256:eb2c01c13125d1629637cb4e4cce7207009fb7cc2c8027f9742758549d15b6f4'
REPORT = ROOT / 'bin' / 'reports' / (datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-gateway-' + secrets.token_hex(3))
REPORT.mkdir(parents=True)
CLUSTER = 'simplecache-gateway-' + secrets.token_hex(4)
ENV = {**os.environ, 'KUBECONFIG': str(REPORT / 'kubeconfig')}
TOKEN = secrets.token_urlsafe(24)
PROXY_LOCAL = CLUSTER + ':envoy'
LOCAL_PROXY_TAG_CREATED = False
EXTENDED = os.environ.get('GATEWAY_EXTENDED', '0') == '1'
PERFORMANCE = os.environ.get('GATEWAY_PERF', '0') == '1'
KEEP_FAILED = os.environ.get('GATEWAY_KEEP_FAILED', '0') == '1'
RESULTS = {'cluster': CLUSTER, 'gateway': GATEWAY, 'node': NODE, 'checks': [], 'suite': 'performance' if PERFORMANCE else ('extended' if EXTENDED else 'smoke'), 'phase': 'setup'}
TRACE_LOCK = threading.Lock()
TRACE = (REPORT / 'requests.jsonl').open('w')
STARTED = time.monotonic()
CREATED = False
LOG = (REPORT / 'commands.log').open('w+')


def run(args, *, data=None, timeout=180, cwd=ROOT):
    # Do not log stdin: it can contain the ephemeral database/admin credentials.
    if args[0] == 'kind':
        args = [KIND, *args[1:]]
    print('+', ' '.join(map(str, args)), flush=True)
    if args[:2] == ['docker', 'build']:
        build_log = REPORT / (str(args[-1]) + '-build.log')
        with build_log.open('w') as log:
            p = subprocess.run(list(map(str, args)), text=True, stdout=log, stderr=subprocess.STDOUT, env=ENV, cwd=cwd, timeout=timeout)
        if p.returncode:
            raise RuntimeError(f'Docker build failed: {build_log}\n{build_log.read_text()[-6000:]}')
        return ''
    LOG.write('+ ' + ' '.join(map(str, args)) + '\n')
    LOG.flush()
    start = LOG.tell()
    # Keep long-running Kind/download progress visible before process completion.
    p = subprocess.run(list(map(str, args)), input=data, text=True, stdout=LOG,
                       stderr=subprocess.STDOUT, env=ENV, cwd=cwd, timeout=timeout)
    LOG.flush()
    LOG.seek(start)
    output = LOG.read()
    LOG.seek(0, os.SEEK_END)
    if p.returncode:
        raise RuntimeError(f'{args[0]} exited {p.returncode}: {output[-6000:]}')
    return output


def k(*args, **kwargs):
    return run(['kubectl', '--context', 'kind-' + CLUSTER, *args], **kwargs)


def obj(kind, name=None, namespace='default', selector=None):
    args = ['-n', namespace, 'get', kind]
    if name:
        args.append(name)
    if selector:
        args += ['-l', selector]
    return json.loads(k(*args, '-o', 'json'))


def apply(value):
    if isinstance(value, list):
        value = {'apiVersion': 'v1', 'kind': 'List', 'items': value}
    k('apply', '-f', '-', data=json.dumps(value))


def condition(value, kind):
    return any(c['type'] == kind and c['status'] == 'True' and
               c.get('observedGeneration') == value['metadata']['generation']
               for c in value.get('status', {}).get('conditions', []))


def wait_for(label, predicate, seconds=180):
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            last = predicate()
            if last:
                return last
        except (RuntimeError, urllib.error.URLError, ConnectionError, TimeoutError, AssertionError) as exc:
            last = str(exc)
        time.sleep(2)
    raise AssertionError(f'Timed out: {label}; last={last}')


@contextlib.contextmanager
def forward(namespace, resource, remote):
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        local = s.getsockname()[1]
    log = (REPORT / f'forward-{namespace}-{resource.replace("/", "-")}-{local}.log').open('w')
    p = subprocess.Popen(['kubectl', '--context', 'kind-' + CLUSTER, '-n', namespace,
                          'port-forward', '--address=127.0.0.1', resource, f'{local}:{remote}'],
                         stdout=log, stderr=subprocess.STDOUT, env=ENV)
    try:
        def connected():
            if p.poll() is not None:
                raise RuntimeError('port-forward exited')
            with socket.socket() as s:
                return s.connect_ex(('127.0.0.1', local)) == 0
        wait_for('port-forward', connected, 30)
        yield 'http://127.0.0.1:' + str(local)
    finally:
        p.terminate()
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()
        log.close()


def request(base, key, method='GET', value=None, etag=None, create=False, raw=None, path='/api', event_version=None, auth=True, extra_headers=None, raw_body=None):
    query = raw if raw is not None else urllib.parse.urlencode({'key': key}, quote_via=urllib.parse.quote, safe='')
    headers = {}
    if method != 'GET' and auth:
        headers['Authorization'] = 'Bearer ' + TOKEN
    if event_version is not None:
        headers['X-SimpleCache-Invalidation-Version'] = str(event_version)
    if etag:
        headers['If-Match'] = etag
    if create:
        headers['If-None-Match'] = '*'
    data = None
    if value is not None:
        data = json.dumps(value).encode()
        headers['Content-Type'] = 'application/json'
    if raw_body is not None:
        data = raw_body.encode() if isinstance(raw_body, str) else raw_body
        headers['Content-Type'] = 'application/json'
    headers.update(extra_headers or {})
    req = urllib.request.Request(base + path + '?' + query, data=data, headers=headers, method=method)
    started = time.monotonic()
    try:
        response = urllib.request.urlopen(req, timeout=5)
    except urllib.error.HTTPError as exc:
        response = exc
    with response:
        result = response.status, {k.lower(): v for k, v in response.headers.items()}, response.read().decode()
    with TRACE_LOCK:
        TRACE.write(json.dumps({'phase': RESULTS['phase'], 'method': method, 'key': key, 'status': result[0], 'etag': result[1].get('etag'), 'node': result[1].get('x-simplecache-node'), 'body_sha256': hashlib.sha256(result[2].encode()).hexdigest(), 'elapsed_seconds': time.monotonic()-started}, ensure_ascii=False) + '\n')
        TRACE.flush()
    return result


def phase(name):
    RESULTS['phase'] = name
    (REPORT / 'result.json').write_text(json.dumps(RESULTS, indent=2))
    print('PHASE:', name, flush=True)


def check(name, detail):
    RESULTS['checks'].append({'name': name, 'phase': RESULTS['phase'], 'run_report': str(REPORT), 'elapsed_seconds': time.monotonic()-STARTED, 'detail': detail})
    (REPORT / 'result.json').write_text(json.dumps(RESULTS, indent=2))
    print('PASS:', name, flush=True)


def affinity(bases, keys, expected=None, shards=None):
    mappings = []
    for base in bases:
        mapping = {}
        for key in keys:
            code, headers, body = request(base, key)
            assert code == 200, (key, code, body)
            assert headers.get('etag') == '"v1"', (key, headers)
            assert json.loads(body) == {'key': key}, (key, body)
            mapping[key] = headers['x-simplecache-node']
        mappings.append(mapping)
    assert mappings[0] == mappings[1], 'proxies disagree after convergence'
    if shards is not None:
        assert len(set(mappings[0].values())) == shards, 'not all shards healthy/routed yet'
    if expected is not None:
        assert mappings[0] == expected, 'stable shard addresses remapped after Pod replacement'
    return mappings[0]


def proxies_healthy(admins, addresses):
    for base in admins:
        with urllib.request.urlopen(base + '/clusters?format=json', timeout=5) as response:
            clusters = json.load(response).get('cluster_statuses', [])
        matched = False
        for cluster in clusters:
            hosts = cluster.get('host_statuses', [])
            ips = {h.get('address', {}).get('socket_address', {}).get('address') for h in hosts}
            if ips != addresses:
                continue
            for host in hosts:
                health = host.get('health_status', {})
                assert not any(health.get(name, False) for name in [
                    'failed_active_health_check', 'failed_outlier_check', 'pending_dynamic_removal', 'pending_active_hc']), health
                assert health.get('eds_health_status') not in ['UNHEALTHY', 'DRAINING', 'TIMEOUT'], health
            matched = True
        assert matched, 'backend cluster not yet present on every proxy'
    return True


def worker_metrics(label):
    source_loads = 0
    api_lookups = 0
    for pod in obj('pods', selector='app=products')['items']:
        with forward('default', 'pod/' + pod['metadata']['name'], 9999) as base:
            text = urllib.request.urlopen(base + '/metrics', timeout=5).read().decode()
            (REPORT / (label + '-' + pod['metadata']['name'] + '-metrics.txt')).write_text(text)
            for line in text.splitlines():
                if line.startswith('simplecache_peer_requests_total{') or (line.startswith('simplecache_local_lookups_total{') and 'kind="peer"' in line):
                    assert float(line.rsplit(' ', 1)[1]) == 0, line
                if line.startswith('simplecache_source_loads_total{'):
                    source_loads += float(line.rsplit(' ', 1)[1])
                if line.startswith('simplecache_local_lookups_total{') and 'kind="api"' in line:
                    api_lookups += float(line.rsplit(' ', 1)[1])
    assert api_lookups > 0, 'no API lookup evidence'
    return source_loads


def snapshot(label):
    try:
        run(['kind', 'export', 'logs', '--name', CLUSTER, str(REPORT / (label + '-kind-logs'))], timeout=120)
    except Exception as exc:
        LOG.write(f'Kind log export failed: {exc}\n')
    if not pathlib.Path(ENV['KUBECONFIG']).exists():
        return
    kinds = ['simplecaches', 'statefulsets', 'services', 'pods', 'gateways', 'httproutes',
             'backends', 'backendtrafficpolicies', 'envoyproxies']
    for kind in kinds:
        try:
            (REPORT / f'{label}-{kind}.json').write_text(k('get', kind, '-A', '-o', 'json'))
        except Exception as exc:
            LOG.write(f'Snapshot failed: {kind}: {exc}\n')
    for ns, target in [('default', 'pod/postgres'), ('default', 'statefulset/products'), ('simplecache-operator-system', 'deployment/simplecache-operator-controller-manager'),
                       ('envoy-gateway-system', 'deployment/envoy-gateway')]:
        try:
            (REPORT / f'{label}-{ns}.log').write_text(k('-n', ns, 'logs', target, '--all-containers', '--tail=250'))
        except Exception:
            pass


def continue_retained_experiment(previous_name):
    global CLUSTER, PROXY_LOCAL, CREATED, LOCAL_PROXY_TAG_CREATED
    previous_path = pathlib.Path(previous_name).resolve()
    assert previous_path.is_relative_to((ROOT / 'bin/reports').resolve())
    old = json.loads((previous_path / 'result.json').read_text())
    assert old.get('status') == 'failed' and old.get('suite') == 'extended'
    assert old.get('cleanup', {}).get('status') == 'retained-for-debug'
    assert old['phase'] in ['database-connectivity-loss', 'restart-after-missed-notification', 'operator-child-repair', 'gateway-proxy-restart']
    CLUSTER = old['cluster']
    assert CLUSTER.startswith('simplecache-gateway-')
    labels = json.loads(run(['docker', 'inspect', CLUSTER + '-control-plane', '--format={{json .Config.Labels}}']))
    assert labels.get('io.x-k8s.kind.cluster') == CLUSTER and labels.get('io.x-k8s.kind.role') == 'control-plane'
    run(['kind', 'export', 'kubeconfig', '--name', CLUSTER, '--kubeconfig', ENV['KUBECONFIG']])
    old_crs = json.loads((previous_path / 'failure-simplecaches.json').read_text())['items']
    old_cr = next(c for c in old_crs if c['metadata']['name'] == 'products' and c['metadata']['namespace'] == 'default')
    current = obj('simplecache', 'products')
    assert current['metadata']['uid'] == old_cr['metadata']['uid'], 'cache ownership changed'
    old_pods = json.loads((previous_path / 'failure-pods.json').read_text())['items']
    old_db = next(p for p in old_pods if p['metadata']['name'] == 'postgres' and p['metadata']['namespace'] == 'default')
    assert obj('pod', 'postgres')['metadata']['uid'] == old_db['metadata']['uid'], 'database fixture changed'
    assert current['spec']['size'] == 3 and current['spec']['ttlSeconds'] == 20
    assert obj('service', 'postgres')['spec']['selector'] == {'app': 'postgres'}, 'database fault was not restored'
    # Read only the token created by this exact fixture; never send it to logs.
    secret_raw = subprocess.check_output(['kubectl', '--context', 'kind-' + CLUSTER, '-n', 'default', 'get', 'secret', 'products', '-o', 'json'], env=ENV, text=True, timeout=30)
    globals()['TOKEN'] = base64.b64decode(json.loads(secret_raw)['data']['CACHE_ADMIN_TOKEN']).decode()
    CREATED = True
    PROXY_LOCAL = CLUSTER + ':envoy'
    run(['docker', 'image', 'inspect', '--format={{.Id}}', PROXY_LOCAL])
    LOCAL_PROXY_TAG_CREATED = True
    RESULTS.update({'cluster': CLUSTER, 'continued_from': str(previous_path), 'images': old['images'],
                    'postgres_storage': old.get('postgres_storage'), 'phase': old['phase'],
                    'checks': [dict(c, run_report=c.get('run_report', str(previous_path))) for c in old['checks']]})
    pods = [p for p in obj('pods', namespace='envoy-gateway-system', selector='gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default')['items'] if not p['metadata'].get('deletionTimestamp')]
    assert len(pods) == 2
    assert all(any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', [])) for p in pods)
    remaining = ['item-' + str(i) for i in range(1, 48)] + ['商品:a b+c/d?&']
    with contextlib.ExitStack() as stack:
        bases = [stack.enter_context(forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 10080)) for p in pods]
        admins = [stack.enter_context(forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 19000)) for p in pods]
        addresses = {obj('service', f'products-shard-{i}')['spec']['clusterIP'] for i in range(3)}
        wait_for('retained fixture health converged', lambda: proxies_healthy(admins, addresses), 90)
        from scenarios import continue_exercise
        continue_exercise(sys.modules[__name__], stack, bases, admins, remaining, [p['metadata']['uid'] for p in pods], old['phase'])
        phase('final-validation')
        worker_metrics('final')
        check('gateway reads do not perform Peer forwarding', 'per-worker metrics captured')
    assert len(RESULTS['checks']) == 26, 'scenario coverage incomplete'
    snapshot('success')
    RESULTS['status'] = 'passed'


def main():
    global CREATED, LOCAL_PROXY_TAG_CREATED, CLUSTER, PROXY_LOCAL
    print('Report:', REPORT, flush=True)
    performance_continuation = os.environ.get('GATEWAY_PERF_CONTINUE_REPORT')
    if performance_continuation:
        assert PERFORMANCE
        from performance import continue_retained
        return continue_retained(sys.modules[__name__], performance_continuation)
    continuation = os.environ.get('GATEWAY_CONTINUE_REPORT')
    if continuation:
        assert EXTENDED
        return continue_retained_experiment(continuation)
    run(['bash', 'scripts/gateway/fetch.sh', '--kind'])
    resume = os.environ.get('GATEWAY_RESUME_REPORT')
    if resume:
        previous_path = pathlib.Path(resume).resolve()
        assert previous_path.is_relative_to((ROOT / 'bin/reports').resolve()), 'resume report must belong to this project'
        previous = json.loads((previous_path / 'result.json').read_text())
        assert previous.get('status') == 'failed' and previous.get('phase') == 'setup'
        assert previous.get('cleanup', {}).get('status') == 'retained-for-debug'
        CLUSTER = previous['cluster']
        assert CLUSTER.startswith('simplecache-gateway-')
        labels = json.loads(run(['docker', 'inspect', CLUSTER + '-control-plane', '--format={{json .Config.Labels}}']))
        assert labels.get('io.x-k8s.kind.cluster') == CLUSTER and labels.get('io.x-k8s.kind.role') == 'control-plane'
        PROXY_LOCAL = CLUSTER + ':envoy'
        RESULTS['cluster'] = CLUSTER
        RESULTS['resumed_from'] = str(previous_path)
        run(['kind', 'export', 'kubeconfig', '--name', CLUSTER, '--kubeconfig', ENV['KUBECONFIG']])
        default_pods = obj('pods')['items']
        assert not any(p['metadata']['name'] == 'postgres' or p['metadata'].get('labels', {}).get('app') == 'products' for p in default_pods), 'resume only supports a retained pre-application setup'
        k('wait', '--for=condition=Ready', 'node/' + CLUSTER + '-control-plane', '--timeout=180s')
    else:
        # Never reuse any existing user cluster by name.
        assert CLUSTER not in run(['kind', 'get', 'clusters']).split()
    for image in [NODE, GATEWAY, PROXY, 'postgres:17-alpine']:
        try:
            run(['docker', 'image', 'inspect', image], timeout=30)
        except RuntimeError:
            run(['docker', 'pull', image], timeout=600)
    # Build the exact production Dockerfiles; images are only loaded locally.
    for module, tag in [('geecache-engine', 'simplecache-engine:gateway'), ('simplecache-operator', 'simplecache-operator:gateway')]:
        run(['docker', 'build', '--build-arg', 'GOPROXY=' + os.environ.get('GOPROXY', 'https://proxy.golang.org,direct'), '-t', tag, module], timeout=900)
    # This is an ephemeral functional-test control plane. Kind mounts /tmp as
    # tmpfs; keeping only its etcd metadata there avoids slow host disk fsyncs.
    # PostgreSQL keeps its ordinary durability settings, and this is no etcd
    # crash-recovery or control-plane durability test.
    kind_config = REPORT / 'kind.yaml'
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
    CREATED = True  # Also clean up a partially created cluster on failure.
    if not resume:
        run(['kind', 'create', 'cluster', '--name', CLUSTER, '--image', NODE,
             '--config', str(kind_config), '--kubeconfig', ENV['KUBECONFIG'], '--retain', '--wait', '180s'], timeout=600)
    # Docker/Kind archives lose the multi-platform registry index reference.
    # Retag the exact digest-verified platform artifact only for this offline
    # smoke; the Operator's ordinary Envoy deployment keeps EG's pinned default.
    run(['docker', 'tag', PROXY, PROXY_LOCAL])
    LOCAL_PROXY_TAG_CREATED = True
    RESULTS['images'] = {}
    for image in ['simplecache-engine:gateway', 'simplecache-operator:gateway', GATEWAY, 'postgres:17-alpine', PROXY_LOCAL]:
        RESULTS['images'][image] = run(['docker', 'image', 'inspect', '--format={{.Id}}', image]).strip()
        run(['kind', 'load', 'docker-image', '--name', CLUSTER, image], timeout=600)
    k('apply', '--server-side', '-f', str(ROOT / 'simplecache-operator/bin/gateway-v1.9.2/install.yaml'), timeout=180)
    config = obj('configmap', 'envoy-gateway-config', 'envoy-gateway-system')
    config['data']['envoy-gateway.yaml'] = config['data']['envoy-gateway.yaml'].replace('extensionApis: {}', 'extensionApis:\n  enableBackend: true')
    apply(config)
    k('-n', 'envoy-gateway-system', 'rollout', 'restart', 'deployment/envoy-gateway')
    wait_for('gateway controller', lambda: obj('deployment', 'envoy-gateway', 'envoy-gateway-system').get('status', {}).get('availableReplicas') == 1, 240)
    k('wait', '-n', 'envoy-gateway-system', '--for=condition=complete', 'job/eg-gateway-helm-certgen', '--timeout=120s')
    apply({'apiVersion': 'gateway.networking.k8s.io/v1', 'kind': 'GatewayClass',
           'metadata': {'name': 'eg'}, 'spec': {'controllerName': 'gateway.envoyproxy.io/gatewayclass-controller'}})
    wait_for('GatewayClass accepted', lambda: condition(obj('gatewayclass', 'eg'), 'Accepted'))
    rendered = run(['simplecache-operator/bin/kustomize', 'build', 'deploy/gateway/operator'])
    k('apply', '-f', '-', data=rendered)
    wait_for('operator', lambda: obj('deployment', 'simplecache-operator-controller-manager', 'simplecache-operator-system').get('status', {}).get('availableReplicas') == 1)
    schema = (ROOT / 'deploy/products/schema.sql').read_text()
    password = secrets.token_hex(16)
    database_objects = [
        {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'product-schema'}, 'data': {'schema.sql': schema}},
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'products'}, 'stringData': {
            'DATABASE_URL': f'postgres://cache:{password}@postgres:5432/products?sslmode=disable', 'CACHE_ADMIN_TOKEN': TOKEN, 'POSTGRES_PASSWORD': password}},
        {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'postgres', 'labels': {'app': 'postgres'}}, 'spec': {
            'containers': [{'name': 'postgres', 'image': 'postgres:17-alpine', 'env': [
                {'name': 'POSTGRES_USER', 'value': 'cache'}, {'name': 'POSTGRES_DB', 'value': 'products'},
                {'name': 'POSTGRES_PASSWORD', 'valueFrom': {'secretKeyRef': {'name': 'products', 'key': 'POSTGRES_PASSWORD'}}}], 'ports': [{'containerPort': 5432}],
                'volumeMounts': [{'name': 'schema', 'mountPath': '/docker-entrypoint-initdb.d', 'readOnly': True}],
                'readinessProbe': {'tcpSocket': {'port': 5432}, 'periodSeconds': 2, 'timeoutSeconds': 1}}],
            'volumes': [{'name': 'schema', 'configMap': {'name': 'product-schema'}}]}},
        {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': 'postgres'}, 'spec': {'selector': {'app': 'postgres'}, 'ports': [{'port': 5432}]}},
    ]
    if EXTENDED or PERFORMANCE:
        pg = next(o for o in database_objects if o['kind'] == 'Pod')
        pg['spec']['volumes'].append({'name': 'functional-data', 'emptyDir': {'medium': 'Memory', 'sizeLimit': '512Mi'}})
        pg['spec']['containers'][0]['volumeMounts'].append({'name': 'functional-data', 'mountPath': '/var/lib/postgresql/data'})
        RESULTS['postgres_storage'] = 'ephemeral emptyDir memory; SQL/notification semantics, no physical durability claim'
    apply(database_objects)
    # Provision the external database first. The image's temporary init server
    # has no TCP listener, so Ready cannot precede schema initialization.
    wait_for('PostgreSQL initialized', lambda: any(c['type'] == 'Ready' and c['status'] == 'True'
             for c in obj('pod', 'postgres').get('status', {}).get('conditions', [])), 300)
    apply({'apiVersion': 'cache.x1kun.com/v1', 'kind': 'SimpleCache', 'metadata': {'name': 'products'}, 'spec': {
        'image': 'simplecache-engine:gateway', 'size': 3, 'sourceSecret': 'products', 'ttlSeconds': 300,
        **({'cacheBytes': 1048576} if PERFORMANCE else {}),
        'gateway': {'className': 'eg', 'replicas': 2}}})
    wait_for('EnvoyProxy created', lambda: obj('envoyproxy', 'products'))
    k('patch', 'envoyproxy', 'products', '--type=merge', '-p', json.dumps({
        'spec': {'provider': {'kubernetes': {'envoyDeployment': {'container': {'image': PROXY_LOCAL}}}}}}))
    wait_for('cache and gateway ready', lambda: condition(obj('simplecache', 'products'), 'Ready'), 300)
    selector = 'gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default'
    def ready_proxies():
        pods = [p for p in obj('pods', namespace='envoy-gateway-system', selector=selector)['items'] if not p['metadata'].get('deletionTimestamp')]
        return pods if len(pods) == 2 and all(any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', [])) for p in pods) else None
    proxies = wait_for('two proxy replicas', ready_proxies)
    if PERFORMANCE:
        from performance import measure
        measure(sys.modules[__name__], proxies)
        snapshot('success')
        RESULTS['status'] = RESULTS.get('performance_status', 'passed')
        return
    phase('routing-convergence')
    keys = ['item-' + str(i) for i in range(48)] + ['商品:a b+c/d?&']
    def literal(value):
        return "'" + value.replace("'", "''") + "'"
    seed = '\n'.join('INSERT INTO simplecache_products(key,value) VALUES (' + literal(key) +
                     ',convert_to(' + literal(json.dumps({'key': key}, ensure_ascii=False)) + ",'UTF8'));" for key in keys)
    k('exec', '-i', 'postgres', '--', 'psql', '-U', 'cache', '-d', 'products', '-v', 'ON_ERROR_STOP=1', data=seed)
    with contextlib.ExitStack() as stack:
        bases = [stack.enter_context(forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 10080)) for p in proxies]
        admins = [stack.enter_context(forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 19000)) for p in proxies]
        addresses = {obj('service', f'products-shard-{i}')['spec']['clusterIP'] for i in range(3)}
        wait_for('all shard health views converge', lambda: proxies_healthy(admins, addresses), 90)
        # Health/xDS views converge independently. Require repeated agreement.
        mapping = wait_for('stable cross-proxy affinity', lambda: affinity(bases, keys, shards=3), 90)
        assert len(set(mapping.values())) == 3, mapping
        assert affinity(bases, keys) == mapping
        # Config/health views may differ before affinity converges. Retain that
        # startup work, then measure the stable repeat as a separate delta.
        phase('stable-reads')
        startup_loads = worker_metrics('warm')
        assert len(keys) <= startup_loads <= len(keys) * 3, startup_loads
        assert affinity(bases, keys) == mapping
        assert worker_metrics('repeat') == startup_loads, 'stable warm requests reached source'
        check('shared warm cache across proxy replicas', {'unique_keys': len(keys), 'startup_source_loads': startup_loads, 'stable_repeat_source_loads': 0})
        check('two proxies agree on all keys', {'keys': len(keys), 'shards': sorted(set(mapping.values()))})
        for raw in ['key=item%2d0', 'key=item-0&key=item-1', 'key=item-0&extra=x', 'key=a+b']:
            assert request(bases[0], '', raw=raw)[0] == 400, raw
        check('ambiguous query encodings rejected', 4)
        phase('versioned-writes')
        key = keys[0]
        code, headers, _ = request(bases[0], key)
        assert code == 200
        code, _, body = request(bases[1], key, 'PUT', 'new-value', etag=headers['etag'])
        assert code == 200, (code, body)
        updated = json.loads(body)
        assert updated['source_version'] == updated['invalidation']['version'] == 2
        assert len(updated['invalidation']['results']) == 3 and updated['invalidation']['complete']
        code, headers, body = request(bases[0], key)
        assert (code, headers['etag'], json.loads(body)) == (200, '"v2"', 'new-value'), (code, headers, body)
        before_events = worker_metrics('before-version-replays')
        for base, event in [(bases[0], 1), (bases[1], 2), (bases[0], 1)]:
            status, _, result = request(base, key, 'POST', path='/invalidate', event_version=event)
            assert status == 200, result
            assert json.loads(result)['invalidation']['version'] == event
            code, h, payload = request(base, key)
            assert (code, h['etag'], json.loads(payload)) == (200, '"v2"', 'new-value')
        assert worker_metrics('after-version-replays') == before_events
        check('old and duplicate version events preserve the current cache', 'v1/v2 replays caused no additional source reads')
        assert request(bases[0], key, 'PUT', 'stale', etag='"v1"')[0] == 412
        assert request(bases[1], key, 'DELETE', etag='"v2"')[0] == 200
        assert request(bases[0], key)[0] == 404
        check('PostgreSQL conditional write and invalidation through gateway', 'v1 -> v2 -> tombstone, stale write 412')
        status, _, body = request(bases[0], key, 'PUT', 'recreated', create=True)
        assert status == 200 and json.loads(body)['source_version'] == 4, body
        assert request(bases[1], key)[1]['etag'] == '"v4"'
        before_delete_replay = worker_metrics('before-delete-replay')
        assert request(bases[1], key, 'POST', path='/invalidate', event_version=3)[0] == 200
        status, h, body = request(bases[0], key)
        assert (status, h['etag'], json.loads(body)) == (200, '"v4"', 'recreated')
        assert worker_metrics('after-delete-replay') == before_delete_replay
        check('late deletion notification preserves a recreated value', 'v4 remained cached after replaying delete v3')
        phase('worker-replacement')
        remaining = keys[1:]
        before = {key: value for key, value in mapping.items() if key in remaining}
        owner = before[remaining[0]].split('/')[-1]
        shard = owner.rsplit('-', 1)[-1]
        svc_ip = obj('service', 'products-shard-' + shard)['spec']['clusterIP']
        old_uid = obj('pod', owner)['metadata']['uid']
        k('delete', 'pod', owner, '--wait=false')
        wait_for('replacement Pod ready', lambda: obj('pod', owner)['metadata']['uid'] != old_uid and any(c['type'] == 'Ready' and c['status'] == 'True' for c in obj('pod', owner).get('status', {}).get('conditions', [])))
        wait_for('replacement keeps ownership', lambda: affinity(bases, remaining, before), 90)
        assert obj('service', 'products-shard-' + shard)['spec']['clusterIP'] == svc_ip
        check('Pod replacement preserves shard identity after health convergence', owner)
        phase('shard-scaling')
        k('patch', 'simplecache', 'products', '--type=merge', '-p', '{"spec":{"size":4}}')
        wait_for('scale up ready', lambda: condition(obj('simplecache', 'products'), 'Ready'))
        grown = wait_for('scale up convergence', lambda: affinity(bases, remaining, shards=4), 90)
        assert len(set(grown.values())) == 4
        moved = sum(grown[key] != before[key] for key in remaining)
        assert 0 < moved < len(remaining), moved
        check('3 -> 4 shards converges across proxies', {'moved': moved, 'total': len(remaining)})
        k('patch', 'simplecache', 'products', '--type=merge', '-p', '{"spec":{"size":3}}')
        wait_for('scale down ready', lambda: condition(obj('simplecache', 'products'), 'Ready'))
        wait_for('scale down restores stable addresses', lambda: affinity(bases, remaining, before), 90)
        check('4 -> 3 shards removes backend and restores affinity', len(remaining))
        if EXTENDED:
            from scenarios import exercise
            exercise(sys.modules[__name__], stack, bases, admins, remaining, [p['metadata']['uid'] for p in proxies])
        phase('final-validation')
        worker_metrics('final')
        check('gateway reads do not perform Peer forwarding', 'per-worker metrics captured')
    assert len(RESULTS['checks']) == (26 if EXTENDED else 10), 'scenario coverage incomplete'
    snapshot('success')
    RESULTS['status'] = 'passed'


def interrupted(signum, frame):
    raise KeyboardInterrupt(f'interrupted by signal {signum}')


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, interrupted)
    try:
        main()
    except BaseException as exc:
        RESULTS['status'] = 'failed'
        RESULTS['error'] = str(exc)
        if CREATED:
            snapshot('failure')
        raise
    finally:
        primary_failed = RESULTS.get('status') == 'failed'
        cleanup_error = None
        RESULTS['cleanup'] = {'status': 'not-created'}
        if CREATED and primary_failed and KEEP_FAILED:
            RESULTS['cleanup'] = {'status': 'retained-for-debug', 'context': 'kind-' + CLUSTER, 'kubeconfig': ENV['KUBECONFIG']}
        elif CREATED:
            for attempt in range(3):
                try:
                    run(['kind', 'delete', 'cluster', '--name', CLUSTER], timeout=120)
                    RESULTS['cleanup'] = {'status': 'passed', 'attempts': attempt + 1}
                    cleanup_error = None
                    break
                except Exception as exc:
                    cleanup_error = exc
                    if attempt < 2:
                        time.sleep(2 + 3 * attempt)
            if cleanup_error is not None:
                RESULTS['cleanup'] = {'status': 'failed', 'error': str(cleanup_error)}
                RESULTS['status'] = 'failed'
        if LOCAL_PROXY_TAG_CREATED and not (primary_failed and KEEP_FAILED):
            try:
                run(['docker', 'image', 'rm', PROXY_LOCAL], timeout=30)
            except Exception as exc:
                RESULTS['local_tag_cleanup_error'] = str(exc)
                RESULTS['status'] = 'failed'
                if cleanup_error is None:
                    cleanup_error = exc
        (REPORT / 'result.json').write_text(json.dumps(RESULTS, indent=2))
        TRACE.close()
        LOG.close()
        print('Report:', REPORT, flush=True)
        if cleanup_error is not None and not primary_failed:
            raise RuntimeError('test resource cleanup failed; see result.json') from cleanup_error
