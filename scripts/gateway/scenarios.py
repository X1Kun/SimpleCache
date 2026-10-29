"""Additional behavioral experiments on the smoke runner's owned Kind cluster.

All SQL uses the run's PostgreSQL Pod; faults target only its resources and
restore their changes in finally blocks. No retries hide mutation outcomes.
"""
import concurrent.futures
import ipaddress
import json
import shlex
import secrets
import time
import urllib.request

CASE_SUFFIX = '-' + secrets.token_hex(4)


def case_key(name):
    return 'experiment-' + name + CASE_SUFFIX


PROXY_SELECTOR = ('gateway.envoyproxy.io/owning-gateway-name=products,'
                  'gateway.envoyproxy.io/owning-gateway-namespace=default')


def literal(value):
    return "'" + value.replace("'", "''") + "'"


def sql(m, statement):
    return m.k('exec', '-i', 'postgres', '--', 'psql', '-X', '-q', '-A', '-t', '-U', 'cache', '-d', 'products',
               '-v', 'ON_ERROR_STOP=1', data=statement).strip()


def source(m, key):
    value = sql(m, "SELECT json_build_object('version',version,'deleted',deleted,'value',convert_from(value,'UTF8')) "
                'FROM simplecache_products WHERE key=' + literal(key))
    result = json.loads(value)
    if not result['deleted']:
        result['value'] = json.loads(result['value'])
    return result


def seed(m, keys):
    statements = ['INSERT INTO simplecache_products(key,value) VALUES (' + literal(key) + ',convert_to(' +
                  literal(json.dumps({'key': key, 'value': 1})) + ",'UTF8'));" for key in keys]
    sql(m, '\n'.join(statements))


def external_update(m, key, value, expected=1):
    result = sql(m, 'UPDATE simplecache_products SET value=convert_to(' + literal(json.dumps(value)) +
                 ",'UTF8'),version=version+1,updated_at=now() WHERE NOT deleted AND key=" + literal(key) +
                 ' AND version=' + str(expected) + ' RETURNING version;')
    assert result == str(expected + 1), ('external CAS did not commit', result)
    return expected + 1


def require(m, base, key, version, value):
    status, headers, body = m.request(base, key)
    assert status == 200, (key, status, body)
    assert headers.get('etag') == '"v' + str(version) + '"', (key, headers)
    assert json.loads(body) == value, (key, body)
    return headers['x-simplecache-node']


def create(m, base, key, value):
    status, _, body = m.request(base, key, 'PUT', value, create=True)
    assert status == 200, (key, status, body)
    result = json.loads(body)
    assert result['source_version'] == result['invalidation']['version'] == 1 and result['invalidation']['complete'], result


def metric(text, name):
    return sum(float(line.rsplit(' ', 1)[1]) for line in text.splitlines()
               if line.startswith(name + ' ') or line.startswith(name + '{'))


def metrics(m, label):
    result = {}
    pods = [p for p in m.obj('pods', selector='app=products')['items'] if not p['metadata'].get('deletionTimestamp')]
    for pod in pods:
        with m.forward('default', 'pod/' + pod['metadata']['name'], 9999) as base:
            with urllib.request.urlopen(base + '/metrics', timeout=5) as response:
                text = response.read().decode()
        (m.REPORT / (label + '-' + pod['metadata']['name'] + '.prom')).write_text(text)
        assert metric(text, 'simplecache_peer_requests_total') == 0, 'normal reads forwarded to Peer'
        result[pod['metadata']['name']] = {
            'uid': pod['metadata']['uid'], 'process_start': metric(text, 'process_start_time_seconds'),
            'source': metric(text, 'simplecache_source_loads_total'),
            'members': metric(text, 'simplecache_members'),
            'floors': metric(text, 'simplecache_version_records'),
            'floor_capacity': metric(text, 'simplecache_version_record_capacity'),
            'floor_evictions': metric(text, 'simplecache_version_record_evictions_total'),
            'unknown_commits': metric(text, 'simplecache_source_unknown_commits_total'),
        }
    (m.REPORT / (label + '-identities.json')).write_text(json.dumps(result, indent=2))
    return result


def same_processes(before, after):
    assert set(before) == set(after), 'worker set changed during fixed-membership experiment'
    for name in before:
        assert (before[name]['uid'], before[name]['process_start']) == (after[name]['uid'], after[name]['process_start']), name


