# Trusted offline worker and local OCI builds

`cmd/worker` is an external executor. The RCP server never runs application
commands. GitHub delivery remains unchanged. This implementation verifies source
checks and builds a real local OCI archive; it neither pushes an image nor marks
a registry artifact PRESENT. A receipt is a scoped executor assertion, not an
independently observed registry or deployment attestation.

A separate operator-approved publisher can publish the exact local receipt and
archive; see [Local OCI publisher](LOCAL_PUBLISHER.md). Its byte observation does
not admit an artifact into RCP or authorize deployment. The separate
[native publication path](NATIVE_PUBLICATION.md) binds the accepted build receipt
to a publication job and independent server registry observation.

## First execution contract

Only `trusted_source` recipes execute. The operator pins the complete execution
definition, images, native platform, fixed argv, output contract and resource
limits in the recipe hash. Request parameters are bounded metadata; they never
become shell text or argv. Reuse a project's Make entrypoints when its offline
dependencies are available. The example uses a dependency-free Go fixture rather
than duplicating this repository's checks into a new workflow.

The executor requires an operator-preseeded Git object mirror and verifies the
exact captured commit before streaming a bounded archive into its private
snapshot. It disables Git user/system configuration, replacement objects, hooks,
SSH and interactive credential lookup. Symlinks, hardlinks, special files,
escaping paths, `.git` content and excessive input are rejected. The compiler
receives only the ephemeral read-only snapshot, never a managed checkout,
mirror, Git metadata, home directory, worker token or Docker socket. Public
fetch is explicitly unsupported until pack/storage limits are implemented.

Compiler profile: UID/GID 65532, all capabilities dropped, no-new-privileges,
read-only rootfs and snapshot, network none, bounded exec-enabled work/tmp tmpfs,
separate bounded output tmpfs, memory and swap limit, CPU and PID limits, bounded
logs/output and deadline. Network/secret/socket/source-write smoke checks run in
the real fixture. Only fixed operator argv execute there.

