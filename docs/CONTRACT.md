# Shared application contract

## Native worker recorded history

`get_worker_history` / `GET /api/worker-history` require `product_id` and expose
separately paginated `jobs`, per-job `attempts` and complete frozen `links`.
Optional `feature_id` selects jobs by immutable recorded revision membership,
including multi-Feature native COMPOSE/main-candidate jobs. Default limit 25,
maximum 100; scope/collection/job-bound cursors; 32 KiB pages and 128 KiB MCP
results. Read authorization precedes lookup. Recorded statuses and
`recorded_snapshot_unverified` grant no verification or execution authority.
Unknown chains retain Product history without guessed Feature links. See
[native worker history](WORKER_HISTORY.md) for trust and storage boundaries.
Existing execution and `get_worker_job` contracts remain unchanged.

`domain.Command` is `{action, actor, id?, product_id?, feature_id?, integration_id?, gate_id?, check_id?, result_id?, data: {...}}`. Actor is mandatory. IDs supplied for creates are optional; generated when absent. Execute returns created/updated entity. State uses arrays (always non-null): products, features, integrations, gates, checks, results, findings, memories, environments, repositories, releases, events. All entities have snake_case JSON keys. Entity IDs globally unique.

Actions and data:
- create_product: name, description
- create_feature: product_id top-level; title, problem, goal, requirements[], constraints[], context, repositories[], owner, status
- update_feature: feature_id; same fields, sparse update
- create_integration / update_integration: feature_id / integration_id; title, objective, rationale, dependencies[], acceptance_criteria[], repositories[], branches[] (objects repository_id,name), commits[] (objects repository_id,sha), owner, working_areas[], position, remaining[], status (create only defaults planned; update status use transition)
- start_integration / complete_integration / transition_integration: integration_id; transition data.status
- record_progress / record_decision / record_discovery / add_blocker / handoff: feature_id and optional integration_id; title, body, reason, completed[], current, remaining[], next[], warnings[], commit, deployment, session. Memory kind is progress/decision/discovery/blocker/handoff. progress updates integration completed/remaining. Handoff is historical structured memory. add_blocker defaults open.
- resolve_blocker: id (memory ID); body resolution
- create_gate: feature_id; title, reason, integration_ids[], position, blocking (default true), environment_id, commit, deployment
- add_check: gate_id; title, mechanism, instructions
- record_check_result: check_id; result (passed/failed/blocked/skipped), commit, environment_id, deployment, steps[], observations, logs, artifacts[] ({kind,url,label})
- record_finding: result_id (failed/blocked result); title, severity, body, integration_ids[], gate_ids[], blocks_release (default true)
- resolve_finding: id; body resolution, commit fix
- create_environment / update_environment: product_id / id; name, desired, reconciled, runtime (objects), composition (object)
- create_repository: product_id; name,url,provider
- plan_release: product_id; name,integration_ids[],excluded_integration_ids[],notes (immutable intent record; does not deploy or mark released)

State entity relationship fields product_id,feature_id,integration_id etc. Each entity has id, created_at, updated_at and actor. Events include id, action, actor, at, entity_id, feature_id, product_id, data (full command). Resume returns feature, integrations, progress {total,ready,released}, memories, gates, checks, results, findings, environments, repositories, other_active_work, next_actions, events. Integration carries completed[], remaining[], owner, working_areas[], branches[],commits[]. Gate result computed from latest results, not mutable tested flag. Default statuses planned/working/implemented/verifying/ready/released.

Go APIs: application.New(store) *Service; State(ctx) (domain.State,error); Execute(ctx,domain.Command) (any,error); Resume(ctx,featureID) (any,error). persistence.Open(ctx,url) (*Store,error); store.Close(). `domain.Actions` exported []string. `application.ErrNotFound`, `application.ErrValidation` support errors.Is. Domain commands use Data map[string]any.

Validation details: check results require at least one of commit/deployment. Gates must name one or more integration_ids; applicability is explicit. Decision requires reason. Integration edits conservatively return a ready integration to working. Unknown or non-lowercase data fields and metadata injection are rejected. Entity IDs are unique across all kinds, including append-only records. Release has `status: planned` and immutable `snapshots` of selected integrations; it is an intentional release plan, not evidence of deployment. Completion reevaluates latest applicable check evidence and requires linked commits to match check provenance when commits are provided.

Resume compactness: `events` contains only the latest 20 audit metadata summaries (id, action, actor, at, entity_id, product_id, feature_id), without duplicated command data; `events_total` and `events_truncated` indicate omitted history. Full audit remains in state. Integrations and gates sort by position then ID. `next_actions` combines the latest feature handoff and latest handoff for each integration in plan order, with active integration remaining-work fallback when no handoff exists; duplicate actions are removed. All structured memories and check results remain available in resume.

## Application and environment composition planning

