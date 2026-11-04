"""Calibrate arrival scheduling using separate client/reference processes."""
import datetime
import hashlib
import json
import os
import pathlib
import secrets
import subprocess
import urllib.request


def render_report(report, result):
    lines = ['# Arrival scheduler calibration', '', result['scope'], '',
             'Outcome: **' + result.get('status', 'incomplete') + '**.', '',
             '| Trial | Policy | Planned | Reference received | Dropped | Dispatch P95 ms | Total P95 ms |',
             '| --- | --- | ---: | ---: | ---: | ---: | ---: |']
    for trial in result['trials']:
        summary = trial['summary']; counts = summary['arrival']['counts']
        lines.append(f"| {trial['name']} | {trial['mode']} | {summary['planned']} | {trial['reference_received']} | {summary['skipped']} | {counts['schedule_to_dispatch']['p95_ms']:.3f} | {counts['schedule_to_completion']['p95_ms']:.3f} |")
    lines += ['', 'All attempts are retained. Strict and bounded runs alternate in order. The independent reference count must equal attempted requests. A bounded run must have no drops to qualify. The 5ms budget applies to dispatch, while worker queue and total delay remain measured.', '', '[Raw results](calibration.json).']
    (report/'calibration.md').write_text('\n'.join(lines)+'\n')


def main():
    root = pathlib.Path(__file__).resolve().parents[2]
    report = root / 'bin/reports' / (datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-arrival-calibration-' + secrets.token_hex(3))
    report.mkdir(parents=True)
    environment = {**os.environ, 'GOFLAGS': '-buildvcs=false', 'CGO_ENABLED': '0'}
    for command, binary in [('arrival-reference', 'reference'), ('gateway-bench', 'client')]:
        with (report / (binary + '-build.log')).open('w') as log:
            subprocess.run(['go', 'build', '-o', str(report/binary), './cmd/' + command],
                           cwd=root/'geecache-engine', env=environment, stdout=log, stderr=subprocess.STDOUT, check=True)
    result = {'scope': 'Separate reference/client processes on one host; 4KiB in-memory reference, not cache capacity',
              'trials': [], 'client_sha256': hashlib.sha256((report/'client').read_bytes()).hexdigest()}
    reference = subprocess.Popen([str(report/'reference')], stdout=subprocess.PIPE, text=True, env=environment)
    try:
        endpoint = json.loads(reference.stdout.readline())['endpoint']
        for repeat in range(3):
            order = [('strict', 0), ('bounded', 5)] if repeat % 2 == 0 else [('bounded', 5), ('strict', 0)]
            for mode, tolerance in order:
                name = f'{repeat+1}-{mode}'
                config = {'endpoint': endpoint, 'host': 'calibration.local', 'prefix': 'calibration',
                          'scenario': 'arrival', 'distribution': 'hot-scan', 'keys': 2048, 'value_bytes': 4096,
                          'reads': 1, 'workers': 64, 'max_pending': 128, 'seed': 42,
                          'max_schedule_lag_ms': tolerance, 'max_catch_up': 8 if mode == 'bounded' else 4,
                          'stages': [{'name': 'rate-1500', 'rate': 1500, 'seconds': 5}]}
                path = report/(name+'-config.json'); path.write_text(json.dumps(config))
                with urllib.request.urlopen(endpoint+'/stats') as response:
                    before = json.load(response)['received']
                with (report/(name+'-client.log')).open('w') as log:
                    completed = subprocess.run([str(report/'client'), '-config', str(path), '-out', str(report/name)],
                                               env={**environment, 'GOMAXPROCS': '2'}, stdout=log, stderr=subprocess.STDOUT, timeout=30)
                with urllib.request.urlopen(endpoint+'/stats') as response:
                    received = json.load(response)['received'] - before
                summary = json.loads((report/name/'summary.json').read_text())
                assert received == summary['attempted'], 'reference count differs from client evidence'
                assert summary['incorrect'] == 0 and summary['errors'] == 0
                assert bool(completed.returncode) == (summary['arrival']['status'] != 'passed')
                result['trials'].append({'name': name, 'mode': mode, 'summary': summary, 'reference_received': received})
                (report/'calibration.json').write_text(json.dumps(result, indent=2))
                print(name, 'received', received, 'drops', summary['skipped'], flush=True)
        bounded = [t for t in result['trials'] if t['mode']=='bounded']
        result['status'] = 'passed' if all(t['summary']['skipped']==0 for t in bounded) else 'client-limited'
        (report/'calibration.json').write_text(json.dumps(result, indent=2))
        render_report(report, result)
        print('Report:', report, 'Outcome:', result['status'], flush=True)
    finally:
        reference.terminate()
        try:
            reference.wait(timeout=8)
        except subprocess.TimeoutExpired:
            reference.kill(); reference.wait()


if __name__ == '__main__':
    main()
