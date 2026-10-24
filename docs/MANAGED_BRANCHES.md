# Managed branches and conflict resolution

All commands use `POST /api/commands` or MCP `execute` and are audited. MCP also exposes named `create_integration_branch` and `protect_integration_revision` tools. UI controls are on Environments & delivery.

Create a developer branch (integration must already reference the component's repository):

```json
{"action":"create_integration_branch","actor":"agent/dev","integration_id":"integration-id","data":{"application_id":"component-id","base_commit":"FULL_CURRENT_MAIN_SHA","approve":true}}
```

Returns a persistent operation immediately. The worker verifies the configured base has not moved, creates `feature/rcp-<integration-id>` and binds it. Existing different history is never overwritten. Existing bound integrations and released integrations cannot use this command.

Retain a captured revision independently of the developer branch:

```json
{"action":"protect_integration_revision","actor":"agent/dev","integration_id":"integration-id","data":{"application_id":"component-id","revision_id":"revision-id","approve":true}}
```

This creates `rcp/revisions/<revision-id>` at the exact captured SHA. Protection means application-enforced create-only behavior: the service never changes that ref. This does not install GitHub administrator-proof rulesets. External administrators may still change or delete it; provider mismatches fail closed.

A composition merge conflict creates a persisted `composition_conflicts` record with the original source plan and both sides' feature/integration/memory snapshots. Feature/integration context includes relevant records.

The agent that requested the build/merge polls `get_operation`. On a persisted
textual conflict, its response includes `agent_action.type: RESOLVE_CONFLICT`
and `status: ACTION_REQUIRED`, with the original composition/conflict/repository,
base ref/SHA, generated target branch and ordered revision IDs, Feature/Integration
IDs, origin branches, exact head SHAs and required check IDs. The **requesting
agent** resolves the code externally; RCP does not spawn or manage another agent.
`repository_id` is the Product attachment; `provider_repository` is the exact
provider address and may be a numeric repository ID. Use `get_project_context`
for the registered repository URL, rather than treating that address as a clone URL.
Read each Feature's intent and Integration acceptance with `resume` and
`get_integration_context`. Preserve every participating feature.

Origin branch names are historical labels, not evidence that a current remote
branch still points to the captured SHA. An optional retained ref has its stored
observation time; the query makes no provider request or current availability
claim. If required identities are absent, ambiguous or exceed limits, the action
is `UNAVAILABLE` with no partial executable context. Do not guess the missing
input or retry deployment. The new projection has at most 20 conflicts, 20 inputs
and 128 checks per conflict and 32 KiB serialized JSON; identifiers are never
truncated. Existing operation/steps/children/feature contexts retain their prior
contracts and are **not** covered by that byte limit.

1. `claim_composition_conflict`: product_id plus data.conflict_id. The actor owns the claim.
2. Resolve externally without dropping either side's history or acceptance criteria.
3. `record_conflict_resolution`: same product_id, owner actor, data.conflict_id, commit (full SHA), rationale.
4. Record passing check results on that exact commit using normal gates/checks. Every affected integration needs scoped evidence, including separate integrations of one feature. Every blocking check needs a passing result.
5. `verify_conflict_resolution`: product_id, data.conflict_id, result_ids (array of passing result IDs).
6. `reconcile_composition`: id of the original composition, data.resolution_conflict_ids array. This creates a new operation. It never restarts a historical failed operation. Both worker and GitHub adapter check resolution ancestry contains the base and every selected head before creating the generated branch and running the normal build/deploy path.

Resolution evidence is checked again at reconciliation admission and through
the existing queued parent/child and mutation guards. A stored `resolved` label
does not preserve permission after a referenced result is superseded, disappears,
changes scope, or no longer covers current blocking checks. Latest means stored
append order; a caller timestamp cannot revive an older result.

After running fresh checks on the **same immutable resolution commit**, call
`verify_conflict_resolution` again with the current passing result IDs. This is
also accepted for an already `resolved` conflict. It preserves the original
claim, source, rationale, commit and historical results/events. It does not
restart stopped work or queue another build: request a new explicit, authorized
`reconcile_composition` after verification. Rejection leaves the requested
attachment and command state unchanged; a worker may record its blocked outcome.
These guards cannot retract a provider request already sent. Existing uncertain
write and reservation recovery still applies.

Verification alone does not deploy or alter source. The rebuilt composition must use the exact original selected inputs; different selections need fresh conflict analysis. Production candidates still use their explicit approval and verification flow; resolved DEV context is not production approval.

The handoff reports the recorded conflict status and conditional workflow; a
read has no caller identity and cannot establish claim ownership. Instructions
and actor labels grant no new permissions. In particular, `reconcile_composition`
may start builds and configured DEV/TEST GitOps preparation, so it needs its own
existing execution authorization. Clean merges failing semantic checks,
independent review authority and semantic changes to goals or check instructions
without new identities remain separate stage-06 work. Current result/coverage
revalidation does not prove those semantics or live delivery.

## Failed builds after a clean source composition

When a COMPOSE operation has an accepted failed managed native build report,
`get_operation.agent_action` can instead return `INVESTIGATE_BUILD_FAILURE`.
It identifies every accepted failed native build, delivery and worker operation, exact job,
attempt, fence, input hash, generated source SHA, recipe/config identities, and
the frozen base and ordered feature revisions. It supplies identifiers for
`get_worker_job`, `get_operation`, `resume` and `get_integration_context`.
The latter semantic context is current: it is not a frozen copy of the goals or
decisions at execution time. Retained refs remain dated observations.

The cause is **unclassified**. Both `CHECK_FAILED` and `EXECUTOR_FAILED` are
preserved as `reported_result`. Currently a Docker execution error can also
produce `CHECK_FAILED`; neither label proves a semantic conflict or incompatible
feature intent. An agent first investigates the recorded attempt and exact
source. It preserves every feature's acceptance criteria and asks the human
when the intended behavior cannot be reconciled.

The historical join validates the unique parent, listed delivery, worker
operation, managed build, current-fence attempt and accepted report, including
the parent's frozen recipe/configuration and source proof. The existing
`worker_report` audit anchors the original admitted input hash and fence;
recomputed job hashes cannot substitute a different execution payload. Missing
attempt/report data for an admitted failure makes the complete set unavailable.
This is a consistency check, not protection against rewriting the entire store.
Old lease expiry and
current operator configuration do not erase an already accepted report. They
also confer no current execution permission. Jobs stopped without an accepted
report, cancellation, registry publication and check-finalization failures are
not classified as accepted build failures.

Existing textual `RESOLVE_CONFLICT` responses take precedence, including an
`UNAVAILABLE` textual response. Multiple failed builds produce the complete
bounded set or `UNAVAILABLE`, never a partial list. The investigation block has
the same 32-KiB JSON limit and identifier validation; recipe argv, parameters,
private configuration, raw reports/logs, actor labels and feature memory are not
copied. This bound does not cover the complete legacy operation reply.

Reading this action changes no state and starts no worker, retry, source edit,
reconciliation or GitOps operation. Record findings and exact evidence through
existing progress/discovery commands. This is an investigation handoff, not
automatic semantic-conflict registration: do not invoke textual conflict
commands without a real conflict record. Independently reviewed semantic
resolution, trusted review authority and the live A+B/selective-release pilot
remain separate unfinished acceptance criteria.
