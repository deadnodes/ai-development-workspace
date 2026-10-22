# Native publication and existing-artifact GitOps PRs

This source slice joins the authenticated offline build protocol to the separate
publisher and server-side artifact admission. It does not install a registry or
replace the automatic GitHub build graph. The server never executes application
commands. Native binaries remain external executors.

## Operator configuration

`RCP_WORKER_CONFIG` adds immutable `publications` entries to the existing worker
configuration. Each `workerconfig.PublicationRecipe` pins ID/revision/hash,
Product, source repository, component, build profile, accepted build recipe hash,
exact registry origin/repository, byte/time bounds and read-only registry settings.
`workerconfig.PublicationHash` computes the hash with the hash field cleared.
The configuration is startup-only; editing a file is not a hot reload.

`pull` contains `ca_file`, separate `username_env`/`password_env` or `bearer_env`,
and an explicitly named `fixture_loopback_http` exception. Production origins
require HTTPS; plain HTTP is permitted only for numeric loopback fixtures.
The server performs authenticated GETs only, with no ambient proxy or redirects,
TLS >= 1.2, bounded bodies and an exact origin/repository. Pull credentials stay
outside persisted jobs, leases and audit. Worker tokens cannot equal the control
credential or a configured registry credential. Source/build and publication
capabilities cannot share a credential or principal.

Configure the existing component using `configure_component` with
`mode: NATIVE_EXISTING_ARTIFACT`, `profile`, `application_id`, `connection_id` and
exact `image_repository` (`host[:port]/repository`, no scheme). The operator
publication recipe must authorize this component/profile/image. Source GitHub
and GitOps bindings remain required. Workflow fields, extra inputs and
`rebuild_missing` are forbidden in native mode. The target must use
`gitops_mode: PR`; DIRECT is rejected for native delivery.

## Queue, execute, admit

1. Finish `queue_worker_job` / `local_oci_build` through the existing authenticated
   worker protocol. Its successful report retains a verified local OCI receipt;
   this is not yet a registry artifact.
2. Call `queue_worker_publication` with Product ID and data `build_job_id`,
   `component_id`, `recipe_id`, `recipe_revision`, `recipe_hash`, `config_revision`,
   `request_key` (16–128 identifier characters) and `deadline` (within one hour).
   Product/Integration/repository/source/receipt are derived from the successful
   build. Callers cannot supply a replacement source, digest or registry target.
   An identical request replay returns the same job; a conflicting key is rejected.
3. Run `rcp-publisher run-once /absolute/operator-config.json`. Its configuration
   is `publishrunner.Config`: endpoint, worker `token_env`, config revision,
   the exact nonsecret publication recipe and `publisher` settings containing
   operator-owned `archive`, private existing `state_dir`, exact allowed target,
   byte/time bounds, CA and separate push credential environment references.
   The registry plan is derived from the authenticated lease. No server-supplied
   local path is accepted. The original `publisher publish` operator CLI remains
   available and continues to make only a local registry-byte observation.
4. The publisher claims only its pinned recipe, durably journals its lease before
   START, heartbeats during publishing and report verification, and persists a
   pending result before REPORT. A lost START/REPORT response is retried using the
   same job/attempt/input. Restart can reobserve/reconcile the same digest intent;
   it never rebuilds. Completed journals are retained before the same profile
   claims another job. A single operator archive path is supported: its bytes must
   match each claimed receipt before network writes; fleet artifact transfer is
   separate future work.
5. The server independently GETs and hashes the manifest, config and layer, then
   verifies the one-image/one-layer closure, platform and rootfs diffID. Network
   I/O happens outside the store transaction. Current credential identity/grants,
   expiry/rotation, fence, config, cancellation, lease and deadline are checked
   again atomically before admission. Only then does the job become SUCCEEDED
   and one `NATIVE_WORKER` DeliveryArtifact appear. Its BuildRunID is zero; there
   is no synthetic Actions run or GitHub report. Replays do not duplicate it.

The original archive hash remains the authenticated build receipt assertion.
Registry pull verifies the manifest-addressed closure; it cannot reconstruct or
prove byte equality of the original tar serialization. Server observation records
manifest/config/layer digests, platform, byte count and time separately.

Read one job through MCP `get_worker_job {job_id}` or
`GET /api/worker-jobs/{id}`. These expose bounded job/attempt history and do not
require a global state download.

## Reconciliation, fencing and restore

Ordinary RUNNING lease expiry becomes RECONCILIATION_REQUIRED. Explicit
`reconcile_worker_publication` with Product/job ID and empty data reads only the
same frozen digest. It can admit a server-observed artifact while retaining the
original EXPIRED attempt and absent worker report, with a separate typed
reconciliation record. Identical successful reconciliation replay is idempotent.
Expired worker reports remain invalid. Cancellation, deadline/config drift,
UNKNOWN registry state or restore quarantine prevents successful reconciliation.
Existing failure/cancel closure remains available.

A lease cannot retract bytes already sent in an immutable registry HTTP write.
Fencing prevents admission and downstream desired state changes. It does not
claim physical reversal of an in-flight remote byte write.

Every restored Product receives a persistent native quarantine. Every imported
nonterminal worker job retains cancellation and requires reconciliation. A restored
copy cannot queue/claim/start/heartbeat/report native builds or publications,
configure native delivery or prepare GitOps from old successful native artifacts.
History and legacy GitHub paths remain available. Quarantine survives reopen and
further backups. There is **no clearing or ownership-transfer action in this
slice**. Deleting a quarantined Product is rejected, so deletion and reuse of its
ID cannot bypass quarantine. Deletion requires a future owner-approved
transfer/recovery design; no such transfer is implemented here.
An owner-approved transfer protocol is required before recovery/live
adoption; ordinary reconciliation cannot impersonate such a transfer.

## Prepare a GitOps PR

Use the existing `deploy_existing_artifact` command with exact Integration,
component, revision, DEV target and artifact ID. It validates typed native
provenance/profile, preserves source HEAD/base and GitOps permission/CAS checks,
performs a fresh independent registry inspection and prepares the existing GitOps
PR. It waits at WAIT_GITOPS_PR for external review/merge. A PR is not a deployment
or runtime-health observation. Native `deploy_integration`, composition/candidate
build graphs, missing-artifact rebuilds, promotion dispatch and DIRECT mapping
fail closed; none can fall back to GitHub Actions. GitHub components retain their
existing workflow evidence checks.

## Verification boundary

`RCP_TEST_DOCKER=1 TEST_DATABASE_URL=<isolated fixture DSN> go test -race -count=1
-v ./...` includes the protected ephemeral API → actual worker subprocess →
separate publisher subprocess → pinned Distribution → server verification →
GitOps PR provider fixture chain on local and actual PostgreSQL stores. The test
owns a labelled exact-ID loopback tmpfs Distribution container and cleans it up;
Docker context `orbstack` and the already cached arm64 compiler/BuildKit pins are
fixture-specific. It tests lost START/REPORT, restart and sequential jobs without
Actions/direct writes. Fast tests cover post-I/O rotation/fence/cancel/config/
lease/deadline changes, corrupt/missing/401 bytes, reopen, backup quarantine and
ordinary reconciliation. `make check` runs repository checks and all three builds.

This is native arm64 fixture evidence, not Linux x86, live registry storage/TLS/
backup/Dev-failure acceptance, production ownership transfer, fleet archive
transport, automatic native build graph, retention/GC or complete RCP-011 delivery.
