#!/usr/bin/env python3
"""Explicit operator-only preparation; never invoked by the worker or controller.

Usage: prepare.py EXISTING_REPOSITORY NEW_PRIVATE_OUTPUT_DIRECTORY
Only an exact Git archive, pinned public Node bytes and these operator files form
the build context. No host home, credentials, caches, .git or Docker socket enters
the toolchain. Docker remains an operator client outside build/source containers.
"""
import hashlib
import json
import os
import pathlib
import shutil
import subprocess
import sys
import time
import urllib.request

REGISTRY = 'registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e'

def run(argv, cwd=None):
    return subprocess.check_output(argv, cwd=cwd, text=True).strip()

def main():
    repo, output = map(lambda s: pathlib.Path(s).resolve(), sys.argv[1:3])
    if len(sys.argv) != 3 or output.exists() or not repo.is_dir():
        raise ValueError('existing repository and new private output required')
    output.mkdir(mode=0o700)
    operator = pathlib.Path(__file__).resolve().parent
    inputs = json.loads((operator / 'inputs.json').read_text())
    source = run(['git', 'rev-parse', inputs['source_commit']+'^{commit}'], repo)
    if source != inputs['source_commit']:
        raise ValueError('source identity changed')
    context = output / 'context'
    context.mkdir(mode=0o700)
    for name in ['Dockerfile', 'inputs.json', 'profile.py', 'finalize.py', 'runtime.py']:
        shutil.copyfile(operator / name, context / name)
    subprocess.run(['git', 'archive', '--format=tar', '--output='+str(context / 'source.tar'), source], cwd=repo, check=True)
    node = context / 'node.tar.xz'
    with urllib.request.urlopen(inputs['node_url'], timeout=60) as stream, node.open('xb') as dest:
        shutil.copyfileobj(stream, dest, length=1024*1024)
    if hashlib.sha256(node.read_bytes()).hexdigest() != inputs['node_sha256']:
        raise ValueError('public Node archive checksum mismatch')
    owner = 'rcp-realapp-prep-'+str(time.time_ns())
    journal = {'owner': owner, 'source_sha': source, 'inputs': inputs,
               'containers': [], 'images': [], 'completed': False}
    def save():
        (output / 'ownership.json').write_text(json.dumps(journal, indent=2)+'\n')
    save()
    docker = ['docker', '--context', 'orbstack']
    transient = []
    try:
        base_tag = 'rcp-realapp-toolchain:'+owner
        with (output / 'toolchain-build.log').open('w') as log:
            subprocess.run(docker+['build', '--pull=false', '--platform', inputs['platform'], '--progress=plain', '-t', base_tag, str(context)], stdout=log, stderr=subprocess.STDOUT, check=True)
        base_id = run(docker+['image', 'inspect', '--format', '{{.Id}}', base_tag])
        journal['images'].append(base_id)
        copier = run(docker+['create', '--network', 'none', '--read-only', '--label', 'rcp.fixture.owner='+owner, '--entrypoint', '/bin/true', base_id])
        if len(copier) != 64:
            raise ValueError('exact preparation container identity missing')
        transient.append(copier);journal['containers'].append(copier);save()
        subprocess.run(docker+['cp', copier+':/runtime-tree', str(output / 'runtime-tree')], check=True)
        subprocess.run(docker+['cp', copier+':/opt/rcp/profile.json', str(output / 'profile.json')], check=True)
        normalizer = output / 'rcp-worker-normalizer'
        subprocess.run(['go', 'build', '-o', str(normalizer), './cmd/worker'], cwd=repo, check=True)
        runtime = json.loads(run([str(normalizer), 'prepare-rootfs', str(output / 'runtime-tree'), str(output / 'runtime-rootfs.tar'), '134217728', '20000']))
        (output / 'runtime-spec.json').write_text(json.dumps(runtime, indent=2)+'\n')
        registry = run(docker+['run', '--rm', '-d', '--label', 'rcp.fixture.owner='+owner,
                              '--read-only', '--tmpfs', '/var/lib/registry:rw,size=3221225472',
                              '--memory', '512m', '--pids-limit', '128', '-p', '127.0.0.1::5000', REGISTRY])
        if len(registry) != 64:
            raise ValueError('exact registry identity missing')
        transient.append(registry);journal['containers'].append(registry);save()
        address = run(docker+['port', registry, '5000/tcp'])
        final = output / 'package'
        final.mkdir(mode=0o700)
        shutil.copyfile(output / 'runtime-rootfs.tar', final / 'runtime-rootfs.tar')
        # BuildKit treats bare sha256 IDs as repository tags. Use the exact owned
        # local tag, with identity checks on both sides; workers only see the
        # final registry-assigned immutable digest, never this preparation tag.
        if run(docker+['image', 'inspect', '--format', '{{.Id}}', base_tag]) != base_id:
            raise ValueError('preparation image changed before packaging')
        (final / 'Dockerfile').write_text('FROM '+base_tag+'\nCOPY runtime-rootfs.tar /opt/rcp/runtime-rootfs.tar\n')
        tag = address+'/rcp-toolchain:'+owner
        with (output / 'toolchain-package.log').open('w') as log:
            subprocess.run(docker+['build', '--pull=false', '--network=none', '-t', tag, str(final)], stdout=log, stderr=subprocess.STDOUT, check=True)
            subprocess.run(docker+['push', tag], stdout=log, stderr=subprocess.STDOUT, check=True)
        if run(docker+['image', 'inspect', '--format', '{{.Id}}', base_tag]) != base_id:
            raise ValueError('preparation image changed during packaging')
        digests = json.loads(run(docker+['image', 'inspect', '--format', '{{json .RepoDigests}}', tag]))
        pin = next(d for d in digests if d.startswith(address+'/rcp-toolchain@sha256:'))
        journal['images'].append(run(docker+['image', 'inspect', '--format', '{{.Id}}', pin]))
        result = {'toolchain_image': pin, 'profile_sha256': hashlib.sha256((output/'profile.json').read_bytes()).hexdigest(),
                  'runtime_rootfs': runtime, 'source_sha': source, 'buildkit_image': 'moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441',
                  'profile': json.loads((output/'profile.json').read_text())}
        (output / 'acceptance.json').write_text(json.dumps(result, indent=2)+'\n')
        journal['completed'] = True
        save()
        print(json.dumps(result, indent=2))
    finally:
        for container in transient:
            identity = run(docker+['inspect', '--format', '{{.Id}} {{index .Config.Labels "rcp.fixture.owner"}}', container])
            if identity != container+' '+owner:
                raise ValueError('preparation container ownership changed')
            subprocess.run(docker+['rm', '--force', container], check=True)
        journal['cleaned_containers'] = transient
        save()

if __name__ == '__main__':
    main()