def gateway_entry(m, remaining):
    m.phase('shared-gateway-service')
    services = m.obj('services', namespace='envoy-gateway-system', selector=PROXY_SELECTOR)['items']
    service = next(s for s in services if any(p['port'] == 80 for p in s['spec']['ports']))
    address = str(ipaddress.IPv4Address(service['spec']['clusterIP']))
    # Use a real in-cluster client and the shared Service, in addition to the
    # independent proxy port-forwards. Every command creates a fresh HTTP flow.
    keys = remaining[:6]
    commands = ['set -eu']
    for key in keys:
        query = m.urllib.parse.urlencode({'key': key}, quote_via=m.urllib.parse.quote, safe='')
        commands += ['wget -qO- ' + shlex.quote('http://' + address + '/api?' + query), "printf '\\n'"]
    payloads = m.k('exec', '-i', 'postgres', '--', 'sh', '-s', data='\n'.join(commands))
    values = [json.loads(line) for line in payloads.splitlines() if line.strip()]
    assert values == [{'key': key} for key in keys], values
    m.check('in-cluster client reads through the shared Gateway Service', {'service': service['metadata']['name'], 'reads': len(keys)})


def guards(m, bases):
    m.phase('api-rejections')
    key = case_key('guards')
    create(m, bases[0], key, {'guard': 1})
    cases = [
        ('empty-key', '', 'GET', {}, 400),
        ('oversized-key', 'x' * 257, 'GET', {}, 400),
        ('unsupported-method', key, 'PATCH', {}, 405),
        ('unauthenticated-write', key, 'PUT', {'value': {'bad': True}, 'etag': '"v1"', 'auth': False}, 401),
        ('missing-write-condition', key, 'PUT', {'value': {'bad': True}}, 428),
        ('weak-etag', key, 'PUT', {'value': {}, 'etag': 'W/"v1"'}, 400),
        ('duplicate-create', key, 'PUT', {'value': {}, 'create': True}, 412),
        ('malformed-json', key, 'PUT', {'raw_body': '{', 'etag': '"v1"'}, 400),
        ('oversized-json', key, 'PUT', {'value': 'x' * (1024 * 1024 + 1), 'etag': '"v1"'}, 413),
        ('missing-delete-condition', key, 'DELETE', {}, 428),
        ('invalid-notification-version', key, 'POST', {'path': '/invalidate', 'event_version': '01'}, 400),
        ('fabricated-commit-version', key, 'PUT', {'value': {}, 'etag': '"v1"', 'event_version': 999}, 400),
        ('unauthenticated-notification', key, 'POST', {'path': '/invalidate', 'event_version': 1, 'auth': False}, 401),
    ]
    outcomes = {}
    for name, target, method, kwargs, expected in cases:
        status, _, body = m.request(bases[0], target, method, **kwargs)
        assert status == expected, (name, status, body)
        outcomes[name] = status
    assert source(m, key) == {'version': 1, 'deleted': False, 'value': {'guard': 1}}
    m.check('invalid API requests preserve the authoritative row', outcomes)


def concurrent_writes(m, bases):
    m.phase('concurrent-conditional-writes')
    for mode in ['create', 'replace', 'delete']:
        key = case_key('cas-' + mode)
        if mode != 'create':
            create(m, bases[0], key, {'initial': True})
        def operation(i):
            value = {'winner': i}
            if mode == 'delete':
                return i, m.request(bases[i % 2], key, 'DELETE', etag='"v1"')
            return i, m.request(bases[i % 2], key, 'PUT', value,
                                create=mode == 'create', etag='"v1"' if mode == 'replace' else None)
        with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
            results = list(pool.map(operation, range(12)))
        winners = [(i, response) for i, response in results if response[0] == 200]
        assert len(winners) == 1, [(i, r[0], r[2]) for i, r in results]
        assert sum(r[0] == 412 for _, r in results) == 11, [(i, r[0], r[2]) for i, r in results]
        expected_version = 1 if mode == 'create' else 2
        winner, response = winners[0]
        result = json.loads(response[2])
        assert result['source_version'] == result['invalidation']['version'] == expected_version
        truth = source(m, key)
        assert truth['version'] == expected_version and truth['deleted'] == (mode == 'delete'), truth
        if mode == 'delete':
            assert all(m.request(base, key)[0] == 404 for base in bases)
        else:
            assert truth['value'] == {'winner': winner}, truth
            for base in bases:
                require(m, base, key, expected_version, truth['value'])
        m.check('concurrent ' + mode + ' has one SQL winner', {'clients': 12, 'success': 1, 'conflicts': 11, 'version': expected_version})


