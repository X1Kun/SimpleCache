"""Per-run evidence and report lifecycle; imports do not create artifacts."""
import datetime
import json
import os
import pathlib
import secrets
import threading
import time

class RunReport:
    def __init__(self, root, *, environ=None):
        self.root = pathlib.Path(root).resolve()
        settings = dict(os.environ if environ is None else environ)
        self.extended = settings.get('GATEWAY_EXTENDED', '0') == '1'
        self.performance = settings.get('GATEWAY_PERF', '0') == '1'
        self.keep_failed = settings.get('GATEWAY_KEEP_FAILED', '0') == '1'
        self.report = self.root / 'bin/reports' / (datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-gateway-' + secrets.token_hex(3))
        self.cluster = 'simplecache-gateway-' + secrets.token_hex(4)
        self.env = {**settings, 'KUBECONFIG': str(self.report / 'kubeconfig')}
        self.token = secrets.token_urlsafe(24)
        self.proxy_local = self.cluster + ':envoy'
        self.created = False
        self.local_proxy_tag_created = False
        self.trace_lock = threading.Lock()
        self.started = time.monotonic()
        self.results = {'cluster': self.cluster, 'checks': [], 'suite': 'performance' if self.performance else ('extended' if self.extended else 'smoke'), 'phase': 'setup'}

    def open(self):
        self.report.mkdir(parents=True)
        self.trace = (self.report / 'requests.jsonl').open('w')
        try:
            self.log = (self.report / 'commands.log').open('w+')
        except BaseException:
            self.trace.close()
            raise

    def close(self):
        try:
            (self.report / 'result.json').write_text(json.dumps(self.results, indent=2))
        finally:
            self.trace.close()
            self.log.close()

    def phase(self, name):
        self.results['phase'] = name
        (self.report / 'result.json').write_text(json.dumps(self.results, indent=2))
        print('PHASE:', name, flush=True)


    def check(self, name, detail):
        self.results['checks'].append({'name': name, 'phase': self.results['phase'], 'run_report': str(self.report), 'elapsed_seconds': time.monotonic()-self.started, 'detail': detail})
        (self.report / 'result.json').write_text(json.dumps(self.results, indent=2))
        print('PASS:', name, flush=True)

