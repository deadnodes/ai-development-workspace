# Agent execution and continuation contract

Status: Stage 1 implementation in progress, 2026-11-25. The first implementation adds typed execution/group state and a reconciliation entry point; stage 1 acceptance and full transport/configuration coverage remain in progress. This document specifies a control loop for
external coding-agent orchestrators. RCP records intent and authority, detects
conditions requiring continuation, and exposes durable wake events. A separate
supervisor delivers a prompt to an already registered harness session. RCP never
starts an agent, edits managed source, or infers permission to deploy.

## 1. Code research and reuse boundary

### Existing primitives

| Concern | Current fact and reusable primitive |
|---|---|
| Integration identity/status | `domain.Integration` already holds status, owner, dependencies, acceptance criteria, progress lists, branch/commit/PR references in `internal/domain/model.go:Integration`. Status values and allowed transitions are shared policy in `internal/domain/status.go:StatusCatalog` / `CanTransition`; command enforcement is in `internal/application/service.go:executeIntegrationCommand` (transition command branch). |
| Worker lease/fence | The external worker is a separate security principal loaded from worker credentials; `application.Service.ClaimWorker` in `internal/application/worker_execution.go:ClaimWorker` authenticates scope, increments a monotonic fence, and creates a persisted attempt with server expiry. `HeartbeatWorker` / `ReportWorker` funnel through `internal/application/worker_execution.go:mutateWorker`, which validates principal, attempt, current fence, immutable input hash, lease and grants. Domain shapes are `WorkerJob`, `WorkerAttempt`, `WorkerLease`, and request structs in `internal/domain/worker_jobs.go`. Reuse transactional lease/fence mechanics only. Do not use worker Principal as coding-agent identity or grant a coding agent the worker API. |
| Durable external operation | `domain.ExternalOperation` and `OperationStep` in `internal/domain/external.go` store typed operation status/phase, scope, attempts and step evidence; generic operations are created and appended through `application.Service.Execute` in `internal/application/service.go:Execute`. They describe bounded RCP/external work, not a coding-agent lease. Link `waiting_external` to a specific operation id. |
| Attention query | MCP `get_attention_required` in `internal/transport/external.go` delegates to `application.Service.Query("list_attention", ...)`; `application.Service.Query` in `internal/application/external_query.go:Query` derives operation/build/deployment and other attention. This is pull-only, product scoped, and requires a caller. It does not track who is responsible for continuing an Integration. |
| Stale-operation watchdog | Build and Deployment records have `StatusChangedAt` in `internal/domain/build.go:Build` and `internal/domain/deployment.go:Deployment`. Transition handlers reset it (`internal/application/builds.go:advanceBuild`, `internal/application/deployments.go:advanceDeployment`). `stale_operation_seconds` is global build config with a 7200-second default and zero disable in `internal/buildconfig/config.go:Config.StaleTimeoutSeconds`. `application.Service.DispatchBuildsOnce` in `internal/application/builds.go:DispatchBuildsOnce` closes stale builds; `application.Service.DispatchDeploymentsOnce` in `internal/application/deployments.go:DispatchDeploymentsOnce` reconciles stale deployments; `application.Service.Query` in `internal/application/external_query.go:Query` includes deployment reconciliation attention. This watchdog closes/reconciles stale RCP delivery operations; it is not agent progress monitoring. Reuse timestamp/config layering concepts, not its terminal-close behavior. |
| History | `domain.Event` is append-only in `internal/domain/model.go:Event`; application mutations append audit events, e.g. `application.workerAudit` in `internal/application/worker_execution.go`. PostgreSQL and local stores protect the event prefix (`internal/persistence/cache.go:validateOrdinaryDelta`, `internal/persistence/local.go:Update`). New execution lifecycle changes must append provenance; do not rewrite history to represent current state. |
| Live UI | Current transport exposes snapshots and ordinary HTTP endpoints in `internal/transport/server.go:New`; no RCP SSE endpoint, event stream or long-poll endpoint was found. Code-mode's `text/event-stream` in `internal/transport/code_mode.go` is the response protocol for MCP, not server-pushed UI updates. UI refresh is polling/manual as implemented by `web/static/app.js`. Polling event API is sufficient initially; SSE is optional. |
| Configuration layering | Runtime defaults such as stale-operation timeout are instance process configuration in `internal/buildconfig/config.go:Config`, not Product state. `domain.Product` in `internal/domain/model.go` has no settings map. `application.ProductConfiguration` / `productConfiguration` in `internal/application/configuration.go` is a one-way export assembled from typed Product-owned DB records, not a settings/import API. Thus global -> Product -> Integration timeout overrides are new typed persisted settings, not reuse of a general Product settings feature. |
| Git inventory | `refresh_repository_git` records provider-observed branch commits and unlinked commits; `application.Service.Query("get_git_state", ...)` / `get_unlinked_git_commits` expose evidence (`internal/application/external_query.go:Query`). `internal/application/managed_branch.go` owns configured managed branch operations. Inventory is evidence, not a write fence. |

