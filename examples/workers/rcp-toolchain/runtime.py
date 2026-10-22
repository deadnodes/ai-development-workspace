#!/usr/bin/python3
"""Select public Git/CA and its ELF closure from the pinned read-only base.

No host root/home, application binary, credentials or service state is an input.
Only explicitly selected paths and their proven selected in-image link targets
are materialized; the core normalizer independently validates the resulting tree.
"""
import hashlib
import json
import pathlib
import re
import shutil
import subprocess

ROOT = pathlib.Path('/runtime-tree')
BASE = 'golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b'
initial = ['/usr/bin/git', '/usr/lib/git-core/git-remote-http',
           '/usr/lib/git-core/git-remote-https', '/etc/ssl/certs/ca-certificates.crt',
           '/etc/nsswitch.conf']
selected = set(initial)
pending = list(initial)
while pending:
    name = pending.pop()
    source = pathlib.Path(name)
    target = source.resolve(strict=True)
    if not target.is_relative_to(pathlib.Path('/')) or not target.is_file():
        raise ValueError('runtime input is not a regular in-image target')
    if str(target) not in selected:
        selected.add(str(target))
        pending.append(str(target))
    if target.read_bytes()[:4] == b'\x7fELF':
        result = subprocess.run(['ldd', str(target)], text=True, capture_output=True, check=True)
        for dependency in re.findall(r'(/[^\s()]+)', result.stdout):
            if dependency not in selected:
                selected.add(dependency)
                pending.append(dependency)

ROOT.mkdir(mode=0o755)
records = []
for name in sorted(selected):
    source = pathlib.Path(name)
    target = source.resolve(strict=True)
    # All original links are flattened only after their exact resolved target
    # has itself entered the explicit closure above.
    if str(target) not in selected:
        raise ValueError('unselected link target')
    destination = ROOT / name.lstrip('/')
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(target, destination)
    destination.chmod(0o555 if target.stat().st_mode & 0o111 else 0o444)
    records.append({'path': name.lstrip('/'), 'public_base_target': str(target),
                    'sha256': hashlib.sha256(destination.read_bytes()).hexdigest()})
manifest = ROOT / 'usr/share/rcp-runtime-inputs.json'
manifest.parent.mkdir(parents=True, exist_ok=True)
manifest.write_text(json.dumps({'public_base': BASE, 'selection': records}, sort_keys=True, indent=2)+'\n')
manifest.chmod(0o444)
