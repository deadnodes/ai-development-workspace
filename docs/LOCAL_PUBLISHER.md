# Local OCI publisher

`cmd/publisher` is a separate external executor. It publishes an operator-approved
local OCI image to one explicitly allowlisted Distribution repository. The
worker receives no registry credentials. Build it with `make build`, then run
`bin/rcp-publisher publish /absolute/path/operator-publication.json`.

Copy the inactive [configuration example](../examples/publishers/local.json),
replace every placeholder, insert the exact `local_oci` receipt returned by the
worker, and review the complete plan before setting `approved: true`. Use an
absolute archive path and an existing private (0700), canonical absolute state
directory without symlink ancestors (`pwd -P`). Do not put credentials in the
configuration: the publisher reads only the named publisher environment variables.
Protect the operator configuration from untrusted source changes. The allowlist
and plan are operator-owned; a worker receipt cannot select a registry target.

The plan binds Product, Component, Repository, source SHA, job, attempt, input
hash, recipe/config/toolchain, platform, archive hash, layout/index/manifest
identity and exact registry origin/repository. Changing any semantic plan field
under the same ID conflicts with its durable intent. Archive path, CA path,
credential environment names/values and operational timeout are not plan identity.
The receipt is an operator-approved assertion, not cryptographic worker
attestation. This CLI does not query RCP to establish ownership or active fences.

The publisher reads one bounded archive snapshot, runs the worker's existing OCI
closure/platform/diffID verifier over those bytes, compares every receipt field,
and uploads precisely those immutable bytes. No archive extraction or mutable
file reopen occurs after verification. It accepts the worker's one-image,
one-layer Linux amd64/arm64 profile. Archive and expansion bounds are at most
256 MiB; the operator normally sets a smaller value. The whole HTTP sequence has
an operator timeout of 1–600 seconds, bounded responses and headers, and no
ambient proxy support.

Before any network I/O it obtains a nonblocking exclusive filesystem lock and
fsyncs immutable intent (plan hash) plus the containing directory. Keep both
`ID.lock` and `ID.intent`: they contain no credentials, registry upload URLs or
raw payloads. Restart by supplying the same approved plan and archive. An intent
is a write-ahead identity binding, not a saved upload session or success cache.
Concurrent invocations and changed plans fail closed. Protect the private state
directory from other writers and retain it across restarts.

Every invocation GETs and hashes actual registry manifest and closure bytes,
even after an earlier success. Missing blobs use POST upload initiation followed
by PUT finalization with a content digest; the manifest is PUT by digest only.
A lost final response returns UNKNOWN; the next invocation reconciles that same
digest and avoids another manifest commit when it already exists. Replays do
not promise exactly-once HTTP writes across arbitrary registry failures. No
mutable tags, deletion, GC, Git, Flux or deployment operations exist. A manifest
whose closure is missing or corrupt fails closed rather than being overwritten.
A failed pre-manifest attempt may leave unreferenced blobs/upload sessions in
the registry; registry retention/GC is an operator concern outside this CLI.

HTTPS requires TLS 1.2 or newer, system trust or an explicit custom CA bundle.
Redirects and token challenge discovery are rejected. Configure either a
preprovisioned bearer token environment name or Basic username/password
environment names; scopes must be restricted externally to the exact repository.
No auth fallback occurs. Upload Location must retain the exact origin and
repository upload prefix. Only the registry's opaque `_state` query is accepted
and retained transiently. Errors omit credentials, remote bodies and URL queries.
Numeric loopback HTTP requires `fixture_loopback_http: true`; it is solely for
explicit isolated local tests. Remote plain HTTP and hostname `localhost` HTTP
are rejected.

A successful stdout JSON record has kind `PUBLISHER_LOCAL_REGISTRY_BYTES` and
includes plan hash, complete approved provenance, manifest digest and observation
time. It means these registry bytes were read and verified during this local
invocation. It is **not** authenticated RCP acceptance, active-attempt fencing,
DeliveryArtifact PRESENT, a numeric GitHub Actions identity, or deployment
eligibility. The separate [`run-once` native path](NATIVE_PUBLICATION.md) uses
RCP leases and server-side admission; this standalone `publish` mode does not.
Live registry provisioning, real credentials, retention policy, runtime consumers
and deployment acceptance remain separate work.

## Verification

`go test -race ./internal/ocipublisher` covers receipt/archive corruption before
network access, immutable snapshot substitution, lost manifest response, replay,
complete repeated GET/hash, remote corruption, TLS trust, bearer auth, rejected
token challenges, redirects, hostile upload locations, proxy isolation, bounded
responses, conflicting journal identity, concurrency and symlinks.

Actual Distribution tests are explicit opt-ins. Use only an operator-owned
throwaway registry with pinned image
`registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e`
(Distribution 3.0.0), loopback random port and bounded tmpfs storage. Keep its exact
container ID and stop only that ID after probes. No shared daemon changes or
bulk cleanup are required.

First export an actual worker fixture into a **new, absent** absolute directory:

```sh
RCP_TEST_DOCKER=1 RCP_TEST_OCI_EXPORT=/tmp/unique-worker-export \
  go test -run '^TestRealWorkerTwoCleanBuilds$' -v ./internal/workerrunner
```

The worker test uses its pinned cached compiler/BuildKit images and native arm64
fixture. Then run the separate CLI against the owned HTTP registry:

```sh
RCP_TEST_REGISTRY_ORIGIN=http://127.0.0.1:PORT \
RCP_TEST_OCI_EXPORT=/tmp/unique-worker-export \
  go test -race -run '^TestRealDistribution$' -v ./internal/ocipublisher
```

That test builds the actual CLI, drops the successful manifest commit response
at a local fault proxy, restarts the CLI, pulls/hashes the complete real registry
closure and repeats it; the proxy requires exactly one observed manifest PUT.
Use a fresh empty registry for each run.

For a separate owned Distribution configured with a loopback IP certificate,
custom CA and htpasswd Basic auth (`publisher-fixture` / fixture-only password
`fixture-only-password`), run:

```sh
RCP_TEST_TLS_REGISTRY_ORIGIN=https://127.0.0.1:PORT \
RCP_TEST_TLS_REGISTRY_CA=/absolute/path/fixture-ca.pem \
RCP_TEST_OCI_EXPORT=/tmp/unique-worker-export \
  go test -race -run '^TestRealDistributionTLSAuth$' -v ./internal/ocipublisher
```

It checks real TLS/custom trust, authenticated publish/re-observation, and wrong
credentials failing UNKNOWN. These tests do not prove x86 builds, live registry
availability, RCP artifact admission or deployment acceptance.