Environments are arbitrary per-product records, not a fixed DEV/STAGE/PROD enumeration. `create_environment` / `update_environment` accept `cluster` and `namespace`; these are target metadata and never provision infrastructure. `desired_composition_id`, `desired_operation_id` and `selection_generation` are server-owned desired intent. Every selection/reconciliation or target reset advances the monotonic generation, including selecting the same plan again; observations do not. Read the token through `get_environment_state`. Target resets clear desired identity while preserving observed state. Overflow refuses atomically. Legacy `composition` maps remain readable but typed compositions are authoritative for selected branch plans.

Additional commands:
- `create_application`: top-level product_id; data `{name, repository_id, path?}`. Path is optional clean repository-relative monorepo path.
- `record_integration_revision`: top-level integration_id; data `{repository_id, branch, base_commit, head_commit, commits: string[]}`. Immutable manually captured provenance. Full lowercase 40- or 64-hex hashes required; one hash format per revision, commits nonempty, unique and containing head_commit. Source branch and repository remain recorded after branch merges/deletion. These assertions are not Git-provider-verified.
- `plan_composition`: top-level product_id; data `{environment_id, name, auto_refresh?, components: [{application_id, base_ref, base_commit, target_branch, revision_ids: string[]}]}`. At least one component; empty revision_ids means base-only app. Target branch must use `generated/` and differ from base_ref. Base commit is a full hash. Same-repository applications must select identical base_ref/base_commit/target_branch and ordered revision_ids lists because one repository produces one composed branch. Only one selected revision per integration/repository. Dependencies must also be selected or already released. Cross-product/repository references rejected.
- `update_environment_feature`: top-level `feature_id` (optional matching `product_id`); data `{environment_id, revision_ids: string[], name?, idempotency_key, expected_selection: {composition_id, operation_id, generation}}`. The nonempty explicit patch replaces only supplied integration/repository pairs in their current positions and appends new pairs in caller order, preserving all untouched revisions, pinned base and existing application/repository cohort. Same-product target Feature/revisions and all ordinary composition dependency/monorepo validations apply. New repository/cohort, removal, base movement and released target work require explicit replan. Requires a current desired baseline, enabled DEV/TEST PR-only targets and current execution authority; active uncertain GitOps writes refuse. CAS is checked atomically before any plan, operation or audit. A conflicting selection returns an actionable validation conflict: reread/reapply explicitly, never silently retry. Key (1–128 bytes), actor, scope and exact typed payload bind a durable COMPOSE receipt: exact replay returns that historical operation even after supersession/restart/restore without selecting, auditing or executing again; conflicting reuse refuses. An unchanged patch with a fresh key refuses and directs the caller to the current operation or explicit reconcile. The new pinned composition always has `auto_refresh: false`. Generic HTTP/MCP `execute`, named MCP `update_environment_feature`, and `POST /api/features/{id}/update-environment` share this command. Runtime/reconciled evidence remains untouched. This capability does not authorize live delivery outside the governing main-first policy.
- `assemble_environment`: top-level product_id; data `{environment_id, name?, integration_ids?, expected_selection?}`. Builds a fresh immutable composition from the latest captured revisions of the selected active integrations (an empty list selects all active captured work), groups same-repository revisions in the supplied order, and immediately queues an asynchronous `COMPOSE` operation. The operation creates disposable generated branches, builds artifacts and applies configured DEV/TEST GitOps targets. A conflict is persisted for agent resolution; poll the returned operation with `get_operation`. This is explicit full replacement, sets `auto_refresh: true`, and may refresh base and every selected revision. Automatic refresh requires that explicit boolean and carries the captured selection CAS into the transaction; stale snapshots safely skip without records or audit. Manual `plan_composition` defaults to safe `auto_refresh: false` and can explicitly opt in.
- `select_composition`: top-level id is composition ID; data `{}`. Sets only the target environment's desired_composition_id and audit metadata. Does not alter reconciled/runtime state or claim deployment, merge success or readiness.

After Git refresh observes a newer revision, the scheduler automatically queues
the same assembly once for a selected DEV/TEST composition only when its persisted
`auto_refresh` is explicitly true and no assembly is running. Refresh checks its
captured selection token atomically; production and pinned compositions are excluded.

