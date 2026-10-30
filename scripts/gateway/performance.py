"""Paired Gateway routing experiments. Client traffic stays inside the cluster.

Single-host Kind numbers are exploratory; raw errors, source deltas and sampled
resource envelopes are retained. No percentile subtraction estimates phase costs.
"""
import base64
import collections
import contextlib
import hashlib
import json
import os
import pathlib
import shutil
import statistics
import subprocess
import threading
import time
import urllib.request

from scenarios import literal, metric, source, sql

MODES = {'random': 'perf-random.local', 'hash': 'perf-hash.local'}
SCENARIOS = ['warm-hot', 'cold-key', 'capacity', 'zipf', 'mixed', 'ttl']


def continue_retained(m, previous_name):
    """Continue only an owned failed performance fixture; keep its evidence."""
    previous = pathlib.Path(previous_name).resolve()
    assert previous.is_relative_to((m.ROOT / 'bin/reports').resolve())
    old = json.loads((previous / 'result.json').read_text())
    measurements = json.loads((previous / 'performance.json').read_text())
    assert old.get('suite') == 'performance' and old.get('status') == 'failed'
    assert old.get('cleanup', {}).get('status') == 'retained-for-debug'
    assert measurements.get('status') == 'failed' and measurements.get('failure')
    m.CLUSTER = old['cluster']
    assert m.CLUSTER.startswith('simplecache-gateway-')
    labels = json.loads(m.run(['docker', 'inspect', m.CLUSTER + '-control-plane', '--format={{json .Config.Labels}}']))
    assert labels.get('io.x-k8s.kind.cluster') == m.CLUSTER and labels.get('io.x-k8s.kind.role') == 'control-plane'
    m.run(['kind', 'export', 'kubeconfig', '--name', m.CLUSTER, '--kubeconfig', m.ENV['KUBECONFIG']])
    crs = json.loads((previous / 'failure-simplecaches.json').read_text())['items']
    old_cr = next(c for c in crs if c['metadata']['name'] == 'products' and c['metadata']['namespace'] == 'default')
    current = m.obj('simplecache', 'products')
    assert current['metadata']['uid'] == old_cr['metadata']['uid'], 'cache fixture changed'
    pods = json.loads((previous / 'failure-pods.json').read_text())['items']
    db = next(p for p in pods if p['metadata']['name'] == 'postgres' and p['metadata']['namespace'] == 'default')
    assert m.obj('pod', 'postgres')['metadata']['uid'] == db['metadata']['uid'], 'database fixture changed'
    assert current['spec']['size'] == 3 and current['spec']['cacheBytes'] == 1048576
    assert current['spec']['ttlSeconds'] in [5, 300]
    assert m.obj('service', 'postgres')['spec']['selector'] == {'app': 'postgres'}
    # Secret contents never pass through the command logger.
    raw = subprocess.check_output(['kubectl', '--context', 'kind-' + m.CLUSTER, '-n', 'default',
                                   'get', 'secret', 'products', '-o', 'json'], env=m.ENV, text=True, timeout=30)
    m.TOKEN = base64.b64decode(json.loads(raw)['data']['CACHE_ADMIN_TOKEN']).decode()
    m.PROXY_LOCAL = m.CLUSTER + ':envoy'
    m.run(['docker', 'image', 'inspect', '--format={{.Id}}', m.PROXY_LOCAL])
    m.CREATED = True
    m.LOCAL_PROXY_TAG_CREATED = True
    m.RESULTS.update({'cluster': m.CLUSTER, 'continued_from': str(previous), 'images': old['images'],
                     'postgres_storage': old.get('postgres_storage'),
                     'checks': [dict(c, run_report=c.get('run_report', str(previous))) for c in old['checks']]})
    proxies = [p for p in m.obj('pods', namespace='envoy-gateway-system',
               selector='gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default')['items']
               if not p['metadata'].get('deletionTimestamp')]
    assert len(proxies) == 2 and all(any(c['type'] == 'Ready' and c['status'] == 'True'
               for c in p.get('status', {}).get('conditions', [])) for p in proxies)
    measure(m, proxies, previous)
    m.snapshot('success')
    m.RESULTS['status'] = m.RESULTS['performance_status']


