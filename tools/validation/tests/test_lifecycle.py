"""Exercise resource ownership and exception cleanup without a Docker daemon."""
import base64
import contextlib
import importlib
import json
import pathlib
import pkgutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from tools.validation.gateway.runner import GatewayRun, run_suite


class LifecycleTest(unittest.TestCase):
    def test_imports_and_construction_do_not_create_files_or_run_commands(self):
        import tools.validation
        with tempfile.TemporaryDirectory() as directory, patch('subprocess.run') as run, patch('subprocess.Popen') as popen, patch('pathlib.Path.mkdir', side_effect=AssertionError('import created a directory')), patch('pathlib.Path.open', side_effect=AssertionError('import opened an artifact')):
            for module in pkgutil.walk_packages(tools.validation.__path__, tools.validation.__name__ + '.'):
                if '.tests' not in module.name:
                    importlib.reload(importlib.import_module(module.name))
            context = GatewayRun(directory, environ={})
            self.assertFalse(context.report.exists())
            self.assertEqual(list(pathlib.Path(directory).iterdir()), [])
            run.assert_not_called()
            popen.assert_not_called()

    def test_independent_runs_keep_separate_flags_state_and_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            first = GatewayRun(directory, environ={'GATEWAY_EXTENDED': '1'})
            second = GatewayRun(directory, environ={'GATEWAY_PERF': '1'})
            self.assertTrue(first.extended)
            self.assertFalse(second.extended)
            self.assertEqual(second.results['suite'], 'performance')
            self.assertNotEqual(first.cluster, second.cluster)
            self.assertNotEqual(first.env['KUBECONFIG'], second.env['KUBECONFIG'])
            with first, second:
                first.check('first-only', {})
                self.assertEqual(second.results['checks'], [])

    def test_continuation_uses_fresh_fault_keys_even_on_the_same_cluster(self):
        from tools.validation.gateway.faults import case_key
        with tempfile.TemporaryDirectory() as directory:
            first = GatewayRun(directory, environ={})
            continued = GatewayRun(directory, environ={})
            continued.cluster = first.cluster
            self.assertNotEqual(case_key(first, 'cold-restart'), case_key(continued, 'cold-restart'))
            self.assertEqual(case_key(first, 'cold-restart'), case_key(first, 'cold-restart'))

    def test_setup_failure_cleans_partial_cluster_and_preserves_original_error(self):
        for error in [RuntimeError('setup failed'), KeyboardInterrupt('interrupted')]:
            with self.subTest(error=type(error).__name__), tempfile.TemporaryDirectory() as directory:
                context = GatewayRun(directory, environ={})
                commands = []
                context.run = lambda args, **kw: commands.append(args) or ''
                context.snapshot = lambda label: None
                with self.assertRaises(type(error)):
                    with context:
                        context.created = True
                        context.local_proxy_tag_created = True
                        raise error
                self.assertEqual(commands, [
                    ['kind', 'delete', 'cluster', '--name', context.cluster],
                    ['docker', 'image', 'rm', context.proxy_local]])
                result = json.loads((context.report/'result.json').read_text())
                self.assertEqual(result['status'], 'failed')
                self.assertEqual(result['cleanup']['status'], 'passed')
                self.assertTrue(context.trace.closed and context.log.closed)

    def test_real_prepare_failure_cleans_a_partially_created_cluster(self):
        from tools.validation.environment.gateway import prepare
        with tempfile.TemporaryDirectory() as directory:
            context = GatewayRun(directory, environ={})
            commands = []
            def execute(args, **kwargs):
                commands.append(args)
                if args[:3] == ['kind', 'create', 'cluster']:
                    raise RuntimeError('partial Kind creation')
                return ''
            context.run = execute
            context.snapshot = lambda label: None
            with self.assertRaisesRegex(RuntimeError, 'partial Kind creation'):
                with context:
                    prepare(context)
            self.assertEqual(commands[-1], ['kind', 'delete', 'cluster', '--name', context.cluster])
            self.assertEqual(context.results['cleanup']['status'], 'passed')

    def test_continuation_rejects_foreign_report_without_deleting_resources(self):
        from tools.validation.gateway.runner import continue_retained_experiment
        with tempfile.TemporaryDirectory() as directory:
            context = GatewayRun(directory, environ={})
            commands = []
            context.run = lambda args, **kw: commands.append(args) or ''
            with self.assertRaises(AssertionError):
                with context:
                    continue_retained_experiment(context, '/tmp/foreign-report')
            self.assertEqual(commands, [])
            self.assertFalse(context.created)
            self.assertEqual(context.results['cleanup']['status'], 'not-created')

    def test_snapshot_failure_still_cleans_and_closes(self):
        with tempfile.TemporaryDirectory() as directory:
            context = GatewayRun(directory, environ={})
            context.run = lambda *args, **kw: ''
            def broken_snapshot(label):
                raise OSError('snapshot disk failure')
            context.snapshot = broken_snapshot
            with self.assertRaisesRegex(RuntimeError, 'original'):
                with context:
                    context.created = True
                    raise RuntimeError('original')
            result = json.loads((context.report/'result.json').read_text())
            self.assertEqual(result['cleanup']['status'], 'passed')
            self.assertIn('snapshot_error', result)

    def test_cleanup_failure_fails_successful_run_after_bounded_retries(self):
        with tempfile.TemporaryDirectory() as directory, patch('tools.validation.environment.kind.time.sleep'):
            context = GatewayRun(directory, environ={})
            commands = []
            def fail_delete(args, **kwargs):
                commands.append(args)
                if args[0] == 'kind':
                    raise RuntimeError('delete failed')
                return ''
            context.run = fail_delete
            with self.assertRaisesRegex(RuntimeError, 'cleanup failed'):
                with context:
                    context.created = context.local_proxy_tag_created = True
                    context.results['status'] = 'passed'
            self.assertEqual(len(commands), 4)
            result = json.loads((context.report/'result.json').read_text())
            self.assertEqual(result['status'], 'failed')
            self.assertEqual(result['cleanup']['status'], 'failed')
            self.assertTrue(context.trace.closed and context.log.closed)

    def test_keep_failed_retains_only_failed_owned_run(self):
        for failed in [False, True]:
            with self.subTest(failed=failed), tempfile.TemporaryDirectory() as directory:
                context = GatewayRun(directory, environ={'GATEWAY_KEEP_FAILED': '1'})
                commands = []
                context.run = lambda args, **kw: commands.append(args) or ''
                with context:
                    context.created = context.local_proxy_tag_created = True
                    context.results['status'] = 'failed' if failed else 'passed'
                self.assertEqual(len(commands), 0 if failed else 2)
                self.assertEqual(context.results['cleanup']['status'], 'retained-for-debug' if failed else 'passed')

    def test_performance_dispatch_passes_explicit_context(self):
        with tempfile.TemporaryDirectory() as directory:
            context = GatewayRun(directory, environ={'GATEWAY_PERF': '1'})
            context.snapshot = lambda label: None
            def measure(actual, proxies):
                self.assertIs(actual, context)
                self.assertEqual(proxies, ['proxy'])
                actual.results['performance_status'] = 'passed'
            with context, patch('tools.validation.gateway.runner.prepare', return_value=['proxy']), patch('tools.validation.gateway.performance.measure', side_effect=measure):
                run_suite(context)
                self.assertEqual(context.results['status'], 'passed')

    def test_valid_retained_continuations_keep_identity_and_cleanup_ownership(self):
        from tools.validation.gateway.runner import continue_retained_experiment
        from tools.validation.gateway.performance import continue_retained
        for performance in [False, True]:
            with self.subTest(performance=performance), tempfile.TemporaryDirectory() as directory:
                context = GatewayRun(directory, environ={'GATEWAY_PERF': str(int(performance))})
                previous = pathlib.Path(directory)/'bin/reports/retained'
                previous.mkdir(parents=True)
                cluster = 'simplecache-gateway-retained'
                old = dict(status='failed', suite='performance' if performance else 'extended',
                           cluster=cluster, phase='database-connectivity-loss', images={},
                           cleanup={'status': 'retained-for-debug'},
                           checks=[{'name': str(i)} for i in range(20)])
                (previous/'result.json').write_text(json.dumps(old))
                cr = {'metadata': {'uid': 'cache-uid', 'name': 'products', 'namespace': 'default'},
                      'spec': {'size': 3, 'ttlSeconds': 300 if performance else 20, 'cacheBytes': 1048576}}
                db = {'metadata': {'uid': 'db-uid', 'name': 'postgres', 'namespace': 'default'}}
                for name, item in [('simplecaches', cr), ('pods', db)]:
                    (previous/('failure-'+name+'.json')).write_text(json.dumps({'items': [item]}))
                (previous/'performance.json').write_text(json.dumps({'status': 'failed', 'failure': {'trial': 'mixed'}}))
                def obj(kind, name=None, namespace='default', **kwargs):
                    if kind == 'simplecache': return cr
                    if kind == 'pod': return db
                    if kind == 'service': return {'spec': {'selector': {'app': 'postgres'}, 'clusterIP': '10.0.0.1'}}
                    return {'items': [{'metadata': {'name': 'proxy-'+str(i), 'uid': str(i)},
                                      'status': {'conditions': [{'type': 'Ready', 'status': 'True'}]}} for i in range(2)]}
                context.obj = obj
                context.snapshot = lambda label: None
                context.forward = lambda *args: contextlib.nullcontext('http://proxy')
                context.proxies_healthy = lambda *args: True
                context.worker_metrics = lambda label: 0
                commands = []
                def run(args, **kwargs):
                    commands.append(args)
                    return json.dumps({'io.x-k8s.kind.cluster': cluster, 'io.x-k8s.kind.role': 'control-plane'})
                context.run = run
                secret = json.dumps({'data': {'CACHE_ADMIN_TOKEN': base64.b64encode(b'test-token').decode()}})
                def extend(actual, *args):
                    self.assertIs(actual, context)
                    for i in range(5): actual.check('continued-'+str(i), {})
                def measure(actual, proxies, path):
                    self.assertIs(actual, context)
                    self.assertEqual(path, previous)
                    actual.results['performance_status'] = 'passed'
                with context, patch('subprocess.check_output', return_value=secret), patch('tools.validation.gateway.faults.continue_exercise', side_effect=extend), patch('tools.validation.gateway.performance.measure', side_effect=measure):
                    (continue_retained if performance else continue_retained_experiment)(context, str(previous))
                    self.assertEqual(context.results['status'], 'passed')
                    self.assertEqual(context.token, 'test-token')
                    self.assertEqual(context.results['checks'][0]['run_report'], str(previous))
                self.assertIn(['kind', 'delete', 'cluster', '--name', cluster], commands)
                self.assertIn(['docker', 'image', 'rm', cluster+':envoy'], commands)

    def test_entrypoint_help_has_no_cluster_or_artifact_side_effects(self):
        root = pathlib.Path(__file__).resolve().parents[3]
        for command in [['python3', '-m', 'tools.validation', '--help'], ['python3', str(root/'scripts/gateway/smoke.py'), '--help']]:
            result = subprocess.run(command, cwd=root, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('--suite', result.stdout)
