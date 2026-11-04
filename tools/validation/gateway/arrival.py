"""Open-loop reads through an owned Kind Gateway, with separate client Pod.

Errors and client saturation remain measurements. Incorrect data, incomplete
artifacts, process replacement and missing instrumentation fail qualification.
"""
import collections
import contextlib
import hashlib
import json
import subprocess
import threading
import time
import urllib.request

from .helpers import metric, sql
from .performance import reset_workers, pressure

FIELDS = [
    'process_cpu_seconds_total', 'process_resident_memory_bytes',
    'process_start_time_seconds', 'go_memstats_heap_alloc_bytes',
    'go_memstats_alloc_bytes_total', 'go_goroutines', 'go_gc_duration_seconds_count',
    'simplecache_source_loads_total',
    'simplecache_source_pending', 'simplecache_source_inflight',
    'simplecache_cache_bytes', 'simplecache_cache_removals_total',
    'simplecache_admission_rejections_total', 'simplecache_api_miss_waiters',
    'simplecache_outbox_pending_events', 'simplecache_outbox_oldest_pending_seconds',
    'simplecache_outbox_purge_eligible_events', 'simplecache_outbox_table_bytes',
]


def schedule():
    return [dict(name=name, rate=rate, seconds=seconds) for name, rate, seconds in [
        ('baseline', 100, 10), ('load-300', 300, 10), ('load-800', 800, 10),
        ('load-1500', 1500, 10), ('recovery', 100, 15)]]


def distributions(settings):
    values = settings.get('GATEWAY_ARRIVAL_DISTRIBUTIONS', 'hot,scan,hot-scan,hit-miss').split(',')
    if not values or len(values) != len(set(values)) or any(v not in ['hot', 'scan', 'hot-scan', 'hit-miss'] for v in values):
        raise ValueError('invalid GATEWAY_ARRIVAL_DISTRIBUTIONS')
    return values


def validate_evidence(summary, records):
    """Never let a partial/client-limited execution become a clean result."""
    assert summary['planned'] == len(records)
    assert summary['attempted'] + summary['skipped'] == summary['planned']
    assert summary['attempted'] == sum(r['attempted'] for r in records)
    assert summary['incorrect'] == sum(r['incorrect'] for r in records) == 0
    assert summary['errors'] == sum(bool(r.get('error')) for r in records)
    assert summary['success'] == sum(r['attempted'] and not r.get('error') and not r['incorrect'] for r in records)
    assert all(r['method'] == 'GET' for r in records)
    assert sorted(r['index'] for r in records) == list(range(len(records)))
    counts = summary['arrival'].get('counts', {})
    if 'dispatched' in counts:
        assert counts['dispatched'] == sum(bool(r.get('dispatched')) for r in records)
        assert summary['attempted'] <= counts['dispatched'] <= summary['planned']
    status = summary['arrival']['status']
    assert status in ['passed', 'client-limited', 'completed-with-errors']
    if summary['skipped']:
        assert status == 'client-limited'
    elif summary['errors']:
        assert status == 'completed-with-errors'
    else:
        assert status == 'passed'
    return status


def execute(m, config, name):
    path = m.report / (name + '-config.json')
    path.write_text(json.dumps(config))
    m.k('cp', str(path), 'gateway-bench:/tmp/config.json')
    code = 0
    try:
        m.k('exec', 'gateway-bench', '--', '/tmp/gateway-bench', '-config', '/tmp/config.json',
            '-out', '/tmp/' + name, timeout=180)
    except RuntimeError:
        code = 1
    m.k('cp', 'gateway-bench:/tmp/' + name, str(m.report / name))
    summary = json.loads((m.report / name / 'summary.json').read_text())
    records = [json.loads(line) for line in (m.report / name / 'requests.jsonl').read_text().splitlines()]
    status = validate_evidence(summary, records)
    assert bool(code) == (status != 'passed'), 'exit status differs from saved outcome'
    return summary, records


