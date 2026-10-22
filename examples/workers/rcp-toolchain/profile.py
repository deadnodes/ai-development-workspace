#!/usr/bin/python3
"""Fixed operator program; source Dockerfiles and dependency fallbacks are unused."""
import hashlib
import json
import os
import pathlib
import shutil
import subprocess
import sys
import time

ROOT = pathlib.Path('/opt/rcp')

def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()

def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()

def tree(root, bound):
    root = pathlib.Path(root)
    if not root.is_dir() or root.is_symlink():
        raise ValueError('missing immutable input tree')
    records, total = [], 0
    for path in sorted(root.rglob('*')):
        relative = path.relative_to(root).as_posix()
        stat = path.lstat()
        if path.is_symlink():
            target = path.resolve(strict=True)
            if not target.is_relative_to(root.resolve()) or not target.is_file():
                raise ValueError('dependency link escapes immutable tree')
            records.append([relative, 'link', os.readlink(path)])
        elif path.is_file():
            total += stat.st_size
            if total > bound:
                raise ValueError('immutable input byte bound exceeded')
            records.append([relative, 'file', stat.st_mode & 0o777, stat.st_size, digest(path)])
        elif not path.is_dir():
            raise ValueError('unsupported immutable input')
    return records

def command(argv, env=None, cwd=None):
    return subprocess.check_output(argv, env=env, cwd=cwd, text=True).strip()

def verify_profile(expected, scope):
    profile = ROOT / 'profile.json'
    if digest(profile) != expected:
        raise ValueError('profile manifest mismatch')
    p = json.loads(profile.read_text())
    if p['scope'] != dict(zip(['product_id', 'repository_id', 'trust', 'platform'], scope)):
        raise ValueError('profile scope mismatch')
    versions = {'go': ['go', 'version'], 'node': ['node', '--version'], 'npm': ['npm', '--version'],
                'python': ['python3', '--version'], 'gcc': ['gcc', '--version'],
                'make': ['make', '--version'], 'git': ['git', '--version']}
    for tool, argv in versions.items():
        if command(argv).splitlines()[0] != p['versions'][tool]:
            raise ValueError('tool version mismatch: ' + tool)
    for name, bound in [('gomod', p['max_dependency_bytes']), ('node_modules', p['max_dependency_bytes']), ('cache', p['max_seed_bytes'])]:
        actual = tree(ROOT / name, bound)
        if hashlib.sha256(canonical(actual)).hexdigest() != p['tree_hashes'][name]:
            raise ValueError('immutable dependency/cache input mismatch: ' + name)
    return p

def source_locks(source, p):
    for name, expected in p['source_hashes'].items():
        path = source / name
        if path.is_symlink() or digest(path) != expected:
            raise ValueError('source lock/entrypoint mismatch: ' + name)

def environment():
    return {'PATH': '/opt/node/bin:/usr/local/go/bin:/usr/bin:/bin', 'HOME': '/tmp',
            'GOENV': 'off', 'GOWORK': 'off', 'GOTOOLCHAIN': 'local', 'GOPROXY': 'off',
            'GOSUMDB': 'off', 'GOFLAGS': '-mod=readonly -count=1', 'GOCACHE': '/work/cache',
            'GOMODCACHE': '/work/gomod', 'PYTHONDONTWRITEBYTECODE': '1', 'LANG': 'C.UTF-8',
            'npm_config_offline': 'true', 'npm_config_ignore_scripts': 'true', 'npm_config_cache': '/work/npm-cache'}

def main():
    if len(sys.argv) != 8 or sys.argv[1] not in ('check', 'build') or sys.argv[2] not in ('cold', 'seeded'):
        raise ValueError('fixed profile invocation required')
    phase, mode, expected = sys.argv[1:4]
    started = time.monotonic()
    p = verify_profile(expected, sys.argv[4:8])
    source = pathlib.Path('/work/source')
    if phase == 'check':
        source_locks(pathlib.Path('/source'), p)
        if source.exists() or pathlib.Path('/work/cache').exists():
            raise ValueError('private attempt work area already exists')
        shutil.copytree('/source', source, symlinks=True)
        if (source / 'node_modules').exists():
            raise ValueError('source cannot supply dependencies')
        shutil.copytree(ROOT / 'node_modules', source / 'node_modules', symlinks=True)
        shutil.copytree(ROOT / 'gomod', '/work/gomod', symlinks=True)
        if mode == 'seeded':
            shutil.copytree(ROOT / 'cache', '/work/cache')
        else:
            pathlib.Path('/work/cache').mkdir()
        env = environment()
        env['CGO_ENABLED'] = '1'
        subprocess.run(['make', 'check'], cwd=source, env=env, check=True)
        pathlib.Path('/work/profile-ready.json').write_bytes(canonical({'profile': expected, 'mode': mode}))
    else:
        source_locks(source, p)
        if json.loads(pathlib.Path('/work/profile-ready.json').read_text()) != {'profile': expected, 'mode': mode}:
            raise ValueError('successful matching source checks required')
        env = environment()
        env['CGO_ENABLED'] = '0'
        subprocess.run(['make', 'build'], cwd=source, env=env, check=True)
        output = source / 'bin/release-control'
        if output.is_symlink() or output.read_bytes()[:4] != b'\x7fELF':
            raise ValueError('release executable missing')
        shutil.copyfile(output, '/output/release-control')
        os.chmod('/output/release-control', 0o555)
    print(json.dumps({'profile_phase': phase, 'cache_mode': mode, 'elapsed_seconds': round(time.monotonic()-started, 3)}), flush=True)

if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print('operator profile failed closed: ' + str(error), file=sys.stderr)
        sys.exit(1)
