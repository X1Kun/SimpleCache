"""Gateway assertions shared by functional and performance suites."""
import json
import urllib.error
import urllib.parse
import urllib.request

class GatewayChecks:
    def affinity(self, bases, keys, expected=None, shards=None):
        mappings = []
        for base in bases:
            mapping = {}
            for key in keys:
                code, headers, body = self.request(base, key)
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


    def proxies_healthy(self, admins, addresses):
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


    def worker_metrics(self, label):
        source_loads = 0
        api_lookups = 0
        for pod in self.obj('pods', selector='app=products')['items']:
            with self.forward('default', 'pod/' + pod['metadata']['name'], 9999) as base:
                text = urllib.request.urlopen(base + '/metrics', timeout=5).read().decode()
                (self.report / (label + '-' + pod['metadata']['name'] + '-metrics.txt')).write_text(text)
                for line in text.splitlines():
                    if line.startswith('simplecache_source_loads_total{'):
                        source_loads += float(line.rsplit(' ', 1)[1])
                    if line.startswith('simplecache_local_lookups_total{') and 'kind="api"' in line:
                        api_lookups += float(line.rsplit(' ', 1)[1])
        assert api_lookups > 0, 'no API lookup evidence'
        return source_loads