def partial_delivery(m, bases):
    m.phase('partial-notification-and-explicit-retry')
    key = case_key('partial')
    create(m, bases[0], key, {'value': 1})
    pg = m.obj('pod', 'postgres')
    address = str(ipaddress.IPv4Address(pg['status']['podIP']))
    name = 'experiment-unreachable-peer'
    fault = {'apiVersion': 'discovery.k8s.io/v1', 'kind': 'EndpointSlice',
             'metadata': {'name': name, 'labels': {'kubernetes.io/service-name': 'products-svc',
                                                'endpointslice.kubernetes.io/managed-by': 'simplecache-experiment'}},
             'addressType': 'IPv4', 'ports': [{'name': 'peer', 'port': 8001, 'protocol': 'TCP'}],
             'endpoints': [{'addresses': [address], 'conditions': {'ready': True},
                            'targetRef': {'kind': 'Pod', 'name': 'postgres', 'namespace': 'default'}}]}
    m.apply(fault)
    try:
        m.wait_for('known unreachable member observed', lambda: all(v['members'] == 4 for v in metrics(m, 'fault-members').values()), 60)
        status, _, body = m.request(bases[0], key, 'PUT', {'value': 2}, etag='"v1"')
        reply = json.loads(body)
        assert status == 202 and reply['source_committed'] is True and reply['source_version'] == 2, (status, reply)
        assert any(r['node'] == 'default/postgres' and r['status'] in ['failed', 'timeout'] for r in reply['invalidation']['results']), reply
        assert source(m, key)['version'] == 2
        for base in bases:
            require(m, base, key, 2, {'value': 2})
        assert m.request(bases[1], key, 'PUT', {'wrong_retry': True}, etag='"v1"')[0] == 412
        m.check('unreachable known member returns 202 without rollback', {'injection': 'owned EndpointSlice advertises PostgreSQL Pod on closed Peer port 8001', 'version': 2})
    finally:
        m.k('delete', 'endpointslice', name, '--ignore-not-found')
    m.wait_for('normal membership restored', lambda: all(v['members'] == 3 for v in metrics(m, 'restored-members').values()), 60)
    status, _, body = m.request(bases[1], key, 'POST', path='/invalidate', event_version=2)
    assert status == 200 and json.loads(body)['invalidation']['complete'], body
    assert source(m, key)['version'] == 2, 'notification retry changed the database'
    m.check('explicit notification retry succeeds without repeating mutation', {'version': 2})


def floor_pressure(m, bases):
    m.phase('bounded-version-metadata')
    keys = [case_key('floor-' + str(i)) for i in range(1030)]
    seed(m, keys)
    def notify(item):
        i, key = item
        status, _, body = m.request(bases[i % 2], key, 'POST', path='/invalidate', event_version=1)
        assert status == 200 and json.loads(body)['invalidation']['complete'], (key, status, body)
        return status
    before = metrics(m, 'before-floor-pressure')
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        outcomes = list(pool.map(notify, enumerate(keys)))
    after = metrics(m, 'after-floor-pressure')
    same_processes(before, after)
    assert all(v['floors'] == v['floor_capacity'] == 1024 and v['floor_evictions'] > before[name]['floor_evictions']
               for name, v in after.items()), after
    assert sum(v['source'] for v in after.values()) == sum(v['source'] for v in before.values()), 'notification-only pressure read the database'
    m.check('version records stay bounded under 1030 committed-key notifications', {'notifications': len(outcomes), 'per_worker': after})


