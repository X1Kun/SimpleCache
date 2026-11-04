"""Product environment ownership, retention and shared-manifest contracts."""
import importlib
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

import yaml

from tools.system.dev import Development
from tools.system.manifests import cache_resources, database_resources, kind_configuration

ROOT = pathlib.Path(__file__).resolve().parents[3]


class DevelopmentStateTest(unittest.TestCase):
    def environment(self, directory):
        env = Development(ROOT, state_dir=directory)
        self.addCleanup(env.close)
        return env

    def test_credentials_persist_privately_and_missing_credentials_are_not_rotated(self):
        with tempfile.TemporaryDirectory() as directory:
            first = self.environment(directory);first.initialize();first.credentials()
            token,password,cluster = first.token,first.password,first.cluster
            self.assertEqual(first.credentials_path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(first.report.stat().st_mode & 0o777, 0o700)
            second = self.environment(directory);second.credentials()
            self.assertEqual((second.token,second.password,second.cluster),(token,password,cluster))
            second.credentials_path.unlink()
            with self.assertRaisesRegex(RuntimeError,'credentials are missing'):
                second.up()

    def test_foreign_workspace_and_replaced_node_are_never_adopted(self):
        with tempfile.TemporaryDirectory() as directory:
            env = self.environment(directory);env.initialize();env.state['node_id']='owned-node';env.save()
            commands=[];env.run=lambda *a,**kw:commands.append(a)
            env.inspect_node=lambda:{'id':'other-node','running':True}
            with self.assertRaisesRegex(RuntimeError,'identity differs'):env.down()
            with self.assertRaisesRegex(RuntimeError,'identity differs'):env.destroy()
            self.assertEqual(commands,[])
            env.state['workspace']='/another/workspace';env.save()
            with self.assertRaisesRegex(RuntimeError,'another workspace'):
                Development(ROOT,state_dir=directory)

    def test_down_retains_node_pvc_credentials_and_up_reuses_them(self):
        with tempfile.TemporaryDirectory() as directory:
            env=self.environment(directory);env.initialize();env.credentials()
            env.state.update(node_id='owned-node',schema_sha256=env.schema_hash(),phase='ready');env.save()
            original=env.credentials_path.read_text();commands=[]
            env.inspect_node=lambda:{'id':'owned-node','running':True}
            env.run=lambda *a,**kw:commands.append(a[0]) or ''
            env.k=lambda *a,**kw:commands.append(list(a)) or ''
            env.obj=lambda *a,**kw:{'items':[]}
            env.down()
            self.assertEqual(env.state['phase'],'stopped')
            self.assertEqual(env.state['node_id'],'owned-node')
            self.assertEqual(env.credentials_path.read_text(),original)
            self.assertTrue(any(c[:3]==['delete','pod','postgres'] for c in commands))
            self.assertFalse(any('persistentvolumeclaim' in ' '.join(c) or 'cluster' in c and 'delete' in c for c in commands))
            env.start_forward=lambda:env.state.update(url='http://127.0.0.1:18080')
            with patch('tools.system.dev.prepare') as prepare:
                env.up();prepare.assert_called_once_with(env,development=True)
            self.assertTrue(env.existing)
            self.assertEqual(env.credentials_path.read_text(),original)

    def test_schema_change_and_active_writers_require_explicit_migration(self):
        with tempfile.TemporaryDirectory() as directory:
            env=self.environment(directory);env.initialize();env.state['schema_sha256']='old';env.save()
            with self.assertRaisesRegex(RuntimeError,'schema.sql changed'):
                env.up()
            env.assert_owned=lambda **kw:None;env.export_context=lambda:None
            env.missing_ok=lambda *a:'simplecache.cache.x1kun.com/products'
            with self.assertRaisesRegex(RuntimeError,'dev-down'):
                env.schema_upgrade()

    def test_unrelated_forward_pid_is_not_signaled(self):
        with tempfile.TemporaryDirectory() as directory:
            env=self.environment(directory);env.initialize();env.state['forward']={'pid':123,'service':'service/owned'}
            env.forward_matches=lambda forward:False
            with patch('os.kill') as kill,patch('os.pidfd_open',create=True) as open_pid:
                env.stop_forward();kill.assert_not_called();open_pid.assert_not_called()


class SharedProductDefinitionTest(unittest.TestCase):
    def test_product_persistence_and_validation_storage_are_explicit(self):
        for mode in ['persistent','ephemeral','memory']:
            resources=database_resources(ROOT,password='password',token='token',storage=mode,seed=mode=='persistent')
            pvc=[r for r in resources if r['kind']=='PersistentVolumeClaim']
            self.assertEqual(bool(pvc),mode=='persistent')
            pod=next(r for r in resources if r['kind']=='Pod')
            volume=next(v for v in pod['spec']['volumes'] if v['name']=='data')
            if mode=='persistent':self.assertEqual(volume['persistentVolumeClaim']['claimName'],pvc[0]['metadata']['name'])
            elif mode=='memory':self.assertEqual(volume['emptyDir']['medium'],'Memory')
            else:self.assertEqual(volume['emptyDir'],{})
            cm=next(r for r in resources if r['kind']=='ConfigMap')
            self.assertEqual('seed.sql' in cm['data'],mode=='persistent')

    def test_cache_profiles_reuse_defaults_and_kind_dev_etcd_is_not_tmpfs(self):
        base=next(r for r in cache_resources() if r['kind']=='SimpleCache')
        experiment=next(r for r in cache_resources(ttl_seconds=300,cache_bytes=1048576) if r['kind']=='SimpleCache')
        self.assertEqual(base['spec']['size'],3)
        self.assertEqual(base['spec']['ttlSeconds'],60)
        self.assertEqual(base['spec']['cacheBytes'],67108864)
        self.assertEqual(experiment['spec']['ttlSeconds'],300)
        self.assertEqual(experiment['spec']['cacheBytes'],1048576)
        self.assertNotIn('/tmp/simplecache-etcd',kind_configuration())
        self.assertIn('/tmp/simplecache-etcd',kind_configuration(validation=True))
        self.assertEqual(yaml.safe_load(kind_configuration())['kind'],'Cluster')

    def test_shared_modules_import_without_creating_an_environment(self):
        with patch('pathlib.Path.mkdir',side_effect=AssertionError('import created a directory')),patch('subprocess.run') as run:
            import tools.system.provision,tools.system.dev
            importlib.reload(tools.system.provision);importlib.reload(tools.system.dev)
            run.assert_not_called()

class DevelopmentConcurrencyTest(unittest.TestCase):
    def test_parallel_mutations_are_rejected_and_lock_reloads_current_state(self):
        from tools.system.dev import OperationBusy
        with tempfile.TemporaryDirectory() as directory:
            first=Development(ROOT,state_dir=directory);second=Development(ROOT,state_dir=directory)
            try:
                with first.operation():
                    first.initialize()
                    with self.assertRaises(OperationBusy):
                        with second.operation():pass
                with second.operation():
                    self.assertEqual(second.cluster,first.cluster)
            finally:
                first.close();second.close()

class DevelopmentDirectorySafetyTest(unittest.TestCase):
    def test_shared_directories_and_orphan_credentials_are_not_overwritten(self):
        for directory in [ROOT,ROOT/'bin',pathlib.Path('/tmp')]:
            with self.assertRaisesRegex(ValueError,'dedicated directory'):
                Development(ROOT,state_dir=directory)
        with tempfile.TemporaryDirectory() as directory:
            env=Development(ROOT,state_dir=directory)
            try:
                env.credentials_path.write_text('do-not-replace')
                with self.assertRaisesRegex(RuntimeError,'untracked credentials'):
                    env.initialize()
                self.assertEqual(env.credentials_path.read_text(),'do-not-replace')
            finally:env.close()

class DevelopmentUntrackedDirectoryTest(unittest.TestCase):
    def test_untracked_directory_is_not_chmodded_or_populated(self):
        with tempfile.TemporaryDirectory() as directory:
            path=pathlib.Path(directory)/'unrelated';path.mkdir(mode=0o755)
            (path/'notes.txt').write_text('keep')
            before=path.stat().st_mode & 0o777
            with self.assertRaisesRegex(RuntimeError,'untracked files'):
                Development(ROOT,state_dir=path)
            self.assertEqual(path.stat().st_mode & 0o777,before)
            self.assertEqual((path/'notes.txt').read_text(),'keep')
            self.assertFalse((path/'commands.log').exists())
