"""Owned Kind resources, command execution, HTTP transport and cleanup."""
import contextlib
import json
import hashlib
import os
import pathlib
import socket
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from ..reporting.run import RunReport

NODE = 'kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed'
GATEWAY = 'envoyproxy/gateway:v1.9.2'
PROXY = 'envoyproxy/envoy:distroless-v1.39.1@sha256:eb2c01c13125d1629637cb4e4cce7207009fb7cc2c8027f9742758549d15b6f4'

class KindEnvironment(RunReport):
    def __init__(self, root, *, environ=None):
        super().__init__(root, environ=environ)
        self.node, self.gateway, self.proxy = NODE, GATEWAY, PROXY
        self.kind = str(self.root / 'simplecache-operator/bin/gateway-v1.9.2/kind-v0.33.0')
        self.results.update(gateway=self.gateway, node=self.node)

    def __enter__(self):
        self.open()
        return self

    def __exit__(self, exc_type, exc, traceback):
        try:
            if exc_type is not None:
                self.results.update(status='failed', error=str(exc))
                if self.created:
                    try:
                        self.snapshot('failure')
                    except BaseException as snapshot_error:
                        self.results['snapshot_error'] = str(snapshot_error)
            self.cleanup()
        finally:
            self.close()
            print('Report:', self.report, flush=True)
        return False

    def run(self, args, *, data=None, timeout=180, cwd=None):
        cwd = self.root if cwd is None else cwd
        # Do not log stdin: it can contain the ephemeral database/admin credentials.
        if args[0] == 'kind':
            args = [self.kind, *args[1:]]
        print('+', ' '.join(map(str, args)), flush=True)
        if args[:2] == ['docker', 'build']:
            build_log = self.report / (str(args[-1]) + '-build.log')
            with build_log.open('w') as log:
                p = subprocess.run(list(map(str, args)), text=True, stdout=log, stderr=subprocess.STDOUT, env=self.env, cwd=cwd, timeout=timeout)
            if p.returncode:
                raise RuntimeError(f'Docker build failed: {build_log}\n{build_log.read_text()[-6000:]}')
            return ''
        self.log.write('+ ' + ' '.join(map(str, args)) + '\n')
        self.log.flush()
        start = self.log.tell()
        # Keep long-running Kind/download progress visible before process completion.
        p = subprocess.run(list(map(str, args)), input=data, text=True, stdout=self.log,
                           stderr=subprocess.STDOUT, env=self.env, cwd=cwd, timeout=timeout)
        self.log.flush()
        self.log.seek(start)
        output = self.log.read()
        self.log.seek(0, os.SEEK_END)
        if p.returncode:
            raise RuntimeError(f'{args[0]} exited {p.returncode}: {output[-6000:]}')
        return output


    def k(self, *args, **kwargs):
        return self.run(['kubectl', '--context', 'kind-' + self.cluster, *args], **kwargs)


    def obj(self, kind, name=None, namespace='default', selector=None):
        args = ['-n', namespace, 'get', kind]
        if name:
            args.append(name)
        if selector:
            args += ['-l', selector]
        return json.loads(self.k(*args, '-o', 'json'))


    def apply(self, value):
        if isinstance(value, list):
            value = {'apiVersion': 'v1', 'kind': 'List', 'items': value}
        self.k('apply', '-f', '-', data=json.dumps(value))


    def condition(self, value, kind):
        return any(c['type'] == kind and c['status'] == 'True' and
                   c.get('observedGeneration') == value['metadata']['generation']
                   for c in value.get('status', {}).get('conditions', []))


    def wait_for(self, label, predicate, seconds=180):
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
    def forward(self, namespace, resource, remote):
        with socket.socket() as s:
            s.bind(('127.0.0.1', 0))
            local = s.getsockname()[1]
        log = (self.report / f'forward-{namespace}-{resource.replace("/", "-")}-{local}.log').open('w')
        p = subprocess.Popen(['kubectl', '--context', 'kind-' + self.cluster, '-n', namespace,
                              'port-forward', '--address=127.0.0.1', resource, f'{local}:{remote}'],
                             stdout=log, stderr=subprocess.STDOUT, env=self.env)
        try:
            def connected():
                if p.poll() is not None:
                    raise RuntimeError('port-forward exited')
                with socket.socket() as s:
                    return s.connect_ex(('127.0.0.1', local)) == 0
            self.wait_for('port-forward', connected, 30)
            yield 'http://127.0.0.1:' + str(local)
        finally:
            p.terminate()
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()
            log.close()


    def request(self, base, key, method='GET', value=None, etag=None, create=False, raw=None, path='/api', event_version=None, auth=True, extra_headers=None, raw_body=None):
        query = raw if raw is not None else urllib.parse.urlencode({'key': key}, quote_via=urllib.parse.quote, safe='')
        headers = {}
        if method != 'GET' and auth:
            headers['Authorization'] = 'Bearer ' + self.token
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
            result = response.status, {name.lower(): value for name, value in response.headers.items()}, response.read().decode()
        with self.trace_lock:
            self.trace.write(json.dumps({'phase': self.results['phase'], 'method': method, 'key': key, 'status': result[0], 'etag': result[1].get('etag'), 'node': result[1].get('x-simplecache-node'), 'body_sha256': hashlib.sha256(result[2].encode()).hexdigest(), 'elapsed_seconds': time.monotonic()-started}, ensure_ascii=False) + '\n')
            self.trace.flush()
        return result


    def snapshot(self, label):
        try:
            self.run(['kind', 'export', 'logs', '--name', self.cluster, str(self.report / (label + '-kind-logs'))], timeout=120)
        except Exception as exc:
            self.log.write(f'Kind log export failed: {exc}\n')
        if not pathlib.Path(self.env['KUBECONFIG']).exists():
            return
        kinds = ['simplecaches', 'statefulsets', 'services', 'pods', 'gateways', 'httproutes',
                 'backends', 'backendtrafficpolicies', 'envoyproxies']
        for kind in kinds:
            try:
                (self.report / f'{label}-{kind}.json').write_text(self.k('get', kind, '-A', '-o', 'json'))
            except Exception as exc:
                self.log.write(f'Snapshot failed: {kind}: {exc}\n')
        for ns, target in [('default', 'pod/postgres'), ('default', 'statefulset/products'), ('simplecache-operator-system', 'deployment/simplecache-operator-controller-manager'),
                           ('envoy-gateway-system', 'deployment/envoy-gateway')]:
            try:
                (self.report / f'{label}-{ns}.log').write_text(self.k('-n', ns, 'logs', target, '--all-containers', '--tail=250'))
            except Exception:
                pass


    def cleanup(self):
        primary_failed = self.results.get('status') == 'failed'
        cleanup_error = None
        self.results['cleanup'] = {'status': 'not-created'}
        if self.created and primary_failed and self.keep_failed:
            self.results['cleanup'] = {'status': 'retained-for-debug', 'context': 'kind-' + self.cluster, 'kubeconfig': self.env['KUBECONFIG']}
        elif self.created:
            for attempt in range(3):
                try:
                    self.run(['kind', 'delete', 'cluster', '--name', self.cluster], timeout=120)
                    self.results['cleanup'] = {'status': 'passed', 'attempts': attempt + 1}
                    cleanup_error = None
                    break
                except Exception as exc:
                    cleanup_error = exc
                    if attempt < 2:
                        time.sleep(2 + 3 * attempt)
            if cleanup_error is not None:
                self.results['cleanup'] = {'status': 'failed', 'error': str(cleanup_error)}
                self.results['status'] = 'failed'
        if self.local_proxy_tag_created and not (primary_failed and self.keep_failed):
            try:
                self.run(['docker', 'image', 'rm', self.proxy_local], timeout=30)
            except Exception as exc:
                self.results['local_tag_cleanup_error'] = str(exc)
                self.results['status'] = 'failed'
                if cleanup_error is None:
                    cleanup_error = exc
        if cleanup_error is not None and not primary_failed:
            raise RuntimeError('test resource cleanup failed; see result.json') from cleanup_error
