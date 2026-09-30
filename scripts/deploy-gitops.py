#!/usr/bin/env python3
"""Publish one verified CI image to an explicitly selected GitOps Deployment.

Uses the operator/CI's existing gh authentication. Credentials are never written
by this script. The target must already exist and have exactly one image field.
"""
import argparse
import base64
import json
import pathlib
import re
import subprocess
import tempfile
import urllib.parse


def gh(*args, payload=None):
    command = ['gh', *args]
    if payload is not None:
        command += ['--input', '-']
    result = subprocess.run(command, input=None if payload is None else json.dumps(payload),
                            text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError('GitHub request failed: ' + result.stderr.strip())
    return json.loads(result.stdout) if result.stdout.strip() else None


def patch_manifest(content, image):
    if not re.fullmatch(r'[a-z0-9][a-z0-9./_-]*@sha256:[a-f0-9]{64}', image):
        raise ValueError('An immutable lowercase image digest is required')
    if not re.search(r'^kind: Deployment\s*$', content, re.M):
        raise ValueError('Only an existing Deployment manifest is supported')
    matches = list(re.finditer(r'^(\s+image: )\S+\s*$', content, re.M))
    if len(matches) != 1:
        raise ValueError('Expected exactly one container image field')
    match = matches[0]
    content = content[:match.start()] + match.group(1) + image + '\n' + content[match.end():].lstrip('\n')
    # Replica activation is an explicit deployment effect, never a cluster patch.
    if len(re.findall(r'^  replicas: [0-9]+$', content, re.M)) != 1:
        raise ValueError('Expected an explicit Deployment replica count')
    return re.sub(r'^  replicas: [0-9]+$', '  replicas: 1', content, count=1, flags=re.M)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-repository', required=True)
    parser.add_argument('--run-id', required=True, type=int)
    parser.add_argument('--gitops-repository', required=True)
    parser.add_argument('--gitops-branch', required=True)
    parser.add_argument('--manifest', required=True)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    if any(not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', r) for r in
           [args.source_repository, args.gitops_repository]):
        parser.error('Invalid repository')
    path = pathlib.PurePosixPath(args.manifest)
    if path.is_absolute() or '..' in path.parts or not str(path).endswith(('.yaml', '.yml')):
        parser.error('Invalid manifest path')
    source = gh('api', f'repos/{args.source_repository}')
    run = gh('api', f'repos/{args.source_repository}/actions/runs/{args.run_id}')
    if (run['conclusion'] != 'success' or run['status'] != 'completed' or
        run['head_repository']['full_name'].lower() != args.source_repository.lower() or
        run['head_branch'] != source['default_branch'] or
        run['path'].split('@')[0] != '.github/workflows/ci.yml' or
        run['event'] not in ('push', 'workflow_dispatch')):
        raise ValueError('Require a successful owning-repository default-branch Check workflow')
    with tempfile.TemporaryDirectory(prefix='rcp-image-evidence-') as folder:
        subprocess.run(['gh', 'run', 'download', str(args.run_id), '--repo', args.source_repository,
                        '--name', 'rcp-image-evidence', '--dir', folder], check=True)
        evidence = json.loads((pathlib.Path(folder) / 'image.json').read_text())
    if (evidence['source_sha'] != run['head_sha'] or
        evidence['repository'].lower() != args.source_repository.lower() or
        str(evidence['run_id']) != str(args.run_id)):
        raise ValueError('Build evidence does not match the verified run')
    expected_image = 'ghcr.io/' + args.source_repository.lower()
    if evidence['image_repository'] != expected_image:
        raise ValueError('Unexpected image registry/repository')
    image = expected_image + '@' + evidence['digest']
    ref = urllib.parse.quote(args.gitops_branch, safe='')
    endpoint = f'repos/{args.gitops_repository}/contents/{urllib.parse.quote(str(path), safe="/")}'
    before = gh('api', endpoint + '?ref=' + ref)
    if before.get('type') != 'file':
        raise ValueError('Target must be an existing regular GitOps file')
    content = base64.b64decode(before['content']).decode()
    updated = patch_manifest(content, image)
    report = dict(source_sha=run['head_sha'], run_id=args.run_id, image=image,
                  target=args.gitops_repository, branch=args.gitops_branch, manifest=str(path), applied=False)
    if args.apply and updated != content:
        result = gh('api', '--method', 'PUT', endpoint, payload={
            'message': f'RCP: обновлён dev-образ {run["head_sha"][:12]}',
            'branch': args.gitops_branch, 'sha': before['sha'],
            'content': base64.b64encode(updated.encode()).decode()})
        report.update(applied=True, gitops_commit=result['commit']['sha'])
    elif args.apply:
        report.update(applied=True, unchanged=True)
    print(json.dumps(report))


if __name__ == '__main__':
    main()
