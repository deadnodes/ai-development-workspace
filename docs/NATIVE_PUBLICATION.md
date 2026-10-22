# Native build, publication and GitOps PR delivery

Native binaries are external executors. The control plane stores immutable intent,
source/recipe identities, jobs, receipts and artifact provenance; it never runs
application commands or stores image bytes. The existing DEPLOY and COMPOSE
operations own delivery. GitHub Actions components retain their existing path.

## Operator authority and component binding

`RCP_WORKER_CONFIG` is immutable startup authority. Its build recipes pin source
repository/URL, ID/revision/hash, toolchain/BuildKit images, literal argv, isolation,
resource bounds and typed nonsecret parameters. Publication recipes pin Product,
component, repository, profile, accepted build recipe hash, exact registry
origin/repository, byte/time bounds and separate read credential references.
`workerconfig.RecipeHash` and `PublicationHash` hash the recipe with `hash` cleared.
The configuration has one revision; editing a file is not a hot reload.

Configure a component with `mode: NATIVE`, profile, source connection and exact
`image_repository` (`host[:port]/repository`, no scheme). The operator publication
recipe must authorize that component/profile/image. Workflow fields, workflow
inputs and `rebuild_missing` are forbidden. There is one native mode and no
prototype alias. Without a `native` binding, the component supports only
`deploy_existing_artifact`.

An explicit automatic binding selects operator recipes and typed values only:

```json
{
  "application_id": "component",
  "connection_id": "source-connection",
  "mode": "NATIVE",
  "profile": "native-dev",
  "image_repository": "registry.example/team/component",
  "native": {
    "build_recipe_id": "component-build",
    "publication_recipe_id": "component-publish",
    "parameters": {}
  }
}
```

Recipe IDs/revisions/hashes, config revision, profile, source identity and image
pins are derived and revalidated by the server. Callers cannot supply recipe
bodies, shell, credential values or executor paths. All native delivery requires
an explicitly enabled DEV/TEST mapping using `gitops_mode: PR`; DIRECT is rejected.
Native release candidates, main advancement and promotion fail closed, including
resumed operations and mixed Actions/native selections before protected writes.

## Automatic delivery

Call `deploy_integration` with the normal integration, DEV environment, optional
component/revision and optional `request_key` (16–128 identifier characters).
Identical keyed retries return the same DEPLOY and persisted absolute deadline;
a conflicting payload is rejected. The server verifies source HEAD/base and
GitOps CAS before queueing the exact frozen-source build. One bounded deadline
is frozen once, with no automatic extension or replacement execution.

Tick creates a managed WorkerJob, waits for its accepted local OCI receipt,
creates a separate publication job, then selects the server-admitted artifact on
that same DEPLOY. Deterministic parent/job links and transitions persist atomically.
Repeated ticks, concurrent workers and reopen do not create another build or
publication. Failed or unknown execution never falls back to Actions, registry
tags or a different artifact. Unknown RUNNING build execution requires explicit
reconciliation and is never executed again automatically.

Native DEV/TEST COMPOSE children use this same path. A server-derived source
proof identifies the immutable DEPLOY/COMPOSE snapshots and generated source
revision. Base-only compositions need no fabricated IntegrationID. Public queue
commands cannot inject source proofs or publish managed builds separately.
COMPOSE persists each component's resolved authority and absolute deadline at
request time, before assembling source. Restart validates every frozen component
before source or child execution; children reuse that authority without refreezing.
Completed artifact reuse validates immutable origin separately from the former
parent's live execution status.

## Durable archive transport and publisher

Both supervisors use operator-configured `artifact_spool` settings. The trusted
build supervisor validates OCI output, durably exports an immutable
content-addressed archive, and only then journals/reports PASSED. Source containers
cannot access the spool or control/registry credentials. RCP stores archive hash,
byte count and receipt, never spool paths or bytes.

The POSIX spool is a private, existing canonical directory with per-archive,
total-byte and entry quotas, locking, bounded copy, no symlink/path escape,
no-overwrite publication and file/directory fsync, including existing-entry
replay. Cleanup affects only the invocation's own staging file; historical
archives are retained. Publisher configuration uses the same archive byte bound
and a read-only spool view. It derives each filename solely from the validated
receipt archive hash and verifies a bounded immutable snapshot before publication.
Two changed-source jobs can use unchanged persistent worker/publisher configs
without archive copying. This supports cohost/shared-volume transport; it is not
remote object storage or fleet transport. See [worker](LOCAL_WORKER.md) and
[publisher](LOCAL_PUBLISHER.md) configuration and examples.