Assembly profile: a new pinned rootless BuildKit daemon per attempt, UID 1000,
native snapshotter, **full process sandbox**, network none, read-only rootfs,
bounded memory/swap/CPU/PIDs and tmpfs for BuildKit storage/context/output. It
uses Docker's ordinary nonprivileged capability set. RootlessKit's uid mapping
helpers require a different profile from the compiler; the compiler's cap-drop
and no-new-privileges flags are deliberately not applied to this daemon.
`allow_rootless_unconfined: true` explicitly opts into seccomp/AppArmor/systempaths
unconfined, as described in the pinned
[BuildKit rootless documentation](https://github.com/moby/buildkit/blob/v0.29.0/docs/rootless.md).
There is no privileged or no-process-sandbox fallback and no host setting change.
Unsupported profiles fail closed. Actual kernel cgroup limits and daemon process
sandbox are checked before use. No anonymous image volumes are allowed.

BuildKit receives only a generated `FROM scratch`, local `COPY --chmod=0555`,
nonroot USER and fixed ENTRYPOINT. No source Dockerfile, RUN, external frontend,
remote ADD, external cache, registry credentials or auth forwarding is supported.
The supervisor uses `docker exec` against its exact owned container, never a
BuildKit TCP/SSH endpoint. Native platform must match the daemon and cached images.

The supervisor verifies OCI layout/index/manifest/config/layer closure, each blob
digest/size, layer diffID, platform, user/entrypoint and exact compiled payload
before reporting archive/layout/index/manifest identities with source, recipe,
toolchain/config and attempt provenance. Unattached blobs, extra images,
corruption, unexpected payloads and excessive output fail closed.

## Configuration and one job

Build the executables locally:

```sh
go build -o bin/release-control ./cmd/server
go build -o bin/rcp-worker ./cmd/worker
```

Start from `examples/workers/{recipe,authority,supervisor}.json`. These are
**inactive examples**: the credential is revoked/expired and IDs/paths must be
replaced. No token values are stored. Supply distinct operator control-plane
and worker tokens through their environment references; never reuse RC_TOKEN.
Images must already be cached by an operator: execution never implicitly pulls.

1. Replace Product/repository IDs, approved public repository URL, native image
   pins, fixed check/build argv and mirror/state/Docker configuration paths.
2. Compute `bin/rcp-worker recipe-hash RECIPE.json` after every recipe edit. Copy
   the same complete recipe and hash into authority and supervisor configuration.
3. Review the rootless security profile explicitly. Select current expiry and
   revoke/unrevoke deliberately. Keep the supervisor state directory private
   (0700); it must not be shared by concurrent supervisors.
4. On a separately approved protected installation, `RCP_WORKERS_FILE` selects
   the authority file. The server refuses worker configuration without RC_TOKEN.
   This document does not authorize changing a working installation.
5. Queue `queue_worker_job` through ordinary authenticated API/MCP with actor,
   Product/Integration, fresh `request_key`, recipe ID/revision/hash, configuration
   revision, captured revision ID and source SHA, exact typed parameters,
   deadline and max attempts. The returned Operation is durable before execution.
6. Run `bin/rcp-worker run-once SUPERVISOR.json`. It executes one eligible job,
   emits only a typed result, and retains the private local archive/journal under
   the owned attempt directory. No eligible job exits without executing anything.

## Supervision and recovery

A private fsynced journal is created before resources, with random owner labels,
explicit CREATE intents, lease/deadline and bounded result metadata. A successful
CREATE's full immutable container ID is persisted before START. Every subsequent
START, exec and deletion addresses that full ID after checking its owner; a
replacement with the old mutable name is never deleted. An exclusive process lock prevents
two supervisors owning the directory. A separate credentialless watchdog observes
that journal and parent liveness. On lease/deadline or supervisor SIGKILL it removes
only exact resource IDs whose owner labels match. Journal updates use a separate
cross-process lock so watchdog discoveries cannot overwrite heartbeat state.
Failed cleanup is retried;
an unavailable Docker engine does not manufacture successful cleanup.

On restart the supervisor first stops its own orphaned resources, then may replay
an identical persisted report. It never resumes uncertain execution. Start/report
lost responses use the same attempt/fence/input. Expired/cancelled writes remain
rejected, and the server requires explicit reconciliation before a new run.
If CREATE's outcome is unknown after timeout or supervisor death, an empty
provider observation does not prove cleanup: CREATE_UNCERTAIN remains durable
and the watcher continues observing. A late owned resource is discovered by its
unique intent name and owner label, its full ID is persisted, and only that ID
is removed. The journal remains unresolved and restart refuses new execution
until explicit operator reconciliation. No blind CREATE retry or automatic
manufactured Done flag is used.
Worker credentials and raw stdout/stderr are never journaled. Docker configuration
is a supervisor/watchdog path reference only; it is never mounted or passed into
source/BuildKit execution or forwarded as registry authentication.

Local archives/journals are operator-owned intermediate output. No automatic
host directory GC, registry retention, publisher, fleet deployment, untrusted
builds or arbitrary Dockerfile support is implied.

## Verification

Normal checks include synthetic corruption/bounds/provenance/replay/reopen tests
on bbolt/PostgreSQL. Real runtime tests are explicit opt-in on an approved native
Linux Docker engine with the two pinned images cached:

```sh
RCP_TEST_DOCKER=1 go test ./internal/workerrunner -run 'TestRealWorker|TestRealCreateStart' -count=1 -v -timeout=12m
```

They use temporary repositories/databases and exact owned containers. The Go
fixture checks isolation, builds twice from clean state with equal manifest
digest, then changes source and verifies different output/provenance. HTTP drops
the first start/report responses to verify replay. Separate failure, timeout,
cancellation, blocked-heartbeat lease expiry, supervisor SIGKILL and restart
smokes require owned cleanup and one execution attempt. Tests also cover
exact-ID deletion across a name-replacement race and real late
CREATE/START supervision windows after SIGKILL or lease timeout. Unknown CREATE
outcomes must remain unresolved even after their late exact resource is removed.
Fixture results are distinct from full stage02/03 and application A+B/selected-Prod acceptance.

## Persistent handoff to the native publisher

For automatic native delivery, configure `artifact_spool` in the supervisor:

```json
{"root":"/absolute/canonical/private/artifact-spool","max_archive_bytes":16777216,"max_total_bytes":268435456,"max_entries":32}
```

The operator precreates a dedicated canonical private directory (0700). Keep it
on a durable POSIX filesystem supporting file/directory fsync, hard links and
advisory locks. Only trusted supervisors receive write access. The source and
BuildKit containers never receive this mount. The separate publisher mounts the
same volume read-only and uses a separate writable private state directory.
The configured root may differ between hosts/mounts; receipt hashes identify
entries, while paths never enter RCP. This supports a shared volume or cohosted
processes; it is not remote object storage or fleet orchestration.

After verifying the OCI archive, the supervisor copies bounded bytes into its
own staging file, verifies the archive hash, fsyncs the file, publishes the hash
entry without overwrite and fsyncs the directory. Only then may it persist or
send a PASSED report. Existing-entry replay rechecks bytes and both durability
acknowledgements. Global byte and entry quotas serialize all cooperating
writers. Quota exhaustion, corruption, symlinks, unsafe permissions and sync
failures fail closed. Restart never rebuilds an unknown running attempt. A crash
before its report is durable can leave an immutable spool entry requiring
operator reconciliation; that entry alone does not authorize RCP acceptance.

Staging cleanup removes only the current invocation's own staging file. Stale
staging and historical archives consume quota; no historical GC or automatic
retention is implemented. Keep capacity under operator control. Spool export is
optional for standalone local builds, but required for unattended handoff across
sequential native deliveries with unchanged operator configurations.
