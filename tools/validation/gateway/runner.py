"""Suite selection and functional Gateway experiments."""
import contextlib
import base64
import json
import pathlib
import subprocess
from ..environment.kind import KindEnvironment
from ..environment.gateway import prepare
from .checks import GatewayChecks

class GatewayRun(GatewayChecks, KindEnvironment):
    pass

def continue_retained_experiment(ctx, previous_name):
    previous_path = pathlib.Path(previous_name).resolve()
    assert previous_path.is_relative_to((ctx.root / 'bin/reports').resolve())
    old = json.loads((previous_path / 'result.json').read_text())
    assert old.get('status') == 'failed' and old.get('suite') == 'extended'
    assert old.get('cleanup', {}).get('status') == 'retained-for-debug'
    assert old['phase'] in ['database-connectivity-loss', 'restart-after-missed-notification', 'operator-child-repair', 'gateway-proxy-restart']
    ctx.cluster = old['cluster']
    assert ctx.cluster.startswith('simplecache-gateway-')
    labels = json.loads(ctx.run(['docker', 'inspect', ctx.cluster + '-control-plane', '--format={{json .Config.Labels}}']))
    assert labels.get('io.x-k8s.kind.cluster') == ctx.cluster and labels.get('io.x-k8s.kind.role') == 'control-plane'
    ctx.run(['kind', 'export', 'kubeconfig', '--name', ctx.cluster, '--kubeconfig', ctx.env['KUBECONFIG']])
    old_crs = json.loads((previous_path / 'failure-simplecaches.json').read_text())['items']
    old_cr = next(c for c in old_crs if c['metadata']['name'] == 'products' and c['metadata']['namespace'] == 'default')
    current = ctx.obj('simplecache', 'products')
    assert current['metadata']['uid'] == old_cr['metadata']['uid'], 'cache ownership changed'
    old_pods = json.loads((previous_path / 'failure-pods.json').read_text())['items']
    old_db = next(p for p in old_pods if p['metadata']['name'] == 'postgres' and p['metadata']['namespace'] == 'default')
    assert ctx.obj('pod', 'postgres')['metadata']['uid'] == old_db['metadata']['uid'], 'database fixture changed'
    assert current['spec']['size'] == 3 and current['spec']['ttlSeconds'] == 20
    assert ctx.obj('service', 'postgres')['spec']['selector'] == {'app': 'postgres'}, 'database fault was not restored'
    # Read only the token created by this exact fixture; never send it to logs.
    secret_raw = subprocess.check_output(['kubectl', '--context', 'kind-' + ctx.cluster, '-n', 'default', 'get', 'secret', 'products', '-o', 'json'], env=ctx.env, text=True, timeout=30)
    ctx.token = base64.b64decode(json.loads(secret_raw)['data']['CACHE_ADMIN_TOKEN']).decode()
    ctx.created = True
    ctx.proxy_local = ctx.cluster + ':envoy'
    ctx.run(['docker', 'image', 'inspect', '--format={{.Id}}', ctx.proxy_local])
    ctx.local_proxy_tag_created = True
    ctx.results.update({'cluster': ctx.cluster, 'continued_from': str(previous_path), 'images': old['images'],
                    'postgres_storage': old.get('postgres_storage'), 'phase': old['phase'],
                    'checks': [dict(c, run_report=c.get('run_report', str(previous_path))) for c in old['checks']]})
    pods = [p for p in ctx.obj('pods', namespace='envoy-gateway-system', selector='gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default')['items'] if not p['metadata'].get('deletionTimestamp')]
    assert len(pods) == 2
    assert all(any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', [])) for p in pods)
    remaining = ['item-' + str(i) for i in range(1, 48)] + ['商品:a b+c/d?&']
    with contextlib.ExitStack() as stack:
        bases = [stack.enter_context(ctx.forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 10080)) for p in pods]
        admins = [stack.enter_context(ctx.forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 19000)) for p in pods]
        addresses = {ctx.obj('service', f'products-shard-{i}')['spec']['clusterIP'] for i in range(3)}
        ctx.wait_for('retained fixture health converged', lambda: ctx.proxies_healthy(admins, addresses), 90)
        from .faults import continue_exercise
        continue_exercise(ctx, stack, bases, admins, remaining, [p['metadata']['uid'] for p in pods], old['phase'])
        ctx.phase('final-validation')
        ctx.worker_metrics('final')
        for pod in ctx.obj('pods', selector='app=products')['items']:
            with ctx.forward('default', 'pod/' + pod['metadata']['name'], 8001) as member:
                assert ctx.request(member, 'item-1', path='/_geecache/products/item-1')[0] == 405
                assert ctx.request(member, 'item-1', path='/_simplecache/invalidate/products/item-1')[0] == 405
        ctx.check('local cache API and notification-only member transport', 'per-worker metrics captured')
    assert len(ctx.results['checks']) == 26, 'scenario coverage incomplete'
    ctx.snapshot('success')
    ctx.results['status'] = 'passed'