The publisher journals its lease before START and a durable PUBLISH_INTENT before
any registry publication. A lost START response before that intent may retry the
same idempotent START, then publish for the first time. Restart with an unresolved
PUBLISH_INTENT performs only exact registry GET/hash observation; it never repeats
POST/PUT. Complete matching bytes and a current lease allow persistence/reporting
of that same result. Missing, corrupt, UNKNOWN, 401 or expired authority remains
uncertain and requires explicit reconciliation. Known durable reports replay the
same job/attempt/result. Completed journals remain in history. The standalone
operator `publisher publish` CLI still accepts a local archive and retains its
explicit operator purpose.

## Admission, cancellation and reconciliation

The server independently GETs/hashes manifest, config and layer, verifying the
one-image/one-layer closure, platform and rootfs diffID. HTTPS is required except
explicit numeric-loopback fixtures. HTTP clients use bounded bodies, exact
origin/repository, no ambient proxy/redirects and TLS >=1.2. Pull credentials and
worker/control/publisher credentials remain separate and outside durable state.
Registry and source provider I/O run outside the store transaction. Current
credentials/grants, recipe/config, cancellation, parent/supersession, target,
fence, lease and deadline are checked before execution and atomically after I/O.
Only then does one `NATIVE_WORKER` DeliveryArtifact appear. BuildRunID is zero;
there is no synthetic Actions run or workflow report. The archive hash describes
the build tar; registry verification proves the manifest-addressed closure, not
byte equality of that tar's serialization.

`cancel_native_delivery` takes Product ID, DEPLOY ID and empty data. Before
external PR intent it atomically stops the delivery and its managed jobs.
Cancelling a managed worker job also stops its delivery. Running work remains
RECONCILIATION_REQUIRED; cancellation makes no claim of physical registry reversal
or cleanup. Cancellation is rejected after APPLY_UNCERTAIN/PR/result; it never
closes or merges a PR. Reads expose state through `get_worker_job {job_id}` or
`GET /api/worker-jobs/{id}` without a global state download.

Ordinary RUNNING publication lease expiry becomes RECONCILIATION_REQUIRED.
Explicit `reconcile_worker_publication` with Product/job ID and empty data reads
only the frozen digest and can admit its exact verified bytes. The original
attempt remains EXPIRED with nil report and a separate server reconciliation
proof. This does not rebuild, replace the job or invent worker success. Native
source/target/authority drift, cancellation, deadline or quarantine blocks it.

`deploy_existing_artifact` remains exact artifact-only delivery and never builds,
even for an automatically bound component. Fresh registry inspection and source/
GitOps CAS lead to the existing WAIT_GITOPS_PR phase. Review/merge is external;
a PR or GitOps merge is not runtime-health evidence.

Restore quarantines every imported Product permanently in this slice. Imported
active jobs require reconciliation, but the restored instance cannot queue,
claim, start, heartbeat, report, reconcile/admit or prepare native delivery from
old artifacts. Deletion/reuse cannot bypass quarantine. Ordinary reopen of the
original instance remains supported. There is no clearing, ownership transfer,
historical GC or recovery cutover command.

## Verification boundary

`RCP_TEST_DOCKER=1 TEST_DATABASE_URL=<isolated fixture DSN> make check` exercises
repository checks plus the protected ephemeral API → Tick → real worker process
→ durable spool → separate publisher process → pinned Distribution → server
admission → GitOps PR provider fixture on local and actual PostgreSQL stores.
It performs two changed-source deliveries with the same configs and a real
two-component generated-source COMPOSE path, without manual queue/archive copy,
Actions, direct writes or fallback. Exact-ID fixture containers are cleaned up.
Lost START before publication and durable REPORT retry are distinguished from
unknown execution. Deterministic tests cover request replay/conflicts, concurrent
ticks, reopen, cancellation, config/grant/parent/deadline races, explicit expired
publication reconciliation, restore quarantine and all-native/mixed production
rejection before main mutation. Runner/spool tests cover uncertainty, fsync,
quotas, corruption and isolation.

This is native arm64/source acceptance. Linux x86, real application dependencies,
cache policy, live registry TLS/storage/backup/node pulls, ownership transfer,
fleet transport/recovery, retention/GC, full A+B update/conflict semantics,
agent-owned merge/verification, Dev/Prod policy cutover and the complete RCP-011
roadmap remain separate acceptance work. No live infrastructure, credentials,
protected merge or production permission is implied.
