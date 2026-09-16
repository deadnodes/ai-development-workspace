#!/usr/bin/env python3
"""Workflow-only GHCR probe/report. GITHUB_TOKEN never leaves the Actions runner."""
import base64
import datetime
import hashlib
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None

opener = urllib.request.build_opener(NoRedirect())
def request(url, headers):
    try:
        with opener.open(urllib.request.Request(url, headers=headers), timeout=30) as response:
            data = response.read(8 * 1024 * 1024 + 1)
            if len(data) > 8 * 1024 * 1024:
                raise RuntimeError('Registry response too large')
            return response.status, response.headers, data
    except urllib.error.HTTPError as error:
        if error.code == 404:
            return 404, error.headers, b''
        raise RuntimeError(f'Registry request failed: HTTP {error.code}') from None
    except urllib.error.URLError:
        raise RuntimeError('Registry unavailable') from None

def main():
    image = os.environ['IMAGE_REPOSITORY']
    tag = os.environ['IMAGE_TAG']
    sha = os.environ['SOURCE_SHA']
    operation = os.environ['OPERATION_ID']
    if not re.fullmatch(r'ghcr\.io/[a-z0-9][a-z0-9._/-]*', image) or '..' in image:
        raise RuntimeError('Expected lowercase GHCR image repository')
    if not re.fullmatch(r'[0-9a-f]{40}|[0-9a-f]{64}', sha):
        raise RuntimeError('Expected full source SHA')
    if not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}', tag):
        raise RuntimeError('Invalid image tag')
    if not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', operation):
        raise RuntimeError('Invalid operation ID')
    repository = image.removeprefix('ghcr.io/')
    basic = base64.b64encode((os.environ['GITHUB_ACTOR'] + ':' + os.environ['GHCR_TOKEN']).encode()).decode()
    scope = urllib.parse.urlencode({'service': 'ghcr.io', 'scope': f'repository:{repository}:pull'})
    status, _, data = request('https://ghcr.io/token?' + scope, {'Authorization': 'Basic ' + basic})
    if status != 200:
        raise RuntimeError('Registry token unavailable; absence is not inferred')
    token = json.loads(data)['token']
    reference = os.environ.get('EXPECTED_DIGEST') or tag
    if ':' in reference and not re.fullmatch(r'sha256:[0-9a-f]{64}', reference):
        raise RuntimeError('Invalid expected digest')
    status, headers, data = request(f'https://ghcr.io/v2/{repository}/manifests/{reference}', {
        'Authorization': 'Bearer ' + token,
        'Accept': ', '.join(['application/vnd.oci.image.index.v1+json', 'application/vnd.oci.image.manifest.v1+json', 'application/vnd.docker.distribution.manifest.list.v2+json', 'application/vnd.docker.distribution.manifest.v2+json']),
    })
    digest = ''
    if status == 200:
        digest = 'sha256:' + hashlib.sha256(data).hexdigest()
        if headers.get('Docker-Content-Digest') != digest:
            raise RuntimeError('Registry digest does not match manifest bytes')
        if reference.startswith('sha256:') and reference != digest:
            raise RuntimeError('Registry returned a different digest')
    elif status != 404:
        raise RuntimeError('Unexpected manifest response')
    if len(sys.argv) > 1 and sys.argv[1] == 'report':
        if status != 200:
            raise RuntimeError('Built artifact is not observable in registry')
        report = dict(operation_id=operation, source_sha=sha, image_repository=image,
                      image_tag=tag, digest=digest, available=True,
                      observed_at=datetime.datetime.now(datetime.timezone.utc).isoformat())
        with open('result.json', 'w') as file:
            json.dump(report, file)
    else:
        with open(os.environ['GITHUB_OUTPUT'], 'a') as file:
            file.write(f'exists={str(status == 200).lower()}\ndigest={digest}\n')

if __name__ == '__main__':
    main()
