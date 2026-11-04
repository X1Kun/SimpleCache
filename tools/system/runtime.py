"""Common scoped cluster commands; no test or development lifecycle policy."""
import json
import os
import subprocess
import time
import urllib.error

NODE = 'kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed'
GATEWAY = 'envoyproxy/gateway:v1.9.2'
PROXY = 'envoyproxy/envoy:distroless-v1.39.1@sha256:eb2c01c13125d1629637cb4e4cce7207009fb7cc2c8027f9742758549d15b6f4'

class ClusterCommands:
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
