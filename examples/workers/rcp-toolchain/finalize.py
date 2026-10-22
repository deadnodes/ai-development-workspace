#!/usr/bin/python3
"""Pin the exact immutable dependency and compiler-seed trees after preparation."""
import importlib.util
import json
import pathlib
import hashlib

root = pathlib.Path('/opt/rcp')
spec = importlib.util.spec_from_file_location('profile', root / 'profile.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
profile = json.loads((root / 'inputs.json').read_text())
profile['tree_hashes'] = {}
for name, bound in [('gomod', profile['max_dependency_bytes']), ('node_modules', profile['max_dependency_bytes']), ('cache', profile['max_seed_bytes'])]:
    records = module.tree(root / name, bound)
    profile['tree_hashes'][name] = hashlib.sha256(module.canonical(records)).hexdigest()
(root / 'profile.json').write_text(json.dumps(profile, sort_keys=True, indent=2)+'\n')
print('profile_sha256='+module.digest(root / 'profile.json'))