Product-level settings should be a typed field/record with validation and revisioned effective
configuration, not arbitrary JSON. All transports must call the same application
commands/queries.

### Identity and authority

`orchestrator_id` identifies the supervisor/orchestrator installation, and
`agent_id` identifies the coding agent instance. Both are caller-attributed
labels unless a later authentication scheme binds them. `harness.session_ref`
is an opaque native session identifier, never a bearer credential. Worker
principal remains reserved for the isolated worker API and its capabilities.

An Integration execution has its own monotonic fence and server-owned lease
expiry. Every RCP-managed mutation made on behalf of that Integration must
include the fence and compare it atomically: progress, Integration lifecycle
transitions, branch intent, and build/deploy operation creation. A stale fence
fails with `STALE_FENCE`; no audit/state mutation occurs. Operations already
started retain their own durable lifecycle and are not retroactively cancelled.
Cross-integration group operations validate a current fence per member.

Lease expiry means `lost`, not proof that the old process stopped. It never
causes automatic assignment. Takeover requires an explicit supervisor claim
after a state-check wake-up, increments the fence, and thereby fences the former
executor from RCP writes. A stalled heartbeat does not revoke or shorten the
lease; it requests a check from the orchestrator first.

#### Direct Git writes (outside the RCP fence)

RCP cannot prevent an agent holding Git credentials from pushing directly after
takeover. Feasible now: keep branch ownership recorded in `Integration.Branches`
and `WorkClaim`; use provider-enforced protected branches/rules and per-agent
branch namespaces where configured; have agents use unique worktrees/branches;
record the expected branch/head on claim and re-read provider refs before any
RCP-orchestrated Git mutation; after takeover, scan inventory for commits newer
than the takeover baseline and surface them as evidence requiring reconciliation.
Do not automatically merge, discard or rewrite those commits. Git inventory may
lag and cannot prove absence of a later push. Later: provider webhooks or
frequent branch inventory with a takeover watermark, provider-native signed
leases/conditional ref updates, and credential brokering scoped to per-executor
branches. Even then, an already-issued broad token cannot be revoked by RCP.

## 2. Execution model

Add a typed `IntegrationExecution` sibling record keyed uniquely by
`integration_id`, stored in the same Product scope and transaction as its
Integration. Do not overload `Integration.Owner` (human/team ownership),
`Integration.Status` (delivery lifecycle), or `ExternalOperation` (one bounded
operation). A sibling separates orchestration state and permits independent
lease/event evolution while retaining Integration scope validation.

Suggested fields:

```json
{
  "integration_id": "int-…", "desired_state": "ready",
  "state": "running", "orchestrator_id": "sup-…", "agent_id": "agent-…",
  "harness": {"kind":"codex", "session_ref":"…", "cwd":"…", "host":"…"},
  "fence": 4, "lease_ttl_seconds": 300,
  "last_heartbeat":"…", "last_progress":"…", "claimed_at":"…",
  "exit_reason":"", "waiting_ref":"operation-id", "blocker_ref":"…",
  "reconciliation": {"enabled":true,"action_required":false,"reason":"","since":null}
}
```