def pressure():
    def read(path):
        try:
            return pathlib.Path(path).read_text().strip()
        except OSError:
            return 'unavailable'
    return {'io': read('/proc/pressure/io'), 'cpu': read('/proc/pressure/cpu'), 'loadavg': read('/proc/loadavg')}


def value(key, version, size):
    base = {'key': key, 'version': version, 'padding': ''}
    empty = json.dumps(base, separators=(',', ':'))
    pad = ''
    index = 0
    while len(pad) < size-len(empty):
        pad += hashlib.sha256(f'{key}:{version}:{index}'.encode()).hexdigest()
        index += 1
    base['padding'] = pad[:max(0, size-len(empty))]
    result = json.dumps(base, separators=(',', ':'))
    assert len(result.encode()) == size
    return result


def resource_sampler(m, path, stop):
    node = m.CLUSTER + '-control-plane'
    command = ['kubectl', '--context', 'kind-' + m.CLUSTER, 'get', '--raw', '/api/v1/nodes/' + node + '/proxy/stats/summary']
    with path.open('w') as log:
        while not stop.is_set():
            captured = time.time()
            try:
                raw = subprocess.check_output(command, env=m.ENV, text=True, stderr=subprocess.PIPE, timeout=10)
                summary = json.loads(raw)
                pods = []
                for pod in summary.get('pods', []):
                    ref = pod['podRef']
                    name = ref['name']
                    if name.startswith('products-') or name.startswith('envoy-default-products-') or name in ['postgres', 'gateway-bench']:
                        pods.append({k: pod.get(k) for k in ['podRef', 'startTime', 'cpu', 'memory', 'containers']})
                log.write(json.dumps({'captured_unix': captured, 'pods': pods})+'\n')
            except Exception as exc:
                log.write(json.dumps({'captured_unix': captured, 'error': type(exc).__name__})+'\n')
            log.flush()
            stop.wait(2)


def resources(path):
    maxima = collections.defaultdict(float)
    samples = 0
    for line in path.read_text().splitlines():
        sample = json.loads(line)
        if 'pods' not in sample:
            continue
        samples += 1
        sums = collections.defaultdict(float)
        for pod in sample['pods']:
            name = pod['podRef']['name']
            group = 'cache' if name.startswith('products-') else ('envoy' if name.startswith('envoy-default-products-') else name)
            sums[group] += (pod.get('memory') or {}).get('workingSetBytes', 0)
        for group, total in sums.items():
            maxima[group] = max(maxima[group], total)
    return {'successful_samples': samples, 'sampled_max_working_set_bytes': dict(maxima),
            'scope': 'Kubelet cached observations during stage envelope, not a continuous RSS peak'}


def cache_metrics(m, label):
    result = {}
    names = ['simplecache_source_loads_total', 'simplecache_peer_requests_total', 'process_cpu_seconds_total',
             'process_resident_memory_bytes', 'go_memstats_alloc_bytes_total', 'process_start_time_seconds',
             'simplecache_source_write_duration_seconds_count', 'simplecache_source_write_duration_seconds_sum',
             'simplecache_invalidation_broadcast_duration_seconds_count', 'simplecache_invalidation_broadcast_duration_seconds_sum',
             'simplecache_version_rejections_total']
    pods = [p for p in m.obj('pods', selector='app=products')['items'] if not p['metadata'].get('deletionTimestamp')]
    for pod in pods:
        with m.forward('default', 'pod/' + pod['metadata']['name'], 9999) as base:
            with urllib.request.urlopen(base+'/metrics', timeout=5) as response:
                text = response.read().decode()
        (m.REPORT / (label+'-'+pod['metadata']['name']+'.prom')).write_text(text)
        values = {name: metric(text, name) for name in names}
        assert values['simplecache_peer_requests_total'] == 0, 'comparison accidentally used Peer forwarding'
        result[pod['metadata']['name']] = {'uid': pod['metadata']['uid'], 'values': values}
    return result


