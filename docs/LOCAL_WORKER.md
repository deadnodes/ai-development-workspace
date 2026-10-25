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

The operator selects exactly one source strategy: a preseeded Git object mirror,
or the recipe-pinned GitHub file-tree reader described below. There is no fallback
between them. Mirror mode verifies the exact captured commit before streaming a
bounded archive through a fresh bare view; it disables user/system Git
configuration, replacement objects, hooks, SSH and interactive credentials.
Both modes reject links, special files, escaping paths, `.git` content and
excessive input. The compiler receives only the completed ephemeral read-only
snapshot, never a managed checkout, mirror, Git metadata, home directory, source
credential, worker token or Docker socket. Git clone/fetch and pack ingestion
remain unsupported.

### GitHub source without a preseeded mirror

Set `execution.source_fetch` in the immutable recipe, then recompute its hash:

```json
{"provider":"github","repository_id":123,"timeout_seconds":60,"max_download_bytes":16777216,"max_requests":1024,"max_entries":10000,"max_depth":32}
```

This fragment is inactive configuration: replace `123` with the approved numeric
GitHub repository identity. The recipe URL must be canonical
`https://github.com/owner/repo`. The profile and bounds enter the recipe and job
input hashes; callers cannot provide another URL, branch, command or credential.
This mode accepts only pinned 40-hex GitHub commit IDs.

Remove `source_mirror` from the supervisor configuration and explicitly select
either `"source_github":{"anonymous":true}` for public reads, or
`"source_github":{"anonymous":false,"token_env":"RCP_SOURCE_READ_TOKEN"}`.
Use a separately provisioned repository-scoped Contents-read credential; no
credential is issued by this profile. References and values must differ from
the worker credential and `RC_TOKEN`. Separation is checked before claim and
before every source request, including rotation and credentials captured by the
running supervisor. Anonymous mode references no source token. Mirror mode
requires `source_mirror` and refuses `source_github`.