`action_required` is server-computed and read-only to agents. `desired_state`
must be an allowed Integration target status, normally `ready` or `released`;
reaching it immediately sets execution `completed` and clears action_required.
Do not interpret `ready` as released or externally accepted.

States:

| State | Meaning | action_required |
|---|---|---|
| `unassigned` | No current executor | true when reconciliation enabled |
| `running` | Current executor holds a live lease | false unless stalled threshold crossed |
| `waiting_external` | Waiting on a named pending RCP/external operation | false while pending; true when it resolves/fails/times out and agent action is needed |
| `blocked` | Waiting on a named external dependency/blocker | false while pending; true when resolved or timed out |
| `paused` | Explicitly paused by an authorized actor | false |
| `cancelled` | Explicitly cancelled | false |
| `exited_incomplete` | Agent released/finished without desired Integration status | true |
| `lost` | Heartbeat expired | true; no automatic reassignment |
| `stalled` | Heartbeat alive but no progress before threshold | true; lease remains valid and check requested |
| `completed` | Desired Integration status reached | false |

For unresolved `waiting_external`/`blocked`, store a typed reference and resolve
from authoritative RCP operation or explicitly refreshed external-dependency
state. On resolution/timeout, transition to `running` only if executor lease is
still live and request a wake; otherwise `lost` or `exited_incomplete` as
appropriate. `paused`, `cancelled`, and `completed` are terminal for the current
execution generation. Reassignment/restart is an explicit new claim.

## 3. Reconciliation timing and settings

Starting values (not yet empirically tuned):

| Setting | Starting value | Semantics |
|---|---:|---|
| Reconciliation interval | 30 s | Reconciler sweep cadence |
| Heartbeat interval | 60 s | Supervisor/agent heartbeat cadence |
| Lease TTL | 5 min | Renewed by heartbeat; expiry marks lost |
| Stalled timeout | 15 min | Time since meaningful progress; independent of lease expiry |
| Notification retry | 30 s | Initial outbox redelivery delay |
| Maximum retry backoff | 15 min | Exponential backoff cap |

Effective stalled timeout precedence: Integration override, then Product
override, then instance/global default (15 minutes). Missing means inherit;
explicit zero disables stalled detection only for that scope. Validate bounded
positive values and persist the effective config revision used by each sweep.
Lease and heartbeat values start globally configurable; Product/Integration
overrides are a later extension unless operational evidence requires them.

## 4. Execution Group

Recommend a new `ExecutionGroup` record, Product-scoped, with immutable member
Integration IDs at creation plus append-only membership revisions. Do not reuse
Feature, Composition, Gate, or dependency graph: Features may group work
semantically, Compositions select source/environment inputs, Gates represent
verification, and dependency edges have ordering semantics. Independent
Integrations must remain independent; group membership does not create
dependencies or shared readiness.

The group records orchestrator identity, label, created/closed timestamps,
current member set/revision, and optional overall note. `get_execution_group(id)`
returns every member (including completed ones), each Integration target,
execution/fence/heartbeat/progress, pending operation/blocker, action_required,
and `next_action` derived by RCP. It is the single restore-by-id call after
context loss. Group operations never overwrite a member's execution state.

## 5. Durable wake events and outbox

Add durable `ExecutionEvent` rows and a delivery outbox (or one row with
independent lifecycle and delivery fields). Required condition types:
`execution_lost`, `execution_stalled`, `execution_unassigned`,
`execution_continuation_required`. Include event id, Product/group/Integration,
execution fence, condition generation, reason, created/resolved timestamps,
`dedupe_key`, delivery attempts, `next_attempt_at`, and acknowledged timestamp.
Condition clear is server-derived from execution state; explicit resolve is
allowed for a supervisor only when it supplies a current fence and resolution
evidence. ACK means “supervisor received this event”; it does not resolve the
cause. Resolve/close must not erase or mutate event history.