def run_suite(ctx):
    print('Report:', ctx.report, flush=True)
    performance_continuation = ctx.env.get('GATEWAY_PERF_CONTINUE_REPORT')
    if performance_continuation:
        assert ctx.performance
        from .performance import continue_retained
        return continue_retained(ctx, performance_continuation)
    continuation = ctx.env.get('GATEWAY_CONTINUE_REPORT')
    if continuation:
        assert ctx.extended
        return continue_retained_experiment(ctx, continuation)
    proxies = prepare(ctx)
    if ctx.arrival:
        from .arrival import measure
        measure(ctx, proxies)
        ctx.snapshot('success')
        return
    if ctx.performance:
        from .performance import measure
        measure(ctx, proxies)
        ctx.snapshot('success')
        ctx.results['status'] = ctx.results.get('performance_status', 'passed')
        return
    ctx.phase('routing-convergence')
    keys = ['item-' + str(i) for i in range(48)] + ['商品:a b+c/d?&']
    def literal(value):
        return "'" + value.replace("'", "''") + "'"
    seed = '\n'.join('INSERT INTO simplecache_products(key,value) VALUES (' + literal(key) +
                     ',convert_to(' + literal(json.dumps({'key': key}, ensure_ascii=False)) + ",'UTF8'));" for key in keys)
    ctx.k('exec', '-i', 'postgres', '--', 'psql', '-U', 'cache', '-d', 'products', '-v', 'ON_ERROR_STOP=1', data=seed)
    with contextlib.ExitStack() as stack:
        bases = [stack.enter_context(ctx.forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 10080)) for p in proxies]
        admins = [stack.enter_context(ctx.forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 19000)) for p in proxies]
        addresses = {ctx.obj('service', f'products-shard-{i}')['spec']['clusterIP'] for i in range(3)}
        ctx.wait_for('all shard health views converge', lambda: ctx.proxies_healthy(admins, addresses), 90)
        # Health/xDS views converge independently. Require repeated agreement.
        mapping = ctx.wait_for('stable cross-proxy affinity', lambda: ctx.affinity(bases, keys, shards=3), 90)
        assert len(set(mapping.values())) == 3, mapping
        assert ctx.affinity(bases, keys) == mapping
        # Config/health views may differ before affinity converges. Retain that
        # startup work, then measure the stable repeat as a separate delta.
        ctx.phase('stable-reads')
        startup_loads = ctx.worker_metrics('warm')
        assert len(keys) <= startup_loads <= len(keys) * 3, startup_loads
        assert ctx.affinity(bases, keys) == mapping
        assert ctx.worker_metrics('repeat') == startup_loads, 'stable warm requests reached source'
        ctx.check('shared warm cache across proxy replicas', {'unique_keys': len(keys), 'startup_source_loads': startup_loads, 'stable_repeat_source_loads': 0})
        ctx.check('two proxies agree on all keys', {'keys': len(keys), 'shards': sorted(set(mapping.values()))})
        for raw in ['key=item%2d0', 'key=item-0&key=item-1', 'key=item-0&extra=x', 'key=a+b']:
            assert ctx.request(bases[0], '', raw=raw)[0] == 400, raw
        ctx.check('ambiguous query encodings rejected', 4)
        ctx.phase('versioned-writes')
        key = keys[0]
        code, headers, _ = ctx.request(bases[0], key)
        assert code == 200
        code, _, body = ctx.request(bases[1], key, 'PUT', 'new-value', etag=headers['etag'])
        assert code == 200, (code, body)
        updated = json.loads(body)
        assert updated['source_version'] == updated['invalidation']['version'] == 2
        assert len(updated['invalidation']['results']) == 3 and updated['invalidation']['complete']
        code, headers, body = ctx.request(bases[0], key)
        assert (code, headers['etag'], json.loads(body)) == (200, '"v2"', 'new-value'), (code, headers, body)
        before_events = ctx.worker_metrics('before-version-replays')
        for base, event in [(bases[0], 1), (bases[1], 2), (bases[0], 1)]:
            status, _, result = ctx.request(base, key, 'POST', path='/invalidate', event_version=event)
            assert status == 200, result
            assert json.loads(result)['invalidation']['version'] == event
            code, h, payload = ctx.request(base, key)
            assert (code, h['etag'], json.loads(payload)) == (200, '"v2"', 'new-value')
        assert ctx.worker_metrics('after-version-replays') == before_events
        ctx.check('old and duplicate version events preserve the current cache', 'v1/v2 replays caused no additional source reads')
        assert ctx.request(bases[0], key, 'PUT', 'stale', etag='"v1"')[0] == 412
        assert ctx.request(bases[1], key, 'DELETE', etag='"v2"')[0] == 200
        assert ctx.request(bases[0], key)[0] == 404
        ctx.check('PostgreSQL conditional write and invalidation through gateway', 'v1 -> v2 -> tombstone, stale write 412')
        status, _, body = ctx.request(bases[0], key, 'PUT', 'recreated', create=True)
        assert status == 200 and json.loads(body)['source_version'] == 4, body
        assert ctx.request(bases[1], key)[1]['etag'] == '"v4"'
        before_delete_replay = ctx.worker_metrics('before-delete-replay')
        assert ctx.request(bases[1], key, 'POST', path='/invalidate', event_version=3)[0] == 200
        status, h, body = ctx.request(bases[0], key)
        assert (status, h['etag'], json.loads(body)) == (200, '"v4"', 'recreated')
        assert ctx.worker_metrics('after-delete-replay') == before_delete_replay
        ctx.check('late deletion notification preserves a recreated value', 'v4 remained cached after replaying delete v3')
        ctx.phase('worker-replacement')
        remaining = keys[1:]
        before = {key: value for key, value in mapping.items() if key in remaining}
        owner = before[remaining[0]].split('/')[-1]
        shard = owner.rsplit('-', 1)[-1]
        svc_ip = ctx.obj('service', 'products-shard-' + shard)['spec']['clusterIP']
        old_uid = ctx.obj('pod', owner)['metadata']['uid']
        ctx.k('delete', 'pod', owner, '--wait=false')
        ctx.wait_for('replacement Pod ready', lambda: ctx.obj('pod', owner)['metadata']['uid'] != old_uid and any(c['type'] == 'Ready' and c['status'] == 'True' for c in ctx.obj('pod', owner).get('status', {}).get('conditions', [])))
        ctx.wait_for('replacement keeps ownership', lambda: ctx.affinity(bases, remaining, before), 90)
        assert ctx.obj('service', 'products-shard-' + shard)['spec']['clusterIP'] == svc_ip
        ctx.check('Pod replacement preserves shard identity after health convergence', owner)
        ctx.phase('shard-scaling')
        ctx.k('patch', 'simplecache', 'products', '--type=merge', '-p', '{"spec":{"size":4}}')
        ctx.wait_for('scale up ready', lambda: ctx.condition(ctx.obj('simplecache', 'products'), 'Ready'))
        grown = ctx.wait_for('scale up convergence', lambda: ctx.affinity(bases, remaining, shards=4), 90)
        assert len(set(grown.values())) == 4
        moved = sum(grown[key] != before[key] for key in remaining)
        assert 0 < moved < len(remaining), moved
        ctx.check('3 -> 4 shards converges across proxies', {'moved': moved, 'total': len(remaining)})
        ctx.k('patch', 'simplecache', 'products', '--type=merge', '-p', '{"spec":{"size":3}}')
        ctx.wait_for('scale down ready', lambda: ctx.condition(ctx.obj('simplecache', 'products'), 'Ready'))
        ctx.wait_for('scale down restores stable addresses', lambda: ctx.affinity(bases, remaining, before), 90)
        ctx.check('4 -> 3 shards removes backend and restores affinity', len(remaining))
        if ctx.extended:
            from .faults import exercise
            exercise(ctx, stack, bases, admins, remaining, [p['metadata']['uid'] for p in proxies])
        ctx.phase('final-validation')
        ctx.worker_metrics('final')
        for pod in ctx.obj('pods', selector='app=products')['items']:
            with ctx.forward('default', 'pod/' + pod['metadata']['name'], 8001) as member:
                assert ctx.request(member, 'item-1', path='/_geecache/products/item-1')[0] == 405
                assert ctx.request(member, 'item-1', path='/_simplecache/invalidate/products/item-1')[0] == 405
        ctx.check('local cache API and notification-only member transport', 'per-worker metrics captured')
    assert len(ctx.results['checks']) == (26 if ctx.extended else 10), 'scenario coverage incomplete'
    ctx.snapshot('success')
    ctx.results['status'] = 'passed'