def delta(before, after):
    assert set(before) == set(after)
    result = collections.defaultdict(float)
    for name in before:
        a, b = before[name], after[name]
        assert a['uid'] == b['uid'] and a['values']['process_start_time_seconds'] == b['values']['process_start_time_seconds'], 'process changed in measurement'
        for field in ['simplecache_source_loads_total', 'process_cpu_seconds_total', 'go_memstats_alloc_bytes_total',
                      'simplecache_source_write_duration_seconds_count', 'simplecache_source_write_duration_seconds_sum',
                      'simplecache_invalidation_broadcast_duration_seconds_count', 'simplecache_invalidation_broadcast_duration_seconds_sum',
                      'simplecache_version_rejections_total']:
            difference = b['values'][field]-a['values'][field]
            assert difference >= 0, (name, field, difference)
            result[field] += difference
    result['cache_rss_after_bytes'] = sum(n['values']['process_resident_memory_bytes'] for n in after.values())
    return dict(result)


def driver(m, config, name, collect_read_errors=False):
    local = m.REPORT / (name+'-config.json')
    local.write_text(json.dumps(config))
    m.k('cp', str(local), 'gateway-bench:/tmp/config.json')
    remote = '/tmp/'+name
    code = 0
    try:
        m.k('exec', 'gateway-bench', '--', '/tmp/gateway-bench', '-config', '/tmp/config.json', '-out', remote, timeout=200)
    except RuntimeError:
        code = 1  # Retrieve evidence on workload errors as well.
    destination = m.REPORT / name
    m.k('cp', 'gateway-bench:'+remote, str(destination))
    result = json.loads((destination/'summary.json').read_text())
    if code and collect_read_errors:
        records = [json.loads(line) for line in (destination/'requests.jsonl').read_text().splitlines()]
        writes = [r for r in records if r['method'] == 'PUT']
        assert result['errors'] > 0 and result['attempted'] == result['planned'] and result['skipped'] == 0 and result['incorrect'] == 0
        assert len(writes) == config['windows'] and all(r['status'] == 200 and not r['incorrect'] and not r.get('error') for r in writes)
        # Failed reads remain errors in every aggregate and raw trace. Never
        # retry them or reinterpret the completed measurement as error-free.
    else:
        assert code == 0, ('workload failed; raw error/503 samples preserved', name, result)
    return result


def reset_workers(m, ttl):
    old = {p['metadata']['uid'] for p in m.obj('pods', selector='app=products')['items']}
    cr = m.obj('simplecache', 'products')
    if cr['spec']['ttlSeconds'] != ttl:
        m.k('patch', 'simplecache', 'products', '--type=merge', '-p', json.dumps({'spec': {'ttlSeconds': ttl}}))
    else:
        m.k('rollout', 'restart', 'statefulset/products')
    def rolled():
        pods = [p for p in m.obj('pods', selector='app=products')['items'] if not p['metadata'].get('deletionTimestamp')]
        return len(pods) == 3 and all(p['metadata']['uid'] not in old and any(c['type']=='Ready' and c['status']=='True' for c in p.get('status',{}).get('conditions',[])) for p in pods) and m.condition(m.obj('simplecache', 'products'), 'Ready')
    m.wait_for('fresh worker processes for paired trial', rolled, 240)


def verify_mixed_source(m, repeats, windows, size):
    """Independently check final authoritative rows, outside timed traffic."""
    checks = []
    for repeat in range(repeats):
        key = f'perf-mixed-{repeat}-0000'
        row = source(m, key)
        assert row['version'] == windows + 1 and not row['deleted'], row
        assert row['value'] == json.loads(value(key, windows + 1, size)), key
        checks.append({'key': key, 'version': row['version'], 'full_payload_matches': True})
    (m.REPORT/'final-database-audit.json').write_text(json.dumps(checks, indent=2))


