"""Retained, workspace-owned development environment and persistent database."""
import contextlib
import fcntl
import hashlib
import json
import os
import pathlib
import secrets
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request

from .manifests import apply_fixture, database_resources
from .provision import prepare
from .runtime import ClusterCommands, NODE, GATEWAY, PROXY


def private_json(path, value):
    temporary = path.with_suffix('.tmp')
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, 'w') as output:
        json.dump(value, output, indent=2)
    os.replace(temporary, path)
    os.chmod(path, 0o600)


class OperationBusy(RuntimeError):
    pass


class Development(ClusterCommands):
    def __init__(self, root, *, state_dir=None):
        self.root = pathlib.Path(root).resolve()
        self.report = pathlib.Path(state_dir or os.environ.get('SIMPLECACHE_DEV_STATE_DIR', self.root/'bin/dev')).resolve()
        if self.report in [self.root, self.root/'bin', pathlib.Path('/'), pathlib.Path('/tmp'), pathlib.Path.home()]:
            raise ValueError('development state must use a dedicated directory')
        self.report.mkdir(parents=True, exist_ok=True)
        self.state_path = self.report/'state.json'
        self.credentials_path = self.report/'credentials.json'
        self.state = json.loads(self.state_path.read_text()) if self.state_path.exists() else {}
        if self.state and self.state.get('workspace') != str(self.root):
            raise RuntimeError('development state belongs to another workspace')
        if not self.state and any(p.name not in ['commands.log','operation.lock'] for p in self.report.iterdir()):
            raise RuntimeError('development state directory contains untracked files')
        os.chmod(self.report, 0o700)
        self.cluster = self.state.get('cluster', '')
        self.kind = str(self.root/'simplecache-operator/bin/gateway-v1.9.2/kind-v0.33.0')
        self.env = {**os.environ, 'KUBECONFIG': str(self.report/'kubeconfig')}
        self.node, self.gateway, self.proxy = NODE, GATEWAY, PROXY
        self.proxy_local = self.cluster + ':envoy'
        self.extended = self.performance = self.arrival = False
        self.created = self.local_proxy_tag_created = False
        self.results = {}
        self.existing = False
        self.log = (self.report/'commands.log').open('a+')

    @contextlib.contextmanager
    def operation(self):
        descriptor = os.open(self.report/'operation.lock', os.O_RDWR | os.O_CREAT, 0o600)
        try:
            try: fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError: raise OperationBusy('another development operation is running; see commands.log')
            self.state = json.loads(self.state_path.read_text()) if self.state_path.exists() else {}
            if self.state and self.state.get('workspace') != str(self.root):
                raise RuntimeError('development state belongs to another workspace')
            self.cluster = self.state.get('cluster', '')
            self.proxy_local = self.cluster + ':envoy'
            yield
        finally:
            os.close(descriptor)

    def close(self):
        self.log.close()

    def save(self):
        private_json(self.state_path, self.state)

    def inspect_node(self):
        if not self.cluster:
            return None
        result = subprocess.run(['docker', 'inspect', '--format',
                                 '{{.Id}} {{index .Config.Labels "io.x-k8s.kind.cluster"}} {{.State.Running}}',
                                 self.cluster+'-control-plane'], text=True, capture_output=True, env=self.env, timeout=15)
        if result.returncode:
            if 'No such' in result.stderr:
                return None
            raise RuntimeError('could not inspect Docker development node: ' + result.stderr.strip())
        node_id, cluster, running = result.stdout.strip().split()
        if cluster != self.cluster:
            raise RuntimeError('node is not the recorded Kind cluster')
        return {'id': node_id, 'running': running == 'true'}

    def assert_owned(self, *, allow_missing=False):
        node = self.inspect_node()
        if node is None:
            if allow_missing:
                return None
            raise RuntimeError('recorded development node is missing; dev-destroy resets stale state explicitly')
        if node['id'] != self.state.get('node_id'):
            raise RuntimeError('node identity differs from development state; refusing to adopt or modify it')
        return node

    def mark_creating(self):
        self.state['creating'] = True
        self.save()

    def capture_node(self):
        node = self.inspect_node()
        if node is None:
            raise RuntimeError('development node was not created')
        if self.state.get('node_id') and self.state['node_id'] != node['id']:
            raise RuntimeError('development node identity changed')
        if not self.state.get('node_id') and not self.state.get('creating'):
            raise RuntimeError('no recorded creation attempt for this node')
        self.state.update(node_id=node['id'], creating=False)
        self.save()

    def export_context(self):
        self.run(['kind', 'export', 'kubeconfig', '--name', self.cluster, '--kubeconfig', self.env['KUBECONFIG']])

    def initialize(self):
        if self.credentials_path.exists() and not self.state:
            raise RuntimeError('untracked credentials exist; refusing to replace them')
        self.cluster = 'simplecache-dev-' + secrets.token_hex(4)
        self.proxy_local = self.cluster + ':envoy'
        self.state = {'workspace': str(self.root), 'cluster': self.cluster, 'phase': 'new'}
        private_json(self.credentials_path, {'password': secrets.token_hex(16), 'token': secrets.token_urlsafe(24)})
        self.save()

    def credentials(self):
        if not self.credentials_path.exists():
            raise RuntimeError('private credentials are missing; restore them rather than replacing a live database password')
        os.chmod(self.credentials_path, 0o600)
        credentials = json.loads(self.credentials_path.read_text())
        self.password, self.token = credentials['password'], credentials['token']

    def schema_hash(self):
        return hashlib.sha256((self.root/'deploy/products/schema.sql').read_bytes()).hexdigest()

    def ensure_storage_helper(self):
        config = self.obj('configmap', 'local-path-config', namespace='local-path-storage')
        raw = config.get('data', {}).get('helperPod.yaml')
        if not raw:
            return
        import yaml
        helper = yaml.safe_load(raw)
        image = helper['spec']['containers'][0].get('image')
        if not image:
            return
        try:
            self.run(['docker', 'exec', self.cluster+'-control-plane', 'crictl', 'inspecti', image], timeout=20)
            return
        except RuntimeError:
            pass
        try:
            self.run(['docker', 'image', 'inspect', image], timeout=20)
        except RuntimeError:
            self.run(['docker', 'pull', image], timeout=180)
        self.run(['kind', 'load', 'docker-image', '--name', self.cluster, image], timeout=300)

    def up(self):
        self.existing = False
        if not self.state or self.state.get('phase') == 'destroyed':
            self.initialize()
        self.credentials()
        if self.state.get('schema_sha256') and self.state['schema_sha256'] != self.schema_hash():
            raise RuntimeError('schema.sql changed; run dev-down and dev-schema-upgrade before dev-up')
        if self.state.get('node_id'):
            node = self.assert_owned()
            if not node['running']:
                self.run(['docker', 'start', self.cluster+'-control-plane'])
            self.existing = True
        elif self.state.get('creating') and self.inspect_node():
            self.capture_node()
            self.existing = True
        self.state['phase'] = 'starting'; self.save()
        try:
            prepare(self, development=True)
            self.state.update(phase='ready', schema_sha256=self.schema_hash())
            self.save()
            self.start_forward()
            print('Development ready:', self.state['url'])
            print('Read example:', self.state['url'] + '/api?key=product%3A42')
            print('Private credentials:', self.credentials_path)
        except BaseException:
            if self.state.get('creating'):
                try: self.capture_node()
                except RuntimeError: pass
            self.state['phase'] = 'start-failed'; self.save()
            raise

    def forward_matches(self, forward):
        try:
            arguments = pathlib.Path(f"/proc/{forward['pid']}/cmdline").read_bytes().split(b'\0')
            return all(value.encode() in arguments for value in [self.env['KUBECONFIG'], 'kind-'+self.cluster,
                                                                'port-forward', forward['service']])
        except (FileNotFoundError, ProcessLookupError):
            return False

    def stop_forward(self):
        forward = self.state.get('forward')
        if forward and self.forward_matches(forward):
            try:
                pidfd = os.pidfd_open(forward['pid']) if hasattr(os, 'pidfd_open') else None
            except ProcessLookupError:
                pidfd = None
            try:
                if self.forward_matches(forward):
                    if pidfd is not None:
                        signal.pidfd_send_signal(pidfd, signal.SIGTERM)
                    else:
                        os.kill(forward['pid'], signal.SIGTERM)
            except ProcessLookupError:
                pass
            finally:
                if pidfd is not None: os.close(pidfd)
            deadline = time.monotonic() + 5
            while self.forward_matches(forward) and time.monotonic() < deadline:
                time.sleep(0.05)
            if self.forward_matches(forward):
                raise RuntimeError('owned port-forward did not stop')
        self.state.pop('forward', None)
        self.save()

    def start_forward(self):
        self.stop_forward()
        selector = 'gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default'
        services = self.obj('services', namespace='envoy-gateway-system', selector=selector)['items']
        service = next(s for s in services if any(p['port'] == 80 for p in s['spec']['ports']))
        port = int(os.environ.get('SIMPLECACHE_DEV_PORT', '18080'))
        if not 1 <= port <= 65535: raise ValueError('invalid SIMPLECACHE_DEV_PORT')
        with socket.socket() as reservation: reservation.bind(('127.0.0.1', port))
        target = 'service/' + service['metadata']['name']
        args = ['kubectl', '--kubeconfig', self.env['KUBECONFIG'], '--context', 'kind-'+self.cluster,
                '-n', 'envoy-gateway-system', 'port-forward', '--address=127.0.0.1', target, f'{port}:80']
        with (self.report/'access.log').open('a') as output:
            process = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=output, stderr=subprocess.STDOUT,
                                       env=self.env, start_new_session=True)
        self.state.update(forward={'pid': process.pid, 'service': target}, url=f'http://127.0.0.1:{port}')
        self.save()
        def reachable():
            if process.poll() is not None: raise RuntimeError('port-forward exited; see access.log')
            try:
                with urllib.request.urlopen(self.state['url']+'/api?key=product%3A42', timeout=2) as response:
                    return response.status == 200
            except urllib.error.HTTPError as error:
                return error.code == 404  # Existing users may have deleted the sample key.
        self.wait_for('development Gateway access', reachable, 45)

    def missing_ok(self, *args):
        try: return self.k(*args)
        except RuntimeError as error:
            if any(text in str(error) for text in ['NotFound', 'not found', "doesn't have a resource"]): return ''
            raise

    def down(self):
        if not self.state or self.state.get('phase') == 'destroyed':
            print('No development environment'); return
        if not self.state.get('node_id') and self.state.get('creating'): self.capture_node()
        self.assert_owned(); self.export_context(); self.stop_forward()
        self.missing_ok('delete', 'simplecache', 'products', '--ignore-not-found', '--wait=true')
        self.missing_ok('wait', '--for=delete', 'gateway/products', '--timeout=180s')
        def proxies_gone():
            try:
                return not self.obj('pods', namespace='envoy-gateway-system', selector='gateway.envoyproxy.io/owning-gateway-name=products,gateway.envoyproxy.io/owning-gateway-namespace=default')['items']
            except RuntimeError as error:
                if 'NotFound' in str(error): return True
                raise
        self.wait_for('Gateway workload cleanup before stopping controllers', proxies_gone, 180)
        self.missing_ok('delete', 'pod', 'postgres', '--ignore-not-found', '--wait=true', '--timeout=120s')
        for namespace, deployment in [('simplecache-operator-system','simplecache-operator-controller-manager'),('envoy-gateway-system','envoy-gateway')]:
            self.missing_ok('-n', namespace, 'scale', 'deployment/'+deployment, '--replicas=0')
        self.state['phase'] = 'stopped'; self.save()
        print('Development workloads stopped; Kind node, database PVC and credentials retained')

    def status(self):
        if not self.state:
            print('No development environment; run make dev-up'); return
        print('Cluster:', self.cluster, 'Phase:', self.state.get('phase'))
        if self.state.get('phase') == 'destroyed': return
        if not self.state.get('node_id'):
            print('Node initialization pending; see', self.report/'commands.log'); return
        node = self.assert_owned(allow_missing=True)
        if not node:
            print('Recorded node missing; data is not implicitly recreated'); return
        if not node['running']:
            print('Kind node is stopped'); return
        self.export_context()
        print(self.k('get', 'pods,services,persistentvolumeclaims').strip())
        print(self.missing_ok('get', 'simplecache', 'products').strip())
        print(self.missing_ok('get', 'gateway', 'products').strip())
        for namespace in ['envoy-gateway-system','simplecache-operator-system']:
            print(namespace + ':')
            print(self.missing_ok('-n',namespace,'get','deployments,pods').strip())
        print('Access:', self.state.get('url', 'not started'),
              'forward running:', bool(self.state.get('forward') and self.forward_matches(self.state['forward'])))

    def schema_upgrade(self):
        self.assert_owned(); self.export_context(); self.credentials()
        if self.missing_ok('get', 'simplecache', 'products', '-o', 'name').strip():
            raise RuntimeError('run dev-down before schema migration')
        database = database_resources(self.root, password=self.password, token=self.token, storage='persistent', seed=True)
        apply_fixture(self, 'database', database)
        self.wait_for('database for schema migration', lambda: any(c['type']=='Ready' and c['status']=='True'
                      for c in self.obj('pod','postgres').get('status',{}).get('conditions',[])), 300)
        self.k('exec', '-i', 'postgres', '--', 'psql', '-X', '-U', 'cache', '-d', 'products', '-v',
               'ON_ERROR_STOP=1', '--single-transaction', '-f', '-', data=(self.root/'deploy/products/schema.sql').read_text())
        self.state.update(schema_sha256=self.schema_hash(), phase='schema-upgraded'); self.save()
        print('Schema upgraded atomically; sample data was not reseeded. Run make dev-up')

    def destroy(self):
        if not self.state or self.state.get('phase') == 'destroyed':
            print('No development environment'); return
        if not self.state.get('node_id') and self.state.get('creating') and self.inspect_node(): self.capture_node()
        self.assert_owned(allow_missing=True); self.stop_forward()
        self.run(['kind', 'delete', 'cluster', '--name', self.cluster], timeout=180)
        try: self.run(['docker', 'image', 'rm', self.proxy_local], timeout=20)
        except RuntimeError as error:
            if 'No such image' not in str(error): raise
        self.credentials_path.unlink(missing_ok=True)
        self.state = {'workspace':str(self.root),'cluster':self.cluster,'phase':'destroyed'}; self.save()
        print('Owned development cluster destroyed; its database PVC/data were removed')
