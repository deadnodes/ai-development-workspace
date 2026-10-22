# Inactive operator preparation only. Worker never interprets this Dockerfile.
FROM golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS prepare
ENV PATH=/opt/node/bin:/usr/local/go/bin:/usr/bin:/bin GOENV=off GOWORK=off GOTOOLCHAIN=local
COPY node.tar.xz /tmp/node.tar.xz
RUN echo '08bfbf538bad0e8cbb0269f0173cca28d705874a67a22f60b57d99dc99e30050  /tmp/node.tar.xz' | sha256sum -c - && python3 -c 'import tarfile;tarfile.open("/tmp/node.tar.xz","r:xz").extractall("/opt")' && mv /opt/node-v22.14.0-linux-arm64 /opt/node && rm /tmp/node.tar.xz
COPY inputs.json profile.py finalize.py runtime.py /opt/rcp/
COPY source.tar /tmp/source.tar
RUN mkdir /seed && tar -xf /tmp/source.tar -C /seed && rm /tmp/source.tar
WORKDIR /seed
# This is the only networked preparation phase; unchanged repository locks bind
# downloaded module/npm bytes. No service/provider credentials are available.
RUN GOMODCACHE=/opt/rcp/gomod GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org go mod download && GOMODCACHE=/opt/rcp/gomod go mod verify && npm ci --ignore-scripts --no-audit --no-fund --registry=https://registry.npmjs.org && mv node_modules /opt/rcp/node_modules && ln -s /opt/rcp/node_modules node_modules && python3 -c 'import sys,json,pathlib;sys.path.insert(0,"/opt/rcp");import profile;profile.source_locks(pathlib.Path("/seed"),json.loads(pathlib.Path("/opt/rcp/inputs.json").read_text()))'
# Compiler seed is produced offline from the declared immutable source scope.
RUN --network=none GOMAXPROCS=4 GOMODCACHE=/opt/rcp/gomod GOCACHE=/opt/rcp/cache GOPROXY=off GOSUMDB=off GOFLAGS='-mod=readonly -count=1 -p=4' CGO_ENABLED=1 PYTHONDONTWRITEBYTECODE=1 make check && GOMAXPROCS=4 GOMODCACHE=/opt/rcp/gomod GOCACHE=/opt/rcp/cache GOPROXY=off GOSUMDB=off GOFLAGS='-mod=readonly -count=1 -p=4' CGO_ENABLED=0 make build
RUN chmod -R a+rX /opt/rcp /opt/node && python3 /opt/rcp/runtime.py && python3 /opt/rcp/finalize.py && chmod 0555 /opt/rcp/profile.py
FROM golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b
ENV PATH=/opt/node/bin:/usr/local/go/bin:/usr/bin:/bin GOENV=off GOWORK=off GOTOOLCHAIN=local
COPY --from=prepare /opt/node /opt/node
COPY --from=prepare /opt/rcp /opt/rcp
COPY --from=prepare /runtime-tree /runtime-tree
# Runtime tar is normalized/verified by the operator CLI in the next local-only
# packaging stage. This image has no source checkout, .git, old RCP binary or data.