def install_control(m):
    route = m.obj('httproute', 'products')
    spec = route['spec']
    control_spec = json.loads(json.dumps(spec))
    control_spec['hostnames'] = [MODES['random']]
    m.apply({'apiVersion': 'gateway.networking.k8s.io/v1', 'kind': 'HTTPRoute', 'metadata': {'name': 'perf-random'}, 'spec': control_spec})
    policy = m.obj('backendtrafficpolicy', 'products')
    policy_spec = json.loads(json.dumps(policy['spec']))
    policy_spec['targetRefs'] = [{'group':'gateway.networking.k8s.io','kind':'HTTPRoute','name':'perf-random'}]
    policy_spec['loadBalancer'] = {'type':'RoundRobin'}
    m.apply({'apiVersion':'gateway.envoyproxy.io/v1alpha1','kind':'BackendTrafficPolicy','metadata':{'name':'perf-random'},'spec':policy_spec})
    def accepted():
        p=m.obj('backendtrafficpolicy','perf-random')
        return any(c['type']=='Accepted' and c['status']=='True' and c.get('observedGeneration')==p['metadata']['generation'] for a in p.get('status',{}).get('ancestors',[]) for c in a.get('conditions',[]))
    m.wait_for('round-robin control policy accepted', accepted, 120)


def summarize_outcome(manifest):
    """Combine current and retained evidence; a rerun cannot erase failures."""
    counts = dict(errors=0, incorrect=0, stale_at_start=0, skipped=0,
                  empty_attempts=0, read_errors=0, write_errors=0, unclassified_errors=0)
    attempts = [(t, t['client']) for t in manifest['trials']]
    attempts += [(t, t['summary']) for t in manifest.get('prior_failed_attempts', [])]
    for attempt, summary in attempts:
        errors = summary.get('errors', 0)
        counts['errors'] += errors
        counts['incorrect'] += summary.get('incorrect', 0)
        counts['stale_at_start'] += summary.get('stale_at_start', 0)
        counts['skipped'] += max(summary.get('skipped', 0),
                                 summary['planned'] - summary['attempted'])
        counts['empty_attempts'] += summary['attempted'] == 0
        if not errors:
            continue
        # Older clients have no per-method error totals. Classify their retained
        # raw records rather than assuming every transport/status error is a GET.
        try:
            path = pathlib.Path(attempt['run_report']) / attempt['name'] / 'requests.jsonl'
            records = [json.loads(line) for line in path.read_text().splitlines()]
            by_method = collections.Counter(r.get('method') for r in records if r.get('error'))
            if sum(by_method.values()) != errors:
                raise ValueError('raw error count differs from summary')
        except (OSError, ValueError):
            counts['unclassified_errors'] += errors
            continue
        counts['read_errors'] += by_method.pop('GET', 0)
        for method in ['PUT', 'DELETE', 'POST', 'PATCH']:
            counts['write_errors'] += by_method.pop(method, 0)
        counts['unclassified_errors'] += sum(by_method.values())
    hard_failure = not attempts or any(counts[k] for k in [
        'incorrect', 'stale_at_start', 'skipped', 'empty_attempts', 'write_errors', 'unclassified_errors'])
    status = 'failed' if hard_failure else ('completed-with-read-errors' if counts['read_errors'] else 'passed')
    return {'status': status, 'counts': counts}