def sample(m, bases, path, stop, active_uids):
    """Own subprocess outputs; never share the orchestrator's seekable log."""
    start = time.monotonic()
    context = ['kubectl', '--context', 'kind-' + m.cluster]
    with path.open('w') as log:
        while not stop.is_set():
            record = {'elapsed_seconds': time.monotonic() - start, 'unix': time.time(), 'cache': {}, 'errors': []}
            for name, base in bases.items():
                try:
                    with urllib.request.urlopen(base + '/metrics', timeout=2) as response:
                        text = response.read().decode()
                    record['cache'][name] = {field: metric(text, field) for field in FIELDS}
                except Exception as exc:
                    record['errors'].append(name + ': ' + type(exc).__name__)
            for label, command in [
                ('kubelet', context + ['get', '--raw', '/api/v1/nodes/' + m.cluster + '-control-plane/proxy/stats/summary']),
                ('postgres', context + ['-n', 'default', 'exec', 'postgres', '--', 'psql', '-X', '-A', '-t', '-U', 'cache', '-d', 'products', '-c',
                 "SELECT json_build_object('connections',(SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()),'active',(SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active'),'waiting_locks',(SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock'),'database_bytes',pg_database_size(current_database()))"]),
            ]:
                try:
                    raw = subprocess.check_output(command, text=True, env=m.env, stderr=subprocess.PIPE, timeout=5)
                    value = json.loads(raw)
                    if label == 'kubelet':
                        value = {'node': value.get('node'), 'pods': [p for p in value.get('pods', []) if
                            p['podRef'].get('uid') in active_uids]}
                    record[label] = value
                except Exception as exc:
                    record['errors'].append(label + ': ' + type(exc).__name__)
            record['sample_finished_unix'] = time.time()
            log.write(json.dumps(record) + '\n'); log.flush()
            stop.wait(2)


def summarize_samples(path):
    samples = [json.loads(line) for line in path.read_text().splitlines()]
    assert len(samples) >= 3, 'insufficient time-series samples'
    errors = [e for s in samples for e in s['errors']]
    starts = collections.defaultdict(set)
    for s in samples:
        assert len(s['cache']) == 3
        for name, values in s['cache'].items():
            starts[name].add(values['process_start_time_seconds'])
    assert all(len(v) == 1 and next(iter(v)) > 0 for v in starts.values()), 'cache process changed during measurement'
    return {'samples': len(samples), 'errors': errors, 'status': 'incomplete' if errors else 'complete'}


def resource_summary(path, before, after):
    rows = [json.loads(line) for line in path.read_text().splitlines()]
    peak = collections.defaultdict(float)
    for row in rows:
        peak['cache_rss_bytes'] = max(peak['cache_rss_bytes'], sum(v['process_resident_memory_bytes'] for v in row['cache'].values()))
        peak['source_pending'] = max(peak['source_pending'], sum(v['simplecache_source_pending'] for v in row['cache'].values()))
        peak['source_inflight'] = max(peak['source_inflight'], sum(v['simplecache_source_inflight'] for v in row['cache'].values()))
        memory = collections.defaultdict(float)
        for pod in row.get('kubelet', {}).get('pods', []):
            name = pod['podRef']['name']
            group = 'proxy' if name.startswith('envoy-') else ('cache' if name.startswith('products-') else name)
            memory[group] += (pod.get('memory') or {}).get('workingSetBytes', 0)
        for group, value in memory.items():
            peak[group + '_working_set_bytes'] = max(peak[group + '_working_set_bytes'], value)
    counters = {}
    for name, old in before.items():
        new = after.get(name, {})
        if 'unavailable' in old or 'unavailable' in new:
            counters[name] = {'status': 'unavailable'}
            continue
        difference = {k: new[k] - v for k, v in old.items() if k in new}
        assert all(v >= 0 for v in difference.values()), 'container counters reset during trial'
        counters[name] = difference
    return {'sampled_maxima': dict(peak), 'container_counter_deltas': counters,
            'scope': 'Sampled/cache-delayed observations, not continuous peaks; cgroup deltas cover the trial envelope including orchestration'}


def container_counters(m, names):
    result = {}
    for name in names:
        try:
            raw = m.k('exec', name, '--', 'sh', '-c', 'cat /sys/fs/cgroup/cpu.stat /sys/fs/cgroup/memory.events')
            result[name] = {k: int(v) for k, v in (line.split() for line in raw.splitlines())}
        except (RuntimeError, ValueError) as exc:
            result[name] = {'unavailable': str(exc)}
    return result


