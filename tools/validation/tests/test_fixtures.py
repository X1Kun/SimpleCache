"""Offline fixture contracts: identity, parameters, isolation and redaction."""
import json
import pathlib
import tempfile
import types
import unittest
from unittest.mock import patch

import yaml

from tools.validation.environment.fixtures import (
    apply_fixture, benchmark_pod, database_resources, deploy_benchmark,
    enable_backend, fixture_text, load_resources,
)

ROOT = pathlib.Path(__file__).resolve().parents[3]


class DatabaseFixtureTest(unittest.TestCase):
    def render(self, memory=False):
        return database_resources(ROOT, password='fixture-password', token='fixture-token', memory_data=memory)

    def test_source_sql_credentials_and_readiness_contract(self):
        docs = {d['kind']: d for d in self.render()}
        self.assertEqual(set(docs), {'ConfigMap', 'Secret', 'Pod', 'Service'})
        self.assertEqual(docs['ConfigMap']['data']['schema.sql'], (ROOT/'deploy/products/schema.sql').read_text())
        secret = docs['Secret']['stringData']
        self.assertEqual(secret['POSTGRES_PASSWORD'], 'fixture-password')
        self.assertEqual(secret['CACHE_ADMIN_TOKEN'], 'fixture-token')
        self.assertIn(':fixture-password@postgres:5432/products', secret['DATABASE_URL'])
        pod = docs['Pod']; client = pod['spec']['containers'][0]
        self.assertEqual(pod['metadata']['name'], 'postgres')
        self.assertEqual(client['image'], 'postgres:17-alpine')
        self.assertEqual(client['readinessProbe']['tcpSocket']['port'], 5432)
        self.assertEqual(docs['Service']['spec']['selector'], pod['metadata']['labels'])
        self.assertEqual(docs['Service']['spec']['ports'][0]['port'], 5432)
        ref = next(e for e in client['env'] if e['name']=='POSTGRES_PASSWORD')['valueFrom']['secretKeyRef']
        self.assertEqual(ref, {'name': docs['Secret']['metadata']['name'], 'key': 'POSTGRES_PASSWORD'})
        volume = next(v for v in pod['spec']['volumes'] if v['name']=='schema')
        self.assertEqual(volume['configMap']['name'], docs['ConfigMap']['metadata']['name'])
        self.assertEqual(next(v for v in client['volumeMounts'] if v['name']=='schema')['mountPath'], '/docker-entrypoint-initdb.d')
        self.assertTrue(next(v for v in client['volumeMounts'] if v['name']=='schema')['readOnly'])

    def test_storage_modes_are_fresh_and_do_not_leak_between_runs(self):
        memory = next(d for d in self.render(True) if d['kind']=='Pod')
        disk = next(d for d in self.render(False) if d['kind']=='Pod')
        self.assertEqual(len(disk['spec']['volumes']), 2)
        self.assertEqual(next(v for v in disk['spec']['volumes'] if v['name']=='data')['emptyDir'], {})
        self.assertEqual(next(v for v in memory['spec']['volumes'] if v['name']=='data')['emptyDir'], {'medium':'Memory', 'sizeLimit':'512Mi'})
        memory['spec']['containers'][0]['image'] = 'mutated'
        again = next(d for d in self.render(True) if d['kind']=='Pod')
        self.assertEqual(again['spec']['containers'][0]['image'], 'postgres:17-alpine')
        self.assertEqual(len(again['spec']['volumes']), 2)

    def test_rendered_report_redacts_secret_without_changing_apply(self):
        resources = self.render(True)
        with tempfile.TemporaryDirectory() as directory:
            applied = []
            ctx = types.SimpleNamespace(report=pathlib.Path(directory), apply=applied.append)
            apply_fixture(ctx, 'database', resources)
            text = (ctx.report/'database-resources.json').read_text()
            self.assertNotIn('fixture-password', text)
            self.assertNotIn('fixture-token', text)
            self.assertIn('<redacted>', text)
            self.assertIs(applied[0], resources)
            self.assertEqual(resources[1]['stringData']['CACHE_ADMIN_TOKEN'], 'fixture-token')