def measure(m, proxies, previous=None):
    repeats = int(os.environ.get('GATEWAY_PERF_REPEATS', '3'))
    reads = int(os.environ.get('GATEWAY_PERF_READS', '32768'))
    assert 1 <= repeats <= 5 and 1024 <= reads <= 200000
    m.phase('performance-setup')
    m.RESULTS['performance_scope'] = 'Single-host exploratory comparison; in-cluster client; no production SLO or disk-IO claim'
    install_control(m)
    services=m.obj('services',namespace='envoy-gateway-system',selector='gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default')['items']
    service=next(s for s in services if any(p['port']==80 for p in s['spec']['ports']))
    endpoint='http://'+service['spec']['clusterIP']
    binary=m.REPORT/'gateway-bench'
    environment={**m.ENV,'CGO_ENABLED':'0','GOOS':'linux','GOFLAGS':os.environ.get('GOFLAGS','')+' -buildvcs=false'}
    if previous:
        old_result = json.loads((previous/'result.json').read_text())
        assert hashlib.sha256((previous/'gateway-bench').read_bytes()).hexdigest() == old_result['client_sha256']
        shutil.copy2(previous/'gateway-bench', binary)
    else:
        with (m.REPORT/'client-build.log').open('w') as log:
            subprocess.run(['go','build','-o',str(binary),'./cmd/gateway-bench'],cwd=m.ROOT/'geecache-engine',env=environment,stdout=log,stderr=subprocess.STDOUT,check=True,timeout=180)
    m.RESULTS['client_sha256']=hashlib.sha256(binary.read_bytes()).hexdigest()
    m.apply({'apiVersion':'v1','kind':'Pod','metadata':{'name':'gateway-bench','labels':{'app':'gateway-bench'}},'spec':{
        'containers':[{'name':'client','image':'simplecache-engine:gateway','command':['/bin/sh','-c','sleep 86400'],
                       'env':[{'name':'GOMAXPROCS','value':'1'},{'name':'CACHE_ADMIN_TOKEN','valueFrom':{'secretKeyRef':{'name':'products','key':'CACHE_ADMIN_TOKEN'}}}],
                       'resources':{'requests':{'cpu':'500m','memory':'128Mi'},'limits':{'cpu':'1','memory':'256Mi'}}}],
        'securityContext':{'runAsUser':65532,'runAsGroup':65532}}})
    m.k('wait','--for=condition=Ready','pod/gateway-bench','--timeout=120s')
    m.k('cp',str(binary),'gateway-bench:/tmp/gateway-bench')
    options={'keys':512,'value_bytes':4096,'reads':reads,'workers':8,'think_ms':1,'seed':42,'windows':128,'duration_seconds':0}
    manifest={'endpoint':endpoint,'service':service['metadata']['name'],'repeats':repeats,'options':options,
              'logical_cache_per_pod':1048576,'logical_cache_total':3145728,'variants':MODES,
              'database':'real PostgreSQL on temporary memory data; PG/OS caches not flushed; not a disk workload',
              'client':'dedicated Pod, one CPU limit/GOMAXPROCS=1; closed-loop bounded concurrency, not an offered-rate saturation test',
              'environment_before':pressure(),'trials':[]}
    if previous:
        prior = json.loads((previous/'performance.json').read_text())
        assert prior['repeats'] == repeats and prior['options'] == options
        assert prior['endpoint'] == endpoint and prior['variants'] == MODES
        manifest['continued_from'] = str(previous)
        manifest['trials'] = [dict(t, run_report=t.get('run_report', str(previous))) for t in prior['trials']]
        manifest['prior_failed_attempts'] = list(prior.get('prior_failed_attempts', []))
        failed_name = prior['failure']['trial']
        manifest['prior_failed_attempts'].append({'name': failed_name, 'run_report': str(previous),
                'summary': json.loads((previous/failed_name/'summary.json').read_text())})
    completed = {t['name'] for t in manifest['trials']}
    (m.REPORT/'performance.json').write_text(json.dumps(manifest,indent=2))
    # Dedicated proxy forwards are used only for topology qualification, never
    # for the measured client workload through the shared Gateway Service.
    with contextlib.ExitStack() as stack:
        bases=[stack.enter_context(m.forward('envoy-gateway-system','pod/'+p['metadata']['name'],10080)) for p in proxies]
        admins=[stack.enter_context(m.forward('envoy-gateway-system','pod/'+p['metadata']['name'],19000)) for p in proxies]
        for scenario in SCENARIOS:
            for repeat in range(repeats):
                order=['random','hash'] if repeat%2==0 else ['hash','random']
                prefix=f'perf-{scenario}-{repeat}'
                keys=[f'{prefix}-{i:04d}' for i in range(options['keys'])]
                for mode in order:
                    name=f'{scenario}-{repeat+1}-{mode}'
                    if name in completed:
                        continue
                    m.phase(name)
                    # The owned fixture is quiescent before resetting its rows;
                    # all cache/floor processes are replaced before traffic.
                    statements=['INSERT INTO simplecache_products(key,value,version,deleted) VALUES ('+literal(key)+',convert_to('+literal(value(key,1,4096))+",'UTF8'),1,false) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,version=1,deleted=false;" for key in keys]
                    sql(m,'\n'.join(statements))
                    reset_workers(m,5 if scenario=='ttl' else 300)
                    addresses={m.obj('service',f'products-shard-{i}')['spec']['clusterIP'] for i in range(3)}
                    m.wait_for('stable trial health',lambda:m.proxies_healthy(admins,addresses),90)
                    base={**options,'endpoint':endpoint,'host':MODES[mode],'prefix':prefix,'scenario':scenario}
                    if scenario=='warm-hot' and repeat==0:
                        key=keys[0]
                        def route_qualification():
                            control=set()
                            for _ in range(9):
                                status,h,_=m.request(bases[0],key,extra_headers={'Host':MODES['random']})
                                assert status==200
                                control.add(h['x-simplecache-node'])
                            hashes=[]
                            for proxy in bases:
                                for _ in range(3):
                                    status,h,_=m.request(proxy,key,extra_headers={'Host':MODES['hash']})
                                    assert status==200
                                    hashes.append(h['x-simplecache-node'])
                            assert len(control)==3 and len(set(hashes))==1, (control,hashes)
                            return {'random_nodes':sorted(control),'hash_node':hashes[0]}
                        qualification=m.wait_for('control and hash policies applied to actual traffic',route_qualification,90)
                        (m.REPORT/(name+'-routing-qualification.json')).write_text(json.dumps(qualification,indent=2))
                        reset_workers(m,300)
                        m.wait_for('qualified route health',lambda:m.proxies_healthy(admins,addresses),90)
                    if scenario=='cold-key':
                        base.update(reads=192,workers=32,think_ms=0)
                    elif scenario=='ttl':
                        base.update(reads=1,workers=1,think_ms=0,duration_seconds=18)
                    # Query actual hash placement, then reset its qualification
                    # data so each variant gets equally fresh cache processes.
                    if mode=='hash' and scenario=='capacity':
                        owner_bytes=collections.Counter()
                        for i,key in enumerate(keys):
                            req=m.urllib.request.Request(bases[0]+'/api?key='+key,headers={'Host':MODES['hash']})
                            with m.urllib.request.urlopen(req,timeout=5) as r:
                                assert r.status==200
                                owner_bytes[r.headers['X-SimpleCache-Node']]+=len(key)+4096
                        assert sum(owner_bytes.values())>1048576 and len(owner_bytes)==3 and max(owner_bytes.values())<=1048576,owner_bytes
                        (m.REPORT/(name+'-placement.json')).write_text(json.dumps(owner_bytes,indent=2))
                        reset_workers(m,300)
                        m.wait_for('qualified placement health',lambda:m.proxies_healthy(admins,addresses),90)
                    if scenario!='cold-key':
                        warm={**base,'scenario':'warm-hot' if scenario in ['warm-hot','ttl'] else 'capacity','reads':96 if scenario in ['warm-hot','ttl'] else options['keys']*3,'workers':1,'think_ms':0,'duration_seconds':0}
                        driver(m,warm,name+'-warmup')
                    before=cache_metrics(m,name+'-before')
                    host_before=pressure()
                    stop=threading.Event();sample_path=m.REPORT/(name+'-resources.jsonl')
                    sampler=threading.Thread(target=resource_sampler,args=(m,sample_path,stop),daemon=True);sampler.start()
                    try:
                        outcome=driver(m,base,name,collect_read_errors=scenario=='mixed')
                    except Exception as exc:
                        manifest['status']='failed'
                        manifest['failure']={'trial':name,'error':str(exc)}
                        (m.REPORT/'performance.json').write_text(json.dumps(manifest,indent=2))
                        raise
                    finally:
                        stop.set();sampler.join(timeout=15)
                    assert not sampler.is_alive(),'resource sampler did not stop'
                    after=cache_metrics(m,name+'-after')
                    changes=delta(before,after)
                    if scenario=='warm-hot':assert changes['simplecache_source_loads_total']==0,changes
                    if scenario=='ttl':assert changes['simplecache_source_loads_total']>=2,'TTL workload failed to span expiry cycles'
                    trial={'name':name,'run_report':str(m.REPORT),'mode':mode,'scenario':scenario,'repeat':repeat+1,'client':outcome,'cache_delta':changes,
                           'resources':resources(sample_path),'host_before':host_before,'host_after':pressure()}
                    manifest['trials'].append(trial)
                    (m.REPORT/'performance.json').write_text(json.dumps(manifest,indent=2))
                    m.check('completed paired workload '+name,{'requests':outcome['attempted'],'errors':outcome['errors'],'source_loads':changes['simplecache_source_loads_total']})
    verify_mixed_source(m, repeats, options['windows'], options['value_bytes'])
    manifest['final_database_audit'] = 'final-database-audit.json'
    manifest['environment_after']=pressure()
    manifest['outcome'] = summarize_outcome(manifest)
    manifest['status'] = manifest['outcome']['status']
    m.RESULTS['performance_status'] = manifest['status']
    m.RESULTS['performance_outcome'] = manifest['outcome']
    (m.REPORT/'performance.json').write_text(json.dumps(manifest,indent=2))
    report=['# Gateway routing comparison','','Single-host exploratory, closed-loop observations; not a production SLO.','',
            '| Scenario | Mode | Trials | Median P95 ms | Median success/s | Median source calls/read attempt | Errors |',
            '| --- | --- | ---: | ---: | ---: | ---: | ---: |']
    for scenario in SCENARIOS:
        for mode in MODES:
            trials=[t for t in manifest['trials'] if t['scenario']==scenario and t['mode']==mode]
            report.append('| '+scenario+' | '+mode+' | '+str(len(trials))+' | '+f"{statistics.median(t['client']['latency_all_attempts']['p95_ms'] for t in trials):.3f}"+' | '+f"{statistics.median(t['client']['success_per_second'] for t in trials):.1f}"+' | '+f"{statistics.median(t['cache_delta']['simplecache_source_loads_total']/max(1,t['client']['attempted']-t['client']['writes']) for t in trials):.4f}"+' | '+str(sum(t['client']['errors'] for t in trials))+' |')
    report+=['','[Raw performance data](performance.json). Each trial retains request JSONL, before/after cache metrics, process identities via raw Pod snapshots and sampled Kubelet resource envelopes.',
             '', 'Failure counts across current and retained attempts: `'+json.dumps(manifest['outcome']['counts'], sort_keys=True)+'`.',
             '', 'Overall outcome: '+manifest['status']+'. Prior failed attempts are retained separately in performance.json and their original reports; they are not converted to successful repetitions.',
             '', 'Percentiles include all attempts; throughput is successful completions in a closed loop with declared think time. Warmup/setup and metrics collection are outside client elapsed time. CPU deltas are cache-process totals; resource sampling uses cached Kubelet observations and is not a continuous RSS peak. PG is memory-backed/resident. No percentile subtraction estimates Gateway latency. Host IO/CPU pressure is retained; contended runs are exploratory, not clean performance baselines.']
    (m.REPORT/'performance.md').write_text('\n'.join(report)+'\n')
    m.RESULTS['performance_report']='performance.md'
    if manifest['status'] == 'failed':
        raise RuntimeError('performance evidence contains correctness, missing-request, write or unclassified failures; see performance.json')