def set_short_ttl(m, admins):
    ttl = 20
    m.phase('short-ttl-fixture-rollout')
    m.k('patch', 'simplecache', 'products', '--type=merge', '-p', json.dumps({'spec': {'ttlSeconds': ttl}}))
    m.wait_for('TTL rollout ready', lambda: m.condition(m.obj('simplecache', 'products'), 'Ready'), 240)
    addresses = {m.obj('service', f'products-shard-{i}')['spec']['clusterIP'] for i in range(3)}
    m.wait_for('post-rollout shard health', lambda: m.proxies_healthy(admins, addresses), 90)
    m.check('short TTL fixture rolls out before expiry experiments', {'ttl_seconds': ttl})
    return ttl


def lost_notifications(m, bases, ttl):
    m.phase('lost-notification-ttl-recovery')
    key = case_key('missed-ttl')
    create(m, bases[0], key, {'value': 1})
    before = metrics(m, 'before-lost-event')
    owner = require(m, bases[0], key, 1, {'value': 1})
    warmed = time.monotonic()
    version = external_update(m, key, {'value': 2})  # Intentionally do not notify.
    truth = source(m, key)
    assert truth['version'] == version and truth['value'] == {'value': 2}
    status, headers, body = m.request(bases[1], key)
    assert (status, headers['etag'], json.loads(body)) == (200, '"v1"', {'value': 1}), 'old value was not actually retained'
    assert headers['x-simplecache-node'] == owner
    status, _, reply = m.request(bases[0], key, 'PUT', {'stale_overwrite': True}, etag='"v1"')
    assert status == 412 and json.loads(reply)['source_committed'] is False, (status, reply)
    stale_reads = 1
    deadline = warmed + ttl + 15
    while True:
        status, h, payload = m.request(bases[stale_reads % 2], key)
        assert status == 200 and h['x-simplecache-node'] == owner, (status, h, payload)
        if h.get('etag') == '"v2"':
            assert json.loads(payload) == {'value': 2}
            fresh_seen = time.monotonic()
            break
        assert h.get('etag') == '"v1"' and json.loads(payload) == {'value': 1}
        stale_reads += 1
        assert time.monotonic() < deadline, 'cache hits renewed TTL or expiry did not recover'
        time.sleep(0.5)
    after = metrics(m, 'after-ttl-recovery')
    same_processes(before, after)
    assert source(m, key)['value'] == {'value': 2}
    m.check('lost notification permits old reads but not old writes; TTL recovers',
            {'ttl_seconds': ttl, 'stale_reads_observed': stale_reads, 'fresh_observed_seconds_from_warm_response': fresh_seen-warmed,
             'unchanged_worker_processes': True, 'overwrite_status': 412})

    m.phase('lost-notification-explicit-recovery')
    key = case_key('missed-explicit')
    create(m, bases[0], key, {'value': 1})
    require(m, bases[0], key, 1, {'value': 1})
    warmed = time.monotonic()
    external_update(m, key, {'value': 2})
    require(m, bases[1], key, 1, {'value': 1})
    status, _, reply = m.request(bases[1], key, 'POST', path='/invalidate', event_version=2)
    assert status == 200 and json.loads(reply)['invalidation']['complete'], reply
    require(m, bases[0], key, 2, {'value': 2})
    assert time.monotonic()-warmed < ttl, 'recovery happened only after expiry'
    m.check('explicit version notification repairs a missed event before TTL', {'elapsed_seconds': time.monotonic()-warmed})


