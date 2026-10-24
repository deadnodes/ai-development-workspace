import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location('delivery', pathlib.Path(__file__).with_name('deploy-gitops.py'))
delivery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(delivery)

class DeploymentPatchTests(unittest.TestCase):
    def test_only_image_and_activation_change(self):
        before = 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  replicas: 0\n  template:\n    spec:\n      containers:\n        - name: app\n          image: registry/app:pending\n          envFrom:\n            - secretRef:\n                name: private\n'
        image = 'ghcr.io/team/app@sha256:' + 'a' * 64
        after = delivery.patch_manifest(before, image)
        self.assertEqual(after, before.replace('replicas: 0','replicas: 1').replace('registry/app:pending',image))
        self.assertEqual(delivery.patch_manifest(after,image),after)
        for invalid in [before+'          image: other\n', before.replace('kind: Deployment','kind: StatefulSet')]:
            with self.assertRaises(ValueError): delivery.patch_manifest(invalid,image)
        with self.assertRaises(ValueError): delivery.patch_manifest(before,'registry/app:latest')

class ProvenanceTests(unittest.TestCase):
    def test_untrusted_evidence_never_writes(self):
        from unittest.mock import patch
        import copy
        import json
        import base64
        good_run = dict(conclusion='success',status='completed',head_repository={'full_name':'team/app'},head_branch='main',path='.github/workflows/ci.yml',event='push',head_sha='a'*40)
        good_evidence = dict(source_sha='a'*40,repository='team/app',run_id='7',image_repository='ghcr.io/team/app',digest='sha256:'+'b'*64)
        cases = [('run', {'conclusion':'failure'}), ('run',{'head_repository':{'full_name':'attacker/app'}}), ('run',{'path':'.github/workflows/other.yml'}),('run',{'head_branch':'feature/unreviewed'}),('evidence',{'source_sha':'c'*40}),('evidence',{'image_repository':'ghcr.io/attacker/app'}),('evidence',{'run_id':'8'})]
        for kind, changes in cases:
            with self.subTest(kind=kind,changes=changes):
                run,evidence=copy.deepcopy(good_run),copy.deepcopy(good_evidence)
                (run if kind=='run' else evidence).update(changes)
                puts=[]
                def fake_gh(*args,payload=None):
                    if payload is not None:
                        puts.append(payload)
                        return {'commit':{'sha':'d'*40}}
                    if args[-1]=='repos/team/app': return {'default_branch':'main'}
                    if args[-1].endswith('/runs/7'):return run
                    raise AssertionError('Manifest must not be requested on invalid provenance')
                def download(args,**kwargs):
                    target=pathlib.Path(args[args.index('--dir')+1]);(target/'image.json').write_text(json.dumps(evidence))
                argv=['deploy-gitops.py','--source-repository','team/app','--run-id','7','--gitops-repository','team/gitops','--gitops-branch','main','--manifest','clusters/dev/app.yaml','--apply']
                with patch('sys.argv',argv),patch.object(delivery,'gh',fake_gh),patch.object(delivery.subprocess,'run',download):
                    with self.assertRaises(ValueError):delivery.main()
                self.assertEqual(puts,[])