Supervisor polls `list_execution_events {cursor, limit, unresolved_only}` and
receives stable cursor pagination. Unresolved conditions are redelivered until
ACK, while resolved-but-unacknowledged events remain queryable for audit. Use
dedupe key `(execution_id, fence, condition_type, condition_generation)` so
reconciliation does not enqueue duplicates. Retry with exponential backoff
30 seconds to 15 minutes; persist attempt and due time. An optional notification
service/Telegram adapter may escalate prolonged unacknowledged events to a
human; it is secondary and must not replace supervisor delivery. SSE can later
reduce latency but polling is the correctness path.

## 6. Supervisor and harness wake-up research

Commands below were inspected on this machine (2026-11-25). Wake probes were run
by the coordinator in a scratch directory on throwaway sessions only; real
sessions were not touched.

| Harness | Wake mechanism | Session ref registration | Verified here | Limitations |
|---|---|---|---|---|
| Codex CLI | `codex exec resume [OPTIONS] <SESSION_ID> "<prompt>"` (options before the id); `--last` selects newest recorded session. `codex resume` is interactive. | Orchestrator registers `thread_id` from its own `codex exec --json` `thread.started` event, or `CODEX_THREAD_ID`/notify payload in-session. Do not scrape rollout files. | **Yes (2026-11-25, coordinator):** `codex exec --json` created thread `01a2116d…`; `codex exec resume --json 01a2116d… "<question>"` ran a new turn on the same thread and recalled context from the first turn. | `--last` is unsafe for multi-project orchestrators; persist exact id. Concurrent open TUI on the same thread not tested; supervisor must serialize one wake per session and skip when a live process for that session is detected. |
| Claude Code CLI | `claude -p --resume <session-id> "<prompt>"`; `-c/--continue` selects latest conversation in cwd; `--fork-session` creates a distinct session. | `session_id` from SessionStart/Stop hook JSON payload or `--output-format json` result; or `--session-id <uuid>` at creation. | **Partially (2026-11-25):** `claude -p --output-format json` returns `session_id`, and `--resume <id>` keeps the same id; the turn itself failed with `Not logged in` because agent-guard strips the OAuth token from agent shells. | Supervisor needs its own owner-provisioned Claude auth (e.g. `claude setup-token` in the launchd environment); without it Claude wake is unsupported. Concurrency with an open interactive/desktop session on the same id is untested. |
| Claude Desktop | No documented external CLI/API wake established. Session may be resumed inside Claude Desktop/tools. | A bridge/tool running inside that session must register its session reference and expose a send-message capability. | No desktop session was opened or messaged. | No out-of-process reliable wake demonstrated; loop is incomplete without an in-session bridge or supported product API. |
| Claude Code Remote / cloud | `claude --cloud <session_id|url>` attaches; direct send requires an in-session tool/bridge such as `send_message`. | Register cloud session id/URL via the orchestrator or bridge; treat URL as sensitive locator. | CLI help documents cloud attach only; no message dispatch tested. | Attach is not equivalent to sending a prompt. Without a bridge that can message the session, the loop is incomplete. |
| CCR (Claude Code Router) / other | No stable generic wake protocol assumed. Adapter must define send/resume, session registration, idempotency and liveness. | Adapter-specific returned session id. | Not installed/probed. | Unsupported until adapter contract and concurrency behavior are tested. |

Do not scan `~/.codex` or Claude session stores for IDs: the owning orchestrator
must register its own exact session reference. A wake request includes group id,
Integration id, event id, and a short instruction to call `get_execution_group`,
check current work, then report state/progress. Do not assume the agent sees a
user message; no-user-message continuation is the supervisor's responsibility.

### Turn-end signal (primary continuation trigger)

An interactive or headless coding agent cannot heartbeat while it is idle
between turns, so heartbeat expiry alone would mark every idle orchestrator
`lost`. The decisive event for continuation is **turn end**:

- Claude Code: `Stop` hook (payload has `session_id`, `cwd`), installed by the
  supervisor's setup, calls `report_turn_end {session_ref}`.
- Codex: `notify` program on `agent-turn-complete` (payload has thread id) calls
  the same endpoint.