class BenchmarkFixtureTest(unittest.TestCase):
    def test_profiles_preserve_distinct_cpu_auth_and_common_structure(self):
        for profile, cpu, gomax, auth in [('performance','1','1',True), ('arrival','2','2',False)]:
            with self.subTest(profile=profile):
                pod = benchmark_pod(profile, image='owned-image:test')
                self.assertEqual(pod['metadata']['name'], 'gateway-bench')
                self.assertEqual(pod['spec']['securityContext'], {'runAsUser':65532,'runAsGroup':65532})
                client = pod['spec']['containers'][0]
                self.assertEqual(client['image'], 'owned-image:test')
                self.assertEqual(client['command'], ['/bin/sh','-c','sleep 86400'])
                self.assertEqual(client['resources']['requests'], {'cpu':'500m','memory':'128Mi'})
                self.assertEqual(client['resources']['limits'], {'cpu':cpu,'memory':'256Mi'})
                env = {e['name']:e for e in client['env']}
                self.assertEqual(env['GOMAXPROCS']['value'], gomax)
                self.assertEqual('CACHE_ADMIN_TOKEN' in env, auth)
                if auth:
                    self.assertEqual(env['CACHE_ADMIN_TOKEN']['valueFrom']['secretKeyRef'], {'name':'products','key':'CACHE_ADMIN_TOKEN'})
        first = benchmark_pod('performance');first['spec']['containers'][0]['env'].clear()
        self.assertEqual(len(benchmark_pod('performance')['spec']['containers'][0]['env']),2)
        with self.assertRaises(ValueError):benchmark_pod('unknown')

    def test_deployment_records_effective_parameters_and_waits(self):
        with tempfile.TemporaryDirectory() as directory:
            applied, commands = [], []
            ctx = types.SimpleNamespace(report=pathlib.Path(directory), results={}, apply=applied.append,
                                        k=lambda *args: commands.append(args))
            deploy_benchmark(ctx, 'arrival')
            self.assertEqual(len(applied), 1)
            self.assertEqual(ctx.results['benchmark_profile']['gomaxprocs'], '2')
            self.assertFalse(ctx.results['benchmark_profile']['admin_token_reference'])
            self.assertEqual(commands, [('wait','--for=condition=Ready','pod/gateway-bench','--timeout=120s')])
            ctx.results['benchmark_profile']['resources']['limits']['cpu'] = 'mutated'
            self.assertEqual(applied[0]['spec']['containers'][0]['resources']['limits']['cpu'], '2')


class YamlConfigurationTest(unittest.TestCase):
    def test_kind_control_plane_contract(self):
        config = yaml.safe_load(fixture_text('kind.yaml'))
        self.assertEqual(config['kind'], 'Cluster')
        self.assertEqual(config['apiVersion'], 'kind.x-k8s.io/v1alpha4')
        self.assertEqual(len(config['nodes']),1)
        patches = [yaml.safe_load(s) for s in config['nodes'][0]['kubeadmConfigPatches']]
        self.assertEqual(patches[0]['timeouts']['kubernetesAPICall'], '4m0s')
        self.assertEqual(patches[1]['etcd']['local']['dataDir'], '/tmp/simplecache-etcd')

    def test_backend_setting_preserves_existing_fields_and_handles_formatting(self):
        for raw in [
            'logging:\n  level: info\nextensionApis: {}\n',
            '# upstream formatting changed\nextensionApis:\n  enableBackend: false\n  extra: keep\nlogging: {level: info}\n',
            'logging: {level: info}\n',
            'logging: {level: info}\nextensionApis: null\n',
        ]:
            with self.subTest(raw=raw):
                result = yaml.safe_load(enable_backend(raw))
                self.assertTrue(result['extensionApis']['enableBackend'])
                self.assertEqual(result['logging'], {'level':'info'})
                if 'extra: keep' in raw:self.assertEqual(result['extensionApis']['extra'],'keep')
        for raw in ['[]', 'scalar', '', 'extensionApis: []', 'extensionApis: false']:
            with self.subTest(raw=raw), self.assertRaises(ValueError):enable_backend(raw)
        with self.assertRaises(yaml.YAMLError):enable_backend('extensionApis: [')

    def test_resource_loader_rejects_bad_shapes(self):
        for raw in ['[]','kind: Pod','']:
            with self.subTest(raw=raw), patch('tools.system.manifests.fixture_text', return_value=raw), self.assertRaises(ValueError):
                load_resources('bad.yaml')