The supervisor makes HTTPS GET requests only to `api.github.com`, using locally
constructed repository, commit, tree and blob paths. It verifies repository ID
and exact full name, then obtains the pinned commit's tree. Redirects, provider
URL fields, truncated trees and alternate hosts are refused. GitHub is trusted
for the commit-to-tree mapping; the reader reconstructs every tree object and
verifies each raw blob's Git object hash and size. This does not reconstruct or
cryptographically verify the complete commit object. See the GitHub contracts
for [trees](https://docs.github.com/en/rest/git/trees#get-a-tree),
[raw blobs](https://docs.github.com/en/rest/git/blobs#get-a-blob) and
[commit objects](https://docs.github.com/en/rest/git/commits#get-a-commit-object).

The snapshot contains the raw file tree: no `export-ignore` or `export-subst`
processing, submodule checkout, LFS download or dependency fetch. Such content
must already fit the trusted offline recipe. Tree closure, directory/type
consistency, modes, names and duplicate/case-colliding paths are validated before
blob download. Expanded bytes include repeated files, even when their blob IDs
match. Metadata responses are capped at 8 MiB; recipe bounds additionally cap
cumulative decoded HTTP body bytes (at most 512 MiB), requests (4096), entries
(20000), depth (64) and source timeout (120 seconds). Existing `source_bytes`
also caps the expanded snapshot. The current job/lease cancellation context
continues to apply.

Only a complete verified snapshot is made readable by the nonroot compiler:
directories become 0755, regular files 0444 and executable files 0555, beneath
the private 0700 attempt directory. The compiler mount remains read-only and
its network remains disabled. Fetch, verification or preparation failure starts
no compiler and cannot produce PASSED or a local OCI receipt. Responses and
credentials are not journaled or reported; failures use bounded categories.
There is no shared source cache, source-history store or automatic source GC.

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

Fixture defaults remain `orbstack` and `linux/arm64`. To run the same small-source
fixtures on a native Linux AMD64 engine, explicitly set
`RCP_TEST_DOCKER_CONTEXT=default RCP_TEST_DOCKER_PLATFORM=linux/amd64` alongside
`RCP_TEST_DOCKER=1`. Only `orbstack`/`default` contexts and `linux/arm64`/
`linux/amd64` platforms are accepted. Preflight requires the expected local Unix
socket and a matching native daemon architecture; remote contexts, conflicting
Docker environment overrides and emulation are rejected. Cache the pinned Go,
BuildKit and Distribution images before running; the upstream workflow lists
exact AMD64 pull commands. The ARM64 BuildKit digest is unchanged. Its AMD64
sibling is from the same `v0.29.0-rootless` index
`sha256:93efd7f1c17f16cea080c34f68e19863b9fe541db550bf607221ce425c0ab9ef`;
this changes no worker production defaults or operator execution profile.
The existing ARM64 clean-build golden remains asserted byte for byte. AMD64
retains OCI platform/content and source checks, equal repeated clean manifests,
and different output after changed source; its observed manifest is logged.
This Go fixture separately reads the compiled app from the verified OCI archive
with bounded in-memory reads, checks ELF64 and the selected architecture's
machine plus the expected source literal, and requires equal clean binary
digests and a different changed-source binary digest. The generic production OCI
verifier checks image metadata/content binding and allows non-ELF images; it
does not promise this Go-specific ELF check.
An independently established fixed AMD64 golden remains a narrow verification
limit, separate from those assertions and from real Linux job execution proof.

The separate `native-docker` upstream CI job runs worker isolation/lifecycle,
late CREATE/START and cleanup recovery, source fetching, composition, historical
main candidate, grouped Dev GitOps, protected API/checks/publication and registry
observation fixtures on `ubuntu-24.04` with isolated bbolt/PostgreSQL stores.
It runs only trusted upstream push/manual events, with read-only repository
permissions and without provider/registry credentials or QEMU. Image publication
waits for both ordinary checks and this native acceptance job. Registries are
temporary loopback fixtures and GitHub/PR adapters use isolated in-process
stubs; no installation is deployed. Application operator-profile/runtime tests require separately approved
images/configuration and are excluded. Passing these fixtures does not establish
fleet, Dev installation or production acceptance.

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

## Explicit local cleanup reconciliation

A known-ID `CREATE_UNCERTAIN` remains historical uncertainty after the watchdog
removes its exact owned container. The local operator can close only its cleanup
obligation. This command does **not** reconcile server job state, send a report,
mark a job PASSED, release quarantine, or resume the original execution. Use the
separate authorized server `reconcile_worker_job` failure/cancellation operation
when server state also requires reconciliation.

Before existing cleanup removes a resource, the watchdog/supervisor attempts a
bounded read-only presence check. It opens one `docker system dial-stdio` stream,
reads daemon identity, and inspects the full container ID, exact intent name and
owner label over that same stream. The private journal stores only this typed
proof and observation time. The helper uses raw HTTP requests on that stream,
never a reconnecting HTTP transport. Docker CLI's audited
[v28.5.2 implementation](https://github.com/docker/cli/blob/v28.5.2/cli/command/system/dial_stdio.go)
and the tested
[v29.3.1 implementation](https://github.com/docker/cli/blob/v29.3.1/cli/command/system/dial_stdio.go)
dial once and copy bytes without reconnecting. Keep an operator-controlled CLI
with this behavior. Unsupported transports, disconnects and invalid responses
produce no proof; existing exact-ID cleanup still runs conservatively.
Canonical struct field spellings are required in daemon responses and journals;
case-distinct arbitrary Docker label keys remain valid. Journal writes validate
their complete bounded content before replacement. If optional presence evidence
cannot fit the journal's 8192-byte bound, cleanup continues without that proof,
preserves the readable journal, and explicit closure remains unavailable.

Use the canonical absolute private state directory configured for the original
supervisor and its exact 32-hex owner. Inspection acquires the existing exclusive
state and journal locks without waiting. Its bounded JSON includes owner,
journal SHA256, resource states/full IDs, proven daemon identities, local closure
eligibility and reasons. `PRESENCE_UNKNOWN` and `RESOURCE_ID_UNKNOWN` remain
unknown. Inspection performs no Docker query; eligibility describes local
preconditions, with fresh daemon verification still required at reconciliation.

```sh
bin/rcp-worker inspect-cleanup /canonical/private/worker-state OWNER
bin/rcp-worker reconcile-cleanup /canonical/private/worker-state OWNER EXPECTED_JOURNAL_SHA256 operator/host-01
```

`operator/host-01` is a configured non-sensitive local provenance reference,
not an authenticated server identity or approval. Use letters, digits, `_`, `/`
and `-` (1–64 characters, starting with a letter/digit); do not use personal names
or email addresses. Copy the exact hash from inspection. Reconciliation accepts
no arbitrary journal path, Docker endpoint or RCP-supplied command. It reads the
original operator Docker context/configuration from the private journal only.

Closure requires a positively dead original supervisor (ESRCH or a successful,
bounded unambiguous zombie-state check), expired lease **and** deadline, unchanged
canonical private directories/locks/journal and expected hash, and a durable
presence proof for every resource. Zero-ID uncertainty, old absent containers
without that proof, live/ambiguous PIDs, unsafe paths, corruption, concurrent
writers and provider failures refuse. PID reuse conservatively refuses closure.
One fresh daemon stream must match the persisted daemon identity and prove all
exact IDs and intent names absent; a same-name replacement is left untouched
and prevents closure. Changing mutable Docker contexts between invocations
cannot supply a missing original-daemon presence proof.

The command fsyncs a closure bound to original resource IDs/states, daemon
identity, accepted hash, operator reference and verification time before Done.
It preserves original CREATE_UNCERTAIN facts, execution times and actual reports.
Identical replay with the original accepted hash and operator reference succeeds
without another write or provider query. A changed proof is rejected. Reopening
validates closure consistency and its original journal hash. Future supervisor
recovery/watchdogs respect that closure and may claim unrelated work; they never
restart the original workload. No local historical journal GC is provided.