New DEV/TEST COMPOSE execution requires enabled PR-only mappings for every
component. Explicit assemble/reconcile/feature-patch requests freeze server-derived
`source_retention_origin_id` and each source's typed `retention` intent (exact
revision-ID refs, ordered SHAs and input hash). `sources[].retained` holds bounded
timestamped readback observations; callers cannot inject these fields. The worker
admits one create-only retained ref per bounded tick, validates complete declared
history independently, and rereads admitted refs plus structural generated
step/final identity before consuming saved results and before creating children.
All provider I/O is outside transactions; the child/admission transaction requires
an unexpired current lease, exact checked snapshots, current selection/configuration,
connection grants and native authority. Lost responses/reopen can observe an
existing exact ref; an admitted missing/moved ref blocks without healing. Legacy
operations without intent require a new explicit request. Scheduler refresh has a
private origin and may inherit only the selected parent's valid explicit intent,
CAS and integration scope. Legacy, cancelled and pre-restore lineage cannot mint
new automatic writes. Complete lineage must have unambiguous scoped creation
events after the last restore in durable append order; host-clock ordering is not
authority. Conflict context, audit and parent BLOCKED outcome use the same current
unexpired lease/frozen-authority admission transaction. Ref observations are reachable-retention evidence, not
permanent branch protection or cryptographic proof of merge contents. Existing
`protect_integration_revision` still requires `approve: true`. Release-candidate
and promotion semantics do not gain retention-write authority from this change.

State adds `applications`, `integration_revisions`, `compositions` arrays. Application fields: id/product_id/meta, name, repository_id, path. IntegrationRevision fields: id/product_id/feature_id/meta, integration_id, repository_id, branch, base_commit, head_commit, commits. Composition fields: id/product_id/meta, environment_id, name, auto_refresh (default false), status=`planned`, components, application_snapshots (Application[]), revision_snapshots (IntegrationRevision[]), environment_snapshot (Environment). Snapshots and revisions have no update action. Recording a newer integration revision never rewrites prior compositions.

Resume includes feature-linked integration_revisions, relevant applications, and compositions containing revision snapshots for that feature. Automatic branch construction, Git verification, Flux reconciliation and Kubernetes changes remain pending provider work.

Environment names are trimmed and unique case-insensitively within each product (the same name in different products is allowed). Changing cluster or namespace clears desired_composition_id; selecting an older plan whose captured cluster/namespace differs is rejected and requires replanning. Revision capture must use an integration's declared repository when its repositories list is nonempty; otherwise any repository in the product is permitted.

## Target architecture boundary

The lifecycle/provider contracts in `internal/domain/lifecycle.go` and `providers.go` are not persisted State arrays or additional execute actions yet. See [architecture](ARCHITECTURE.md) and [rollout](ROADMAP.md). Current `Application` maps to the domain term Component without JSON/storage renames. Current `Release` remains an immutable planned selection; future final `ReleaseRecord` requires verified deployment evidence. Instance visibility remains trusted-local, not per-user product authorization.

MCP `get_state` is a compact navigation projection, not a serialized `State`: no arguments lists Products, `product_id` lists Features, and `feature_id` lists Integrations with compact parent identities. A pair of IDs must match. Unknown, duplicate or case-variant arguments, invalid scopes, limits or cursors fail closed. `limit` defaults to 25 and accepts 1..100. Labels are UTF-8 prefixes of at most 256 bytes with an explicit `truncated` flag; IDs remain exact. JSON projection is bounded to 32 KiB and the full MCP result to 128 KiB. Follow `pagination.next_cursor` using the same scope; a byte-limited page may contain fewer cards than requested. Keyset pagination is live under concurrent writes, not a frozen snapshot or an authorization mechanism.

Only identity, bounded labels and recorded statuses are included. History, Context, requirements, memories, results, logs, operations, reports and credentials are omitted. Recorded status does not prove current gates, runtime health or release acceptance. Use scoped context and specialized query tools for details; `resume` is not a bounded substitute. HTTP `GET /api/state`, its ETag, backups and exports retain the full snapshot contract. A missing bounded metadata-reader capability produces an error rather than falling back to the full State reader. PostgreSQL projects scoped metadata before transfer; local bbolt scans its existing JSON blob in O(blob) time while decoding selected metadata only. No storage-format migration is implied.

## GitHub execution and external impact context

The current execution commands and their typed fields are published by `/api/schema` and MCP `execute`. Named MCP queries include `get_integration_context`, `get_git_state`, `get_unlinked_git_commits`, `get_environment_state`, `get_operation`, `get_attention_required`; `deploy_integration` returns a durable operation immediately. Git synchronization and deployment steps execute in the worker, outside the command's database transaction. `refresh_repository_git` scans every non-default branch of an attached code repository (SOURCE/APPLICATION/LIBRARY/MIXED) and records commits even when no Feature/Integration can be inferred. Those records appear as `UNLINKED_GIT_COMMIT` attention until an agent links them; `resume` includes the same list as `unlinked_git_commits`, and GITOPS repositories remain delivery-state history rather than work candidates. All `BEHIND/DIVERGED` observations are retained for Git context but are not Operations attention; a concrete deploy operation still performs its own freshness check. This keeps old Codex branches from looking like delivery failures.

