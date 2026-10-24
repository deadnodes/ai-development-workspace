# Offline RCP application fixture

The inactive operator profile in `examples/workers/rcp-toolchain` builds the real
RCP application through the existing `make check` and `make build` entrypoints.
It is a local acceptance fixture, not an installed RCP release or a live delivery
configuration. Native production candidates and protected main advancement stay
blocked. See [native delivery](NATIVE_PUBLICATION.md) for the operation graph and
[local execution](LOCAL_WORKER.md) for isolation and failure semantics.

## Immutable preparation

`inputs.json` pins the public Go base image, official Node archive SHA256,
tool versions, repository lock/Makefile hashes, native platform, and dependency
scope. The initial compiler seed is prepared from source commit
`fdfcafb63a1f4433e8d62888553a50d46d951245`. This seed commit is distinct from
each worker job's exact source commit and from the final feature implementation
commit. Changing an embedded application asset does not change dependency locks.

Explicit operator preparation uses only an existing checkout:

```sh
python3 examples/workers/rcp-toolchain/prepare.py \
  /absolute/path/to/existing/ai-development-workspace \
  /absolute/path/to/new-private-preparation-directory
```

Preparation downloads the exact official Node bytes and the dependencies already
declared by the unchanged Go/npm locks. It verifies the locks after download,
runs checks and release builds offline, and records complete dependency/cache
tree hashes. No source checkout, `.git`, host home, host cache, Docker socket,
control token or application state enters the final toolchain. `ownership.json`
records exact fixture container and image identities. The temporary local
registry supplies a digest reference; its exact owned container is removed.
The digest remains cached locally, so this is cohost preparation rather than
distribution to a remote worker fleet. Images are not garbage collected.

The fixed profile verifies its own manifest hash, Product/repository/trust/
platform scope, tool versions, source locks and every immutable dependency and
compiler-seed file before use. The operator recipe's immutable image and argv
are bound into the existing recipe/job hashes. No caller selects a shell or
dependency source. Each attempt receives private source, module and compiler
cache copies inside its bounded work area. `GOENV`/`GOWORK` are disabled,
`GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, module writes are forbidden,
and npm is offline. Missing or changed inputs fail before checks/build success.

The `seeded` and `cold` argv variants are separate explicit operator recipes.
They use the same immutable input manifests; only the private Go compilation
cache starts populated or empty. There is no shared writable cache, cache export
or eviction policy. Measurements include checks, compilation, OCI validation
and durable spool export, and impose no speed threshold.

## Runtime bundle

Preparation selects public Git, its HTTPS helper, public CA roots and their ELF
library dependencies from the pinned public base. It records their public input
provenance in `usr/share/rcp-runtime-inputs.json`. It excludes the old application
binary, keys, state and credentials. `rcp-worker prepare-rootfs` normalizes this
selected tree to a bounded deterministic regular-file/directory archive. Only
links proven inside the selected tree may be flattened during preparation;
runtime archives themselves cannot contain links or special files.

The recipe pins the typed archive identity/bounds/tree hash and the immutable
toolchain supplies it at `/opt/rcp/runtime-rootfs.tar`. The supervisor verifies
and merges it with the newly compiled binary. BuildKit receives a generated
scratch image and one local archive, with networking disabled. Admission checks
the exact complete filesystem, modes, entrypoint, PATH and nonroot identity.
The bundle is optional; existing scratch-only recipes retain their behavior.

## Reproducible local acceptance

With the existing isolated PostgreSQL fixture available, run:

```sh
RCP_TEST_DOCKER=1 \
RCP_TEST_REALAPP_CONFIG=/absolute/private/preparation/acceptance.json \
TEST_DATABASE_URL='postgres://fixture-user:fixture-password@127.0.0.1:fixture-port/fixture-db?sslmode=disable' \
go test -v -count=1 -timeout=40m ./internal/transport \
  -run '^TestRealApplicationOfflineNativeDelivery$'
```

The test creates protected API intent and lets the ordinary Tick/worker/job/
publisher/spool/admission/GitOps PR path carry four independent deliveries:
same-source seeded builds, an actual embedded-source change, and a same-source
cold control. It compares immutable output manifests, exact source/runtime
provenance and elapsed times. Worker/publisher configs persist between the
seeded attempts; there is no manual job queue or archive copy. Provider/PR
behavior is a local fixture, not evidence of a live GitHub check bridge.

The admitted changed-source image runs by digest with an ephemeral fake token,
temporary bbolt/workspace files and no real database. Runtime checks exercise
health, API authentication, Product creation/readback, Feature creation/update/
readback, supported Product deletion with Feature cascade/absence, the changed embedded
asset and real Git workspace scanning. A separate private TLS fixture exercises
the actual Go GitHub App provider: default public roots and unrelated fixture
roots reject its certificate; a private per-process certificate-file augmentation
accepts it. The image's original public CA bytes remain unchanged. On OrbStack,
the fixture sets its documented per-container
[`dev.orbstack.add-ca-certificates=false`](https://docs.orbstack.dev/features/https)
label: otherwise the engine changes CA files at container start even with a
read-only root filesystem. No global engine settings change. Negative TLS checks
require the exact sanitized provider error and a certificate-rejection TLS alert
at the private server; unrelated HTTP errors cannot satisfy them. Fake private
keys never enter the normal toolchain, runtime bundle or source tree.

This baseline has no `update_product` or standalone `delete_feature` command;
the fixture exercises the supported API operations without adding those features.
`TestRealApplicationAdmittedRuntime` is an optional separate smoke for a
previously admitted cached local digest. It asserts runtime behavior only and
does not replace full graph/admission acceptance.

`TestRealApplicationOperatorProfileGuards` runs ordinary Python input-guard tests
as part of the Go suite. Runtime parser/OCI/admission tests separately cover
malformed bundles, quotas, collisions and identity mismatches. Existing executor
failure/cancellation tests still apply to this same executor. The source build's
optional PostgreSQL, Docker and provider tests are honestly skipped inside the
networkless compiler; the host required `make check` runs PostgreSQL/Docker
fixtures separately. Set `RCP_TEST_REALAPP_CONFIG` only for the explicit heavy
fixture, not for recursive compiler checks.

This profile is native Linux arm64 on the available local Docker host. Native
x86 validation, fleet transport/recovery, retention, the live provider check
bridge, full composition/conflict acceptance and the A+B Dev/selective Prod pilot
remain separate required work. Nothing here adopts a new real RCP installation,
changes authentication, merges a protected ref or proves a live rollout.