def source_outage(m, bases, ttl):
    m.phase('database-connectivity-loss')
    hit, cold, write = case_key('db-hit'), case_key('db-cold'), case_key('db-write')
    seed(m, [hit, cold, write])
    require(m, bases[0], write, 1, {'key': write, 'value': 1})
    require(m, bases[0], hit, 1, {'key': hit, 'value': 1})
    warmed = time.monotonic()
    original = m.obj('service', 'postgres')['spec']['selector']
    m.k('patch', 'service', 'postgres', '--type=merge', '-p', json.dumps({'spec': {'selector': {'app': 'experiment-no-database-endpoint'}}}))
    try:
        def no_endpoints():
            slices = m.obj('endpointslices', selector='kubernetes.io/service-name=postgres')['items']
            return not any(e.get('addresses') for s in slices for e in (s.get('endpoints') or []))
        m.wait_for('database Service disconnected', no_endpoints, 30)
        sql(m, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='products' AND usename='cache' AND pid<>pg_backend_pid();")
        require(m, bases[0], hit, 1, {'key': hit, 'value': 1})
        assert time.monotonic()-warmed < ttl, 'warm hit already expired before outage check'
        status, _, body = m.request(bases[1], cold)
        assert status in [503, 504], (status, body)
        write_status, _, reply = m.request(bases[0], write, 'PUT', {'unsafe': True}, etag='"v1"')
        outcome = json.loads(reply)
        assert write_status in [503, 504] and outcome['source_committed'] is None and 'source_version' not in outcome, (write_status, reply)
        assert source(m, write) == {'version': 1, 'deleted': False, 'value': {'key': write, 'value': 1}}
        time.sleep(max(0, warmed + ttl + 1 - time.monotonic()))
        expired_status, _, body = m.request(bases[1], hit)
        assert expired_status in [503, 504], 'expired data was served during database loss: ' + body
        m.check('database loss preserves unexpired hits and rejects cold/expired reads',
                {'warm_hit': 200, 'cold': status, 'expired': expired_status, 'source_committed_on_failed_write': None,
                 'injection': 'owned DB Service selector changed and existing app sessions terminated; PG process/data retained'})
    finally:
        m.k('patch', 'service', 'postgres', '--type=merge', '-p', json.dumps({'spec': {'selector': original}}))
    m.wait_for('database source recovered', lambda: require(m, bases[0], cold, 1, {'key': cold, 'value': 1}), 60)
    require(m, bases[1], hit, 1, {'key': hit, 'value': 1})
    require(m, bases[0], write, 1, {'key': write, 'value': 1})
    m.check('database connectivity recovery reloads correct values without hidden writes', {'version': source(m, write)['version']})


def cold_restart(m, bases, admins):
    m.phase('restart-after-missed-notification')
    key = case_key('cold-restart')
    create(m, bases[0], key, {'value': 1})
    owner = require(m, bases[0], key, 1, {'value': 1}).split('/')[-1]
    external_update(m, key, {'value': 2})
    require(m, bases[1], key, 1, {'value': 1})
    old = m.obj('pod', owner)['metadata']['uid']
    ordinal = owner.rsplit('-', 1)[-1]
    address = m.obj('service', 'products-shard-' + ordinal)['spec']['clusterIP']
    m.k('delete', 'pod', owner, '--wait=false')
    def replacement():
        pod = m.obj('pod', owner)
        return pod if pod['metadata']['uid'] != old and any(c['type'] == 'Ready' and c['status'] == 'True' for c in pod.get('status', {}).get('conditions', [])) else None
    pod = m.wait_for('cold replacement ready', replacement, 180)
    before = metrics(m, 'before-cold-recovery')
    assert before[owner]['uid'] == pod['metadata']['uid'] and before[owner]['floors'] == 0 and before[owner]['source'] == 0, before[owner]
    addresses = {m.obj('service', f'products-shard-{i}')['spec']['clusterIP'] for i in range(3)}
    m.wait_for('cold worker health converged', lambda: m.proxies_healthy(admins, addresses), 90)
    m.wait_for('cold worker reads primary generation', lambda: require(m, bases[0], key, 2, {'value': 2}) == 'default/' + owner, 60)
    after = metrics(m, 'after-cold-recovery')
    assert after[owner]['source'] >= 1 and after[owner]['floors'] == 0
    assert m.obj('service', 'products-shard-' + ordinal)['spec']['clusterIP'] == address
    m.check('restart loses cache/floors and reloads the primary after a missed event',
            {'worker': owner, 'old_uid': old, 'new_uid': pod['metadata']['uid'], 'initial_floors': 0, 'initial_source_loads': 0,
             'fresh_version': 2, 'stable_service_ip': address})


def repair_policy(m, bases, remaining):
    m.phase('operator-child-repair')
    policy = m.obj('backendtrafficpolicy', 'products')
    original = policy['metadata']['uid']
    m.k('delete', 'backendtrafficpolicy', 'products', '--wait=false')
    def repaired():
        p = m.obj('backendtrafficpolicy', 'products')
        return p if p['metadata']['uid'] != original and p['spec']['loadBalancer']['type'] == 'ConsistentHash' else None
    replacement = m.wait_for('owned hash policy recreated', repaired, 90)
    cr = m.obj('simplecache', 'products')
    assert any(r.get('controller') and r['uid'] == cr['metadata']['uid'] for r in replacement['metadata']['ownerReferences'])
    def accepted():
        p = m.obj('backendtrafficpolicy', 'products')
        return any(c['type'] == 'Accepted' and c['status'] == 'True' and
                   c.get('observedGeneration') == p['metadata']['generation']
                   for ancestor in p.get('status', {}).get('ancestors', []) for c in ancestor.get('conditions', []))
    m.wait_for('recreated policy accepted by Envoy Gateway', accepted, 90)
    m.wait_for('repaired route accepted', lambda: m.condition(m.obj('simplecache', 'products'), 'Ready'), 90)
    m.wait_for('repaired policy routes consistently', lambda: m.affinity(bases, remaining, shards=3), 90)
    m.check('Operator watch recreates owned Gateway policy and restores routing', {'old_uid': original, 'new_uid': replacement['metadata']['uid']})


def proxy_restart(m, stack, bases, admins, remaining, original_proxy_uids):
    m.phase('gateway-proxy-restart')
    pods = [p for p in m.obj('pods', namespace='envoy-gateway-system', selector=PROXY_SELECTOR)['items'] if not p['metadata'].get('deletionTimestamp')]
    assert len(pods) == 2
    by_uid = {p['metadata']['uid']: p for p in pods}
    assert set(original_proxy_uids) == set(by_uid), 'proxy set changed unexpectedly before restart experiment'
    victim = by_uid[original_proxy_uids[0]]
    victim_index = 0
    survivor = bases[1]
    old_uids = {p['metadata']['uid'] for p in pods}
    m.k('-n', 'envoy-gateway-system', 'delete', 'pod', victim['metadata']['name'], '--wait=false')
    for key in remaining[:12]:
        require(m, survivor, key, 1, {'key': key})
    def replaced():
        ready = [p for p in m.obj('pods', namespace='envoy-gateway-system', selector=PROXY_SELECTOR)['items']
                 if not p['metadata'].get('deletionTimestamp') and p['metadata']['uid'] not in old_uids and
                 any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', []))]
        return ready[0] if ready else None
    new = m.wait_for('proxy replica replaced', replaced, 180)
    bases[victim_index] = stack.enter_context(m.forward('envoy-gateway-system', 'pod/' + new['metadata']['name'], 10080))
    admins[victim_index] = stack.enter_context(m.forward('envoy-gateway-system', 'pod/' + new['metadata']['name'], 19000))
    addresses = {m.obj('service', f'products-shard-{i}')['spec']['clusterIP'] for i in range(3)}
    m.wait_for('new proxy health converged', lambda: m.proxies_healthy(admins, addresses), 90)
    m.wait_for('replacement proxy agrees', lambda: m.affinity(bases, remaining, shards=3), 90)
    m.check('surviving proxy serves reads while its peer restarts', {'survivor_reads': 12, 'old_uid': victim['metadata']['uid'], 'new_uid': new['metadata']['uid']})


def exercise(m, stack, bases, admins, remaining, original_proxy_uids):
    gateway_entry(m, remaining)
    guards(m, bases)
    concurrent_writes(m, bases)
    partial_delivery(m, bases)
    floor_pressure(m, bases)
    ttl = set_short_ttl(m, admins)
    lost_notifications(m, bases, ttl)
    source_outage(m, bases, ttl)
    cold_restart(m, bases, admins)
    repair_policy(m, bases, remaining)
    proxy_restart(m, stack, bases, admins, remaining, original_proxy_uids)


def continue_exercise(m, stack, bases, admins, remaining, original_proxy_uids, failed_phase):
    # Only support resuming a fully identified, retained application fixture.
    # Each experiment uses a new key suffix, while already passed evidence is
    # kept with its original report identity rather than rerun or relabelled.
    stages = [
        ('database-connectivity-loss', lambda: source_outage(m, bases, 20)),
        ('restart-after-missed-notification', lambda: cold_restart(m, bases, admins)),
        ('operator-child-repair', lambda: repair_policy(m, bases, remaining)),
        ('gateway-proxy-restart', lambda: proxy_restart(m, stack, bases, admins, remaining, original_proxy_uids)),
    ]
    names = [name for name, _ in stages]
    assert failed_phase in names, 'unsupported continuation phase: ' + failed_phase
    for _, operation in stages[names.index(failed_phase):]:
        operation()