On turn end RCP looks up every execution whose harness `session_ref` matches,
renews nothing, and for each member of that orchestrator's groups that is not
`completed`/`paused`/`cancelled`/`waiting_external`/`blocked` and has not
reached `desired_state`, raises `execution_continuation_required` (after a short
debounce so the agent's own final RCP writes land first). Liveness while a turn
runs comes from hook-driven heartbeats (Claude `PostToolUse`, Codex progress
events) or from the supervisor observing the harness process for that session.
A turn-end report is advisory input; the server still derives the condition from
stored state.

## 7. Application API contract (one policy layer)

Expose equivalent MCP tools, HTTP JSON endpoints, and code-mode SDK methods over
one application command/query implementation. Every write includes Product and
Integration scope, actor labels, idempotency key where retryable, and current
execution fence; timestamps/lease expiry are server-owned. Never allow request
fields to set `action_required`, fence, or lease expiry.

| Operation | Required input | Result |
|---|---|---|
| `claim_execution` | `integration_id`, `orchestrator_id`, `agent_id`, `harness`, `desired_state`, optional `takeover_event_id`, `idempotency_key` | `{execution, fence, lease_until, takeover_required}`. Initial unassigned claim; an existing live lease returns `LEASE_HELD`. Lost/stalled takeover requires event/state-check evidence and increments fence. |
| `heartbeat_execution` | `integration_id`, `fence`, optional bounded progress receipt | `{state, fence, lease_until, last_heartbeat}`. Heartbeat does not by itself update last_progress. |
| `report_execution` | `integration_id`, `fence`, `state`, `reason`, `waiting_ref` or `blocker_ref` as required | `{execution, action_required}`. Allowed reports: `waiting_external`, `blocked`, `paused`, `exited_incomplete`; progress is a separate bounded receipt. |
| `release_execution` | `integration_id`, `fence`, `outcome: completed|exited_incomplete|paused|cancelled`, `reason` | State snapshot; completed only if Integration is already at desired state. |
| `takeover_execution` | `integration_id`, `orchestrator_id`, `agent_id`, `harness`, old fence, `state_check_event_id`, idempotency key | New lease/fence; old-fence invalidation and takeover event in same transaction. Explicit action only. |
| `set_desired_state` | `integration_id`, `fence`, target status, reason, idempotency key | Updated execution and recomputed completion. Must also pass existing Integration transition policy. |
| `create_execution_group` | `product_id`, `orchestrator_id`, `name`, `integration_ids[]` | `{group_id, revision, members[]}`; validate all IDs share Product. No dependency edges added. |
| `get_execution_group` | `group_id` | Full set and per-member execution/reconciliation/next action for context restoration. |
| `report_turn_end` | `harness.kind`, `session_ref`, optional `cwd` | `{matched_executions[], raised_events[]}`; idempotent per turn id. |
| `list_execution_events` | `product_id`, cursor, limit, unresolved filter | `{items[], next_cursor}`. |
| `ack_execution_event` | `event_id`, supervisor id, delivery id | Acknowledgement receipt only; does not clear condition. |
| `resolve_execution_event` | `event_id`, supervisor id, current fence, resolution evidence | `{resolved_at}` only if cause is cleared or explicitly resolved. |

Common errors: `NOT_FOUND`, `SCOPE_MISMATCH`, `INVALID_STATE`,
`STALE_FENCE`, `LEASE_HELD`, `LEASE_EXPIRED`, `TAKEOVER_CHECK_REQUIRED`,
`DESIRED_STATE_INVALID`, `WAITING_REFERENCE_REQUIRED`, `IDEMPOTENCY_CONFLICT`,
`CURSOR_INVALID`. Return stable machine code, safe message, and current fence/state
where safe. A stale request must be rejected even if its Integration body otherwise
matches. Reads remain Product scoped.

## 8. Staged delivery and acceptance

### Stage 1 — model, group, reconciler

Touch `internal/domain/model.go` (or a focused `execution.go` plus State arrays
and validation), `internal/application/` execution service and reconciler,
`internal/application/external_query.go`, `internal/transport/external.go`,
`internal/transport/server.go`, store schema/migrations and persistence
validation, and product configuration code. Add no worker-principal reuse.

Tests: unit state transitions/derived action_required, independent group members,
scope and transition validation, lost versus stalled (stalled never revokes),
pending/resolved operation semantics, takeover old-fence rejection, competing
transactions, restart persistence; bbolt and PostgreSQL transaction tests with
`TEST_DATABASE_URL` and isolated schemas. Done: group restore returns every
member; reconciliation detects conditions automatically; every RCP mutation
covered by execution fence is atomic; no automatic reassignment.

### Stage 2 — durable events and delivery

Touch event/outbox domain and migrations, reconciler condition generation,
application event queries/commands, MCP/HTTP/code-mode mappings, and retention
policy only if it preserves unresolved rows. UI may poll the same query.

Tests: dedupe, cursor stability, ack distinct from resolve, clear/timeout,
retry/backoff/restart recovery, duplicate delivery, append-only audit, Product
scope, PostgreSQL concurrent poll/ack/reconcile using `TEST_DATABASE_URL`. Done:
unresolved conditions redeliver durably; ACK never suppresses an unresolved cause;
resolved lifecycle is visible and immutable history retained.

### Stage 3 — supervisor and launchd

Add a separate macOS supervisor under `cmd/` or `bin/` (never inside RCP server),
launchd plist/template and operator documentation. The supervisor polls events,
deduplicates by event id, resolves registered harness adapter, wakes exact
session, records ACK after durable acceptance, and performs bounded retry. It
must not read session secrets, shell text supplied by RCP, or arbitrary commands.

Tests: adapter contract tests using fake CLIs, no-session and already-running
cases, timeout/retry/idempotency, launchd restart, safe logging/redaction, and
local integration against scratch sessions. Done: tested Codex and Claude Code
CLI wakeups with exact registered IDs; unsupported Desktop/Remote adapters are
reported as unsupported rather than falsely ACKed.

### Stage 4 — dev end-to-end

Use several independent Integrations in one group under one orchestrator;
deliberately remove some Integration context from the orchestrator's working
memory. Prove full group restoration, automatic detection without an agent
attention query, supervisor-triggered state-check prompt, resumed orchestrator
reload by group id and continuation without a user message, fencing of a stale
executor, and completed members not restarted. Keep live DEV acceptance separate
from implementation checks; no automatic production action.

Tests/evidence: repeatable E2E harness with event/outbox trace, group snapshots,
session IDs and per-member transitions; PostgreSQL run with `TEST_DATABASE_URL`
and isolated test schema; verify all listed commands from AGENTS.md for the
implementation stage. Done: above proof is captured and linked to the Product
acceptance record; Desktop/Remote remain explicitly incomplete until a working
bridge is tested.

## Stage 1 implementation notes

Current implementation uses the ordinary typed State collections so embedded and PostgreSQL stores persist executions and groups through their existing schema-derived record codec. Execution condition changes increment reconciliation.generation and append an audit Event, providing the stage 2 hook without adding the event/outbox model in this stage. Takeover stage 1 accepts a required state_check_reference string; stage 2 will bind it to durable event evidence.

## Open questions

1. Which existing Integration status values are valid `desired_state` targets
   for a given Product's acceptance policy, and who may set them?
2. Which provider branch protection can be required for managed branches across
   the supported Git providers, and what inventory freshness is adequate after
   takeover?
3. Does Codex's active interactive session accept `codex queue` reliably, or
   should wake use `exec resume` only? Verify both under serialized supervisor
   operation before Stage 3 claims support.
4. Which supported Claude hook/event provides the most reliable session ID
   registration, and what behavior occurs when CLI resume races Desktop/cloud?
5. Is a native authenticated supervisor identity needed beyond installation
   token + attributed orchestrator id, given current trusted-local API model?
6. Should stalled detection default on for every Integration, or only when an
   orchestrator opts in at group creation?