When a release was executed by an external MCP agent using GitHub Actions/Flux/operator tooling rather than by the Control Plane's own release-candidate worker, record the fact with `record_release`: provide `product_id`, `data.name`, `data.environment_id`, `data.integration_ids`, and any known `source_commits`, `artifact_digests`, `gitops_commits`, `evidence`, and `notes`. The resulting immutable release row is marked `released` and `recorded_externally=true`; it preserves the current integration snapshots and does not pretend that the Control Plane itself performed or verified the rollout.

The instance Repository Registry stores numeric GitHub identities; Product Repository records are explicit attachment projections retaining legacy IDs. Connection grants control which installation a Product may use. Feature repository selection narrows Product attachments; Integration selection narrows the Feature. Source discovery is provider-observed rather than caller-asserted names.

`create_external_system` / `update_external_system` accept name, description, team, contact, interfaces[], contracts[], notes. `create_system_relationship` links a product (and optional component_id) to external_system_id using type DEPENDS_ON, CONSUMES, PROVIDES_TO or SHARES_DATA_WITH. `set_external_scope` records external_system_ids[] and relationship_ids[] against a Feature, Integration or Gate. Scopes are append-only; the latest target scope is effective. Changing a blocking Gate's external obligation requires fresh check evidence. Resume/context returns relevant systems, relationships and scopes. External systems have no deployment commands.

Operation lifecycle status is separate from deployment state. `SUCCEEDED` for this milestone means the GitOps mutation was confirmed; deployment remains `GITOPS_APPLIED` / pending reconciliation, never inferred DEPLOYED. See [GitHub DEV setup and build contract](GITHUB_DEV.md) for credentials, workflow evidence and the exact live acceptance boundary.

## Caller-agent conflict handoff

`get_operation` additionally returns `agent_action` for a persisted textual
composition conflict. Its bounded `RESOLVE_CONFLICT` context tells the requesting
agent the exact original branches/SHAs and existing claim/report/verify/reconcile
steps. RCP does not start a resolver agent. `UNAVAILABLE` omits all partial inputs;
the 32 KiB limit covers only this new field. Branch/ref observations are historical
and the instructions grant no new execution rights. See [conflict workflow](MANAGED_BRANCHES.md).

## Full instance backup

MCP `get_backup_capabilities`, `create_backup {actor}`, `restore_backup {actor, archive_base64, sha256}` share the application backup service. HTTP uses binary gzip at POST `/api/backups/export` and `/api/backups/restore`, with actor/checksum headers. Restore is atomic into empty state only; it preserves record/event order and cancels imported active operations with original evidence in restore audit. Format v1 is bounded to 8 MiB compressed / 64 MiB expanded. See [backup contract](BACKUP.md).

### Default action attribution

The Web UI records actions as `human/local` without an author field. HTTP and MCP
commands default omitted/blank attribution to `agent`. Callers may still supply a
specific agent identifier to preserve provenance. These labels are not authenticated
identities and do not grant permissions. Historical attribution remains unchanged.


## Workspace agent kit

`GET /api/agent-kit` and MCP `get_agent_kit` share `instructions`, the existing
`skill` (`rcp-handoff`) and the separate `delivery_skill` (`rcp-delivery`). Each
skill has exact `name`, `content` and lowercase SHA-256. `get_agent_skill` accepts
only these two names; each has a fixed `rcp://skills/<name>/SKILL.md` resource.
Embedded public snapshots and their repository/commit/path/checksum provenance
are tracked under `internal/agentguide/skills/`. Guidance does not grant adoption,
external execution or a main-first/human-only Flux policy cutover.

`connect` validates both skill identities/frontmatter/checksums before writes.
Its seven fixed project paths are `.codex/config.toml`, `AGENTS.md`,
`.release-control.json` and both skills under `.agents/skills/` and `.claude/skills/`.
It preserves managed blocks, unrelated configuration, file modes and token-name/
credential-profile semantics; no credential values or global/Claude configuration
are installed. Identical raw canonical skill bytes may be adopted; differing
unmanaged content or symlinks refuse the entire install. Staging and rollback
retain the existing process-failure atomicity, not multi-file crash atomicity.

Automatic Product selection uses MCP `get_state {"limit":2}` only, requiring
`compact_navigation`, Product collection with no foreign parents, present integer
`returned` equal to the bounded item count, a present boolean `has_more:false`,
no continuation cursor, and exactly one valid ID of at most 128 bytes. Otherwise
an explicit Product ID is required, without full-State fallback or file writes.
The discovery-only client has a 15-second context/HTTP timeout, no reconnect
retries/redirects/standalone SSE, same-origin credentials and a 128-KiB whole-body
cap for every HTTP status/content type as well as the SSE event cap. Declared
oversize is rejected before body reads; unknown/chunked/lying lengths are bounded
and oversize is an error, never silent JSON truncation. Explicit Product selection
keeps the scoped HTTP context path and 2-MiB response cap, with identity readback.