def measure(m, proxies):
    m.phase('arrival-setup')
    m.k('patch', 'simplecache', 'products', '--type=merge', '-p', json.dumps({'spec': {'resources': {'requests': {'cpu': '100m', 'memory': '128Mi'}, 'limits': {'cpu': '500m', 'memory': '256Mi'}}}}))
    m.wait_for('cache CPU/memory budgets rolled out', lambda: m.condition(m.obj('simplecache', 'products'), 'Ready'), 240)
    environment = {**m.env, 'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOFLAGS': '-buildvcs=false'}
    binary = m.report / 'gateway-bench'
    with (m.report / 'arrival-build.log').open('w') as log:
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/gateway-bench'], cwd=m.root/'geecache-engine',
                       env=environment, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=180)
    services = m.obj('services', namespace='envoy-gateway-system', selector='gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default')['items']
    service = next(s for s in services if any(p['port'] == 80 for p in s['spec']['ports']))
    m.apply({'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'gateway-bench', 'labels': {'app': 'gateway-bench'}}, 'spec': {
        'securityContext': {'runAsUser': 65532, 'runAsGroup': 65532},
        'containers': [{'name': 'client', 'image': 'simplecache-engine:gateway', 'command': ['/bin/sh', '-c', 'sleep 86400'],
                        'env': [{'name': 'GOMAXPROCS', 'value': '2'}],
                        'resources': {'requests': {'cpu': '500m', 'memory': '128Mi'}, 'limits': {'cpu': '2', 'memory': '256Mi'}}}]}})
    m.k('wait', '--for=condition=Ready', 'pod/gateway-bench', '--timeout=120s')
    m.k('cp', str(binary), 'gateway-bench:/tmp/gateway-bench')
    manifest = {'scope': 'One-host Kind, separate client/cache/PostgreSQL Pods, real Envoy Gateway and production binaries. Memory-backed PostgreSQL; not disk durability or production SLO.',
                'scheduler': {'max_schedule_lag_ms': 5, 'max_catch_up': 8}, 'distributions': distributions(m.env), 'stages': schedule(), 'client_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
                'cache_cpu_limit_per_pod': '500m', 'cache_memory_limit_per_pod': '256Mi', 'cache_bytes_per_pod': 1048576, 'cache_pods': 3, 'keys': 2048, 'value_bytes': 4096,
                'host_before': pressure(), 'trials': []}
    (m.report / 'arrival.json').write_text(json.dumps(manifest, indent=2))
    for distribution in distributions(m.env):
        name = 'arrival-' + distribution
        m.phase(name)
        config = {'endpoint': 'http://' + service['spec']['clusterIP'], 'host': 'arrival.local',
                  'prefix': name, 'scenario': 'arrival', 'distribution': distribution, 'keys': 2048,
                  'value_bytes': 4096, 'reads': 1, 'workers': 64, 'max_pending': 128, 'seed': 42, 'max_schedule_lag_ms': 5, 'max_catch_up': 8, 'stages': schedule()}
        local = m.report / (name + '-config.json'); local.write_text(json.dumps(config))
        fixture = m.report / (name + '-fixture.sql')
        subprocess.run([str(binary), '-config', str(local), '-fixture', str(fixture)], env=m.env, check=True, timeout=60)
        assert sql(m, 'SELECT count(*) FROM simplecache_outbox WHERE delivered_at IS NULL') == '0'
        sql(m, fixture.read_text())
        reset_workers(m, 300)
        workers = [p for p in m.obj('pods', selector='app=products')['items'] if not p['metadata'].get('deletionTimestamp')]
        identities = {p['metadata']['name']: p['metadata']['uid'] for p in workers}
        with contextlib.ExitStack() as stack:
            bases = {p['metadata']['name']: stack.enter_context(m.forward('default', 'pod/' + p['metadata']['name'], 9999)) for p in workers}
            # Qualification is outside timing. Hot traffic warms exactly its one key.
            admins = [stack.enter_context(m.forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 19000)) for p in proxies]
            addresses = {m.obj('service', 'products-shard-' + str(i))['spec']['clusterIP'] for i in range(3)}
            m.wait_for('all arrival backends healthy on both proxies', lambda: m.proxies_healthy(admins, addresses), 90)
            owners = set()
            for p in proxies:
                base = stack.enter_context(m.forward('envoy-gateway-system', 'pod/' + p['metadata']['name'], 10080))
                code, headers, _ = m.request(base, config['prefix'] + '-0000')
                assert code == 200
                owners.add(headers['x-simplecache-node'])
            assert len(owners) == 1, 'proxies disagree on warm-key ownership'
            if distribution == 'hit-miss':
                assert sql(m, "SELECT count(*) FROM simplecache_products WHERE key LIKE 'arrival-hit-miss-missing-%'") == '0'
            (m.report / (name + '-pods.json')).write_text(json.dumps(workers, indent=2))
            host_before = pressure()
            cgroups_before = container_counters(m, [*bases, 'gateway-bench'])
            path = m.report / (name + '-timeline.jsonl')
            active_pods = m.obj('pods')['items'] + m.obj('pods', namespace='envoy-gateway-system', selector='gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default')['items']
            active_uids = {p['metadata']['uid'] for p in active_pods if not p['metadata'].get('deletionTimestamp') and (p['metadata']['name'] in [*bases, 'postgres', 'gateway-bench'] or p['metadata']['name'].startswith('envoy-default-products-'))}
            stop = threading.Event(); sampler = threading.Thread(target=sample, args=(m, bases, path, stop, active_uids))
            sampler.start()
            try:
                summary, records = execute(m, config, name)
            finally:
                stop.set(); sampler.join(timeout=30)
            assert not sampler.is_alive(), 'sampler did not stop'
        assert identities == {p['metadata']['name']: p['metadata']['uid'] for p in m.obj('pods', selector='app=products')['items']}, 'worker replacement invalidates trial'
        cgroups_after = container_counters(m, [*identities, 'gateway-bench'])
        instrumentation = summarize_samples(path)
        observed_uids = {p['podRef'].get('uid') for line in path.read_text().splitlines() for p in json.loads(line).get('kubelet', {}).get('pods', [])}
        assert active_uids <= observed_uids, 'Kubelet never observed all measured Pods'
        instrumentation['observed_pods'] = len(active_uids)
        assert instrumentation['status'] == 'complete', instrumentation
        cohorts = collections.defaultdict(collections.Counter)
        for r in records:
            if r['attempted'] and not r.get('error') and not r['incorrect']:
                cohorts[r['cohort']][r['key_index']] += 1
        revisits = {c: {'unique_keys': len(v), 'repeated_reads': sum(v.values()) - len(v)} for c, v in cohorts.items()}
        if distribution in ['scan', 'hot-scan']:
            assert revisits['scan']['unique_keys'] * (4096 + len(config['prefix']) + 5) > 3 * 1048576, 'scan did not exceed aggregate capacity'
            assert revisits['scan']['repeated_reads'] > 0, 'scan never revisited a key'
        resource_data = resource_summary(path, cgroups_before, cgroups_after)
        trial = {'resources': resource_data, 'name': name, 'config': config, 'summary': summary, 'instrumentation': instrumentation,
                 'cgroup_before': cgroups_before, 'cgroup_after': cgroups_after, 'revisits': revisits, 'worker_uids': identities, 'host_before': host_before, 'host_after': pressure()}
        manifest['trials'].append(trial)
        (m.report/'arrival.json').write_text(json.dumps(manifest, indent=2))
        m.check('captured ' + name, {'status': summary['arrival']['status'], 'planned': summary['planned'],
                                   'errors': summary['errors'], 'dropped': summary['skipped'], 'revisits': revisits})
    manifest['host_after'] = pressure()
    statuses = [t['summary']['arrival']['status'] for t in manifest['trials']]
    manifest['status'] = 'client-limited' if 'client-limited' in statuses else ('completed-with-errors' if 'completed-with-errors' in statuses else 'passed')
    (m.report/'arrival.json').write_text(json.dumps(manifest, indent=2))
    m.results['status'] = manifest['status']; m.results['arrival_report'] = 'arrival.json'
    lines = ['# Kind arrival experiments', '', manifest['scope'], '', 'Outcome: **' + manifest['status'] + '**.', '',
             '| Behaviour | Stage | Offered/s | Attempted | Errors | Client drops | HTTP P95 ms | Total P95 ms |',
             '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |']
    for t in manifest['trials']:
        for stage in t['summary']['arrival']['stages']:
            c = stage['counts']
            lines.append(f"| {t['config']['distribution']} | {stage['name']} | {stage['rate']} | {c['attempted']} | {c['errors']} | {c['dropped']} | {c['request_latency']['p95_ms']:.3f} | {c['schedule_to_completion']['p95_ms']:.3f} |")
    lines += ['', '| Behaviour | Cache RSS max MiB | Client working set max MiB | DB working set max MiB | Source pending max |', '| --- | ---: | ---: | ---: | ---: |']
    for t in manifest['trials']:
        r = t['resources']['sampled_maxima']
        lines.append(f"| {t['config']['distribution']} | {r.get('cache_rss_bytes', 0)/1048576:.1f} | {r.get('gateway-bench_working_set_bytes', 0)/1048576:.1f} | {r.get('postgres_working_set_bytes', 0)/1048576:.1f} | {r.get('source_pending', 0):.0f} |")
    lines += ['', 'Client-limited stages do not establish server capacity. All errors/drops remain in the raw evidence. Stage attribution uses planned arrival time; check queue latency for spillover. Timelines contain separate Pod Kubelet observations, per-cache metrics and PostgreSQL connection/lock samples. Sampling is not a continuous peak.', '', '[Raw manifest](arrival.json). Individual client reports are in arrival-*/report.md.']
    (m.report/'arrival.md').write_text('\n'.join(lines)+'\n')
