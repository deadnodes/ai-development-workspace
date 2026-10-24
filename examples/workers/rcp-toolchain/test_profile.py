"""Operator input guards: no network, Docker, host caches or application changes."""
import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('operator_profile', pathlib.Path(__file__).with_name('profile.py'))
profile = importlib.util.module_from_spec(spec)
spec.loader.exec_module(profile)

class ProfileGuards(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.scope = ['p', 'source', 'trusted_source', 'linux/arm64']
        self.p = {'scope': dict(zip(['product_id', 'repository_id', 'trust', 'platform'], self.scope)),
                  'versions': {k: k+' exact pinned version' for k in ['go', 'node', 'npm', 'python', 'gcc', 'make', 'git']},
                  'max_dependency_bytes': 64, 'max_seed_bytes': 64, 'tree_hashes': {}, 'source_hashes': {}}
        for name in ['gomod', 'node_modules', 'cache']:
            tree = self.root / name
            tree.mkdir()
            (tree / 'input').write_bytes(b'locked input')
            self.p['tree_hashes'][name] = hashlib.sha256(profile.canonical(profile.tree(tree, 64))).hexdigest()
        self.source = self.root / 'source'
        self.source.mkdir()
        for name in ['go.mod', 'go.sum', 'package-lock.json', 'Makefile']:
            (self.source / name).write_bytes(b'exact source input')
            self.p['source_hashes'][name] = profile.digest(self.source / name)
        self.save()
        self.root_patch = mock.patch.object(profile, 'ROOT', self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)
        self.command_patch = mock.patch.object(profile, 'command', side_effect=lambda argv: self.p['versions']['python' if argv[0]=='python3' else argv[0]])
        self.command_patch.start()
        self.addCleanup(self.command_patch.stop)

    def save(self):
        (self.root / 'profile.json').write_bytes(profile.canonical(self.p))
        self.expected = profile.digest(self.root / 'profile.json')

    def verify(self):
        return profile.verify_profile(self.expected, self.scope)

    def test_exact_scope_tools_dependencies_seed_and_locks(self):
        self.assertEqual(self.verify(), self.p)
        profile.source_locks(self.source, self.p)

    def test_scope_rejects_each_dimension(self):
        for i in range(4):
            scope = self.scope.copy(); scope[i] += '-other'
            with self.subTest(dimension=i), self.assertRaisesRegex(ValueError, 'scope mismatch'):
                profile.verify_profile(self.expected, scope)

    def test_profile_bytes_are_pinned(self):
        with (self.root/'profile.json').open('ab') as stream: stream.write(b' ')
        with self.assertRaisesRegex(ValueError, 'manifest mismatch'): self.verify()

    def test_tool_version_rejects(self):
        with mock.patch.object(profile, 'command', return_value='wrong version'):
            with self.assertRaisesRegex(ValueError, 'tool version mismatch'): self.verify()

    def test_missing_or_corrupt_dependencies_and_seed_reject(self):
        for name in ['gomod', 'node_modules', 'cache']:
            path = self.root/name/'input'
            for content in [None, b'corrupt']:
                with self.subTest(tree=name, content=content):
                    if content is None: path.unlink()
                    else: path.write_bytes(content)
                    with self.assertRaisesRegex(ValueError, 'input mismatch'): self.verify()
                    path.write_bytes(b'locked input')

    def test_missing_tree_rejects(self):
        (self.root/'cache'/'input').unlink(); (self.root/'cache').rmdir()
        with self.assertRaisesRegex(ValueError, 'missing immutable'): self.verify()

    def test_dependency_quota_rejects(self):
        (self.root/'gomod'/'input').write_bytes(b'x'*65)
        with self.assertRaisesRegex(ValueError, 'byte bound'): self.verify()

    def test_external_dependency_link_rejects(self):
        (self.root/'gomod'/'escape').symlink_to(self.source/'go.mod')
        with self.assertRaisesRegex(ValueError, 'escapes immutable'): self.verify()

    def test_each_lock_and_entrypoint_rejects(self):
        for name in self.p['source_hashes']:
            path = self.source/name
            with self.subTest(lock=name):
                path.write_bytes(b'changed')
                with self.assertRaisesRegex(ValueError, 'lock/entrypoint mismatch'): profile.source_locks(self.source,self.p)
                path.write_bytes(b'exact source input')

    def test_offline_private_environment(self):
        env = profile.environment()
        for name,value in {'GOENV':'off','GOWORK':'off','GOTOOLCHAIN':'local','GOPROXY':'off','GOSUMDB':'off','GOMODCACHE':'/work/gomod','GOCACHE':'/work/cache','npm_config_offline':'true'}.items():
            self.assertEqual(env[name],value)
        self.assertIn('-mod=readonly',env['GOFLAGS'])
        self.assertNotIn('TEST_DATABASE_URL',env)
        self.assertNotIn('RC_TOKEN',env)

if __name__ == '__main__': unittest.main()
