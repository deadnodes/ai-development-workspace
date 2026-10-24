# Dynamic environments and independently traceable feature composition

## Product boundaries
Every Product owns its own named Environments. Names are data, not an enum or mandatory promotion chain. Product A may use DEV/PROD while B uses DEV/TEST/STAGE/PROD. Adding a target records cluster/namespace coordinates; it does not create a cluster or namespace. Independent products may share a physical cluster, but their logical plans and feature selections remain isolated. Cross-product bundles would be an explicit future object, never an implicit merge of unrelated products.

Application identifies a deployable part of a product, linked to a repository and optional monorepo path. Multiple applications can share a repository. Environment identifies where; Composition identifies what should run there.

## Source identity versus integration branch history
IntegrationRevision is an immutable capture of one integration's source for one repository: branch, pinned base SHA, head SHA, and ordered commit identifiers. A new capture creates a new revision; it never replaces an earlier revision. Multiple repositories yield separate captures for the same integration.

A composition pins a base commit and an ordered selection of revision IDs for every application's repository. The Git worker derives a disposable `generated/...` branch from those inputs. Multiple features may therefore coexist in the same integration branch while their individual source history remains independent of merge commits. Applications sharing a repository must agree on its base, target branch and ordered revision selection.

Example: DEV revision 12 contains main@M + PAY/INT-03@P + AUTH/INT-02@A. DEV revision 13 can contain main@M2 + PAY/INT-03@P2, excluding AUTH. Revision 12 and the P/A source captures remain unchanged. Recomposition starts from the chosen base; removing AUTH is not a chain of guessed revert commits.

Recording SHAs alone does not retain Git objects. New explicit DEV/TEST PR-only
COMPOSE operations freeze exact retained refs `rcp/revisions/<revision-id>` for
all selected immutable captures, then create only absent refs and read back exact
SHAs before merging. The operation stores timestamped retention observations;
manual revision records remain caller-reported. A current reachable ref is not
repository-side branch protection or eternal object retention. Foreign writers
can still move/delete refs; later source boundaries detect that drift and block.
The existing explicit `protect_integration_revision` action still requires
`approve: true`; its approval is not reused as permission for another operation.
Feature source branches should start from main and declare their dependencies.


## Planning slice implemented now
- Create applications and arbitrary environment targets.
- Record immutable per-integration repository revisions with full SHA identifiers.
- Plan a named composition with application rows, exact base, generated branch and selected revisions.
- Preserve application, environment and source revision snapshots in every plan.
- Select an existing immutable plan as the environment's desired composition; audit the change.
- Reject cross-product references, duplicate application/revision selections, incompatible same-repository plans and missing unreleased dependencies.
- Keep desired selection separate from reconciled/runtime observations. A selected plan is still `planned`, not deployed or healthy.

Selecting an older plan changes only desired intent. It is not a verified runtime rollback. Creating an empty revision selection allows a base-only application version. Feature gates/readiness do not prohibit planning unfinished work on DEV. Production eligibility is not inferred from an environment's name; execution/promotion policy must be configured before production automation is enabled.

## Independent release boundary

Shared test compositions are not promoted as production artifacts. Production candidates are built from pinned main commits after only selected completed integrations are integrated by an explicitly approved branch-only merge/fast-forward operation. Verification of a mixed DEV composition does not automatically verify that narrower main candidate. See [test scenarios and release flow](TEST_AND_RELEASE_FLOW.md).

## Execution slice
On a deployment request: resolve and verify immutable source refs → isolated composition/merge preview → conflict report or generated commit → build each affected application → retain image digests and test evidence → update GitOps desired state → record attributed runtime evidence. Automatic Flux/Kubernetes collection remains future work. A Deployment record must link the composition ID, repository outputs, artifact digests and observed outcome. Merge conflicts stop the operation for a human/agent resolution; they are not automatically guessed away. Partial rollout remains distinguishable from success.

### Patch one feature while keeping shared work pinned

Use `update_environment_feature` with top-level `feature_id` and data:

```json
{
  "environment_id": "dev",
  "revision_ids": ["A2"],
  "idempotency_key": "feature-A-update-2026-10-22",
  "expected_selection": {"composition_id": "A1-B1", "operation_id": "current-compose", "generation": 12}
}
```

Read all three expected values from `get_environment_state.environment`. The
command atomically changes A1+B1 to A2+B1, preserving the pinned main base, merge
order and selected application cohort even if B2 and main M2 have been observed.
Only explicit revision pairs of the target Feature are patched; truly new pairs
within an existing selected repository are appended in caller order. Shared-repo
applications keep identical ordered selections. New repositories/applications,
removals, base changes and released target work require an explicit baseline
replan. A missing baseline is never invented. Dependency validation still applies.

The command returns the ordinary COMPOSE parent and freezes current build/target
configuration through existing delivery preparation. It requires configured,
enabled DEV/TEST reviewed PR-only GitOps targets for every selected component and
current execution authority; production and active uncertain GitOps writes refuse.
It performs no provider calls inside the transaction. Current main-first delivery
policy still governs live execution; this source capability grants no permission
for pre-main deployment, protected merges or production promotion.

Selection CAS includes a monotonic `selection_generation`, advanced by every
selection, reconcile and target reset, even an identical selection. A stale request
leaves no new composition, operation, receipt or audit. Reread current desired
intent and explicitly reapply; the server never retries a conflicting first patch.
Overflow refuses atomically; observations never change this generation.

The nonempty bounded opaque idempotency key binds actor, Product, Feature,
environment and exact typed payload (including revision order and CAS). Exact
replay returns the same historical operation after supersession, reopen or backup
restore, without reselecting or creating events. Conflicting reuse refuses. Replay
grants no execution permission. An unchanged patch under a new key refuses with
the current operation/composition and explicit reconcile guidance.

Every patched composition has `auto_refresh: false`. New B2/main M2 observations
cannot change A2+B1/base M, including after COMPOSE completes. A later explicit
full replacement is a separate intent. Prior compositions and snapshots remain
immutable; observed runtime/reconciled state stays separate. This slice preserves
pinned source inputs and current request-time configuration; it does not prove
historical build/config reconstruction, retained source objects or real rollout.

### Assemble active work (explicit full refresh)

Agents and the UI can use `assemble_environment` when several developers need one
shared test target. The command accepts `product_id` and `data.environment_id`,
with optional `name` and `integration_ids`. If the list is empty, the service
selects all active integrations that have captured Git revisions. For every
selected repository it chooses the newest captured revision, pins the latest
observed `main` commit as the base, groups all integrations for that repository
in the requested order, and creates one immutable Composition. It then queues
the normal asynchronous `COMPOSE` operation and returns its operation ID.

The worker creates disposable `generated/<environment>/<operation>/<repository>`
branches, merges the pinned integration heads, builds the resulting source,
updates the configured GitOps file and records the commit. The generated branch
name is an output, never the source of truth. Poll `get_operation` until the
parent and child steps finish. A source conflict changes the operation to
`BLOCKED` and creates a semantic conflict record; claim and resolve that record
through MCP before retrying. New pushes are picked up by the background Git
refresh. When an environment already has this composition selected, the
background scheduler queues one new assembly after a newer
revision is observed only when its immutable `auto_refresh` boolean is explicitly
true. `assemble_environment` sets that boolean true; manual `plan_composition`
defaults false and may explicitly opt in. Scheduler refresh carries the captured
composition, operation and generation into the transaction, so stale snapshots
skip without new work or audit. To replace pinned feature updates with the latest
full selection, explicitly run `assemble_environment` again. Older compositions and their
operation evidence remain available for rollback or comparison.

Production stays separate: use the main-derived release-candidate flow and an
explicit promotion click. A DEV composition never becomes a production release
merely because its generated branch was built.

The UI/API/MCP expose `reconcile_composition`, which queues a durable COMPOSE parent and per-component deployment children. Release candidate preparation and exact-digest promotion use separate operations and explicit approvals. Commands, evidence requirements and current limitations are documented in [test scenarios and release flow](TEST_AND_RELEASE_FLOW.md#commands-and-evidence).

## Retained source execution and drift boundaries

Explicit `assemble_environment`, `reconcile_composition` and
`update_environment_feature` requests now establish typed retention intent only
for configured enabled DEV/TEST PR-only targets. Intent binds Product, parent,
repository, full source plan/connection/configuration and ordered immutable
revision IDs to exact `rcp/revisions/<revision-id>` names and head SHAs. Invalid
or oversized derived names and duplicate-head ownership refuse atomically.
Same-SHA captures in separate operations retain distinct revision-ID refs.
Older operations without intent fail closed with new-operation guidance.
No public field or actor string can supply retention authority or lineage.

The normal COMPOSE worker uses existing create-only `EnsureRef` and scoped ref
readback. One missing revision is pinned and admitted per durable tick. Existing
progress is reused after reopen or a lost create response. Admitted missing/moved
refs block rather than being recreated from old evidence. Full declared source
history is independently validated, so a valid ref never proves an integration
contains only its declared work. Every call uses Tick's bounded 25-second context;
success admission requires its current unexpired 45-second lease, selected parent,
frozen bindings/connections/grants and native execution authority, even when no
new worker has replaced the same owner token. Late remote creation can leave an
exact orphan ref; it cannot admit progress or queue children after supersession.
Conflict results also require the current unexpired lease and frozen authority:
conflict context, audit and the parent blocked outcome are admitted atomically.

A read-only source verifier reuses the production adapter's exact step refs,
commit parents/message, ancestry and final result identity. It never calls
composition/merge/ref creation as a verification fallback. Saved source results
are reread before consumption and all retained/generated identities are reread
again at the actual child-queue boundary, outside the database transaction. The
same transaction that creates children compares the checked frozen snapshot and
current lease/authority. Missing pins, final/step drift, malformed proof or provider
failure preserve prior runtime and immutable history while blocking new builds.
These checks prove current structural provider provenance under the trusted Git
provider boundary; parents/messages are not cryptographic proof of merge trees,
and the checks do not lock remote Git against changes after the observation.

Automatic full refresh may inherit retention authority only through a private
server call from the currently selected parent's valid explicit intent and exact
selection CAS, preserving that parent's selected integration scope. Legacy
`auto_refresh: true` selections without intent create no operation, refs or audit.
A new explicit request can establish authority before later automatic refresh.
Creation authority is checked by ordered append-only command/restore provenance
for the complete ancestor chain through the explicit root, rather than clocks
from different hosts. Missing, ambiguous or foreign lineage fails closed.
Cancelled or pre-restore intent cannot restart automatic writes; restored work
remains historical and existing restore quarantine still applies. Pinned feature
patches keep `auto_refresh: false`. Production/main advancement rules and source
worker/mirror ingestion remain separate from this source capability.

## Independent environment reconciliation

Environment count is unbounded configuration: one, two, three or more targets. Targets do not form an implicit DEV → TEST → STAGE → PROD promotion pipeline. Each owns an independent desired composition and reconciliation operation.

Example:

| Target | Desired source |
| --- | --- |
| test-a | main@M + A@A1 + B@B2 |
| test-b | main@M + B@B2 + C@C1 |
| test-c | main@M + A@A1 |
| production | pinned main commits only |

Adding/removing selected work changes only the chosen environment's desired composition. Removing A from test-a produces a new immutable definition `main@M + B@B2`; test-b/test-c are unchanged. No application data is deleted. A feature already included in the chosen main baseline cannot be removed merely by deselecting an active integration: that requires an explicit different baseline or a source change by an agent/developer.

### Required execution operation

1. Validate selected revision scope, dependencies and test-data/migration constraints. Removing a required dependency is rejected or requires an explicit revised plan; do not silently cascade-remove unrelated work.
2. Freeze a new composition revision: exact base commit and ordered integration source revisions for each repository, component build definitions, environment mapping and policy.
3. Ask the external Git executor to create a fresh generated branch from the pinned base, for example `generated/<environment-id>/<composition-id>`. Apply only selected work. Do not mutate developer branches or try to remove work by a chain of guessed reverts.
4. On conflicts, preserve the previous running environment, record conflict evidence and give an agent both contexts. Do not attempt semantic source fixes in the Control Plane.
5. Record generated commit(s), trigger external CI on those exact SHAs and resolve immutable artifact digests.
6. Before publishing desired GitOps, verify that this operation still targets the latest desired composition. A slow old build must not reintroduce a feature removed by a newer request.
7. Publish the pinned component artifacts through the configured environment mapping, then observe Flux/runtime separately. Failures and partial multi-component rollouts remain explicit; a GitOps commit is not runtime success.
8. Retain the prior composition, generated commits, artifacts and test evidence for diagnosis/rollback. Do not silently change business data or undo migrations when removing a feature; incompatible persisted data can block rollback/recomposition.

Generated branches are disposable materializations of the database definition. Git stores their code/history; the Control Plane records how to reconstruct them. Build caching is allowed only for matching recorded source/build provenance and immutable digest, never by mutable branch/tag name alone.

Production keeps the distinct policy: build from pinned main source after selected work has landed there. Test-like targets use composition sources regardless of their display names. Supporting arbitrary environment names does not enable arbitrary production writes.

### Current behavior and limits

Planning and `select_composition` change desired intent only. Explicit `reconcile_composition` starts Git composition, component builds and configured DEV/TEST GitOps updates. Superseded-operation guards check desired identity before publishing. A GitOps write is not deployment success: the parent requires matching healthy observations for all child outputs. Observations are currently caller-attested; the service does not query Flux/Kubernetes or execute test scenarios automatically.

Production uses `prepare_release_candidate` with explicit main-update approval, fresh exact-candidate scenario evidence, and `promote_release_candidate` with explicit promotion approval. Multi-repository Git updates and multi-component rollouts are not atomic; inspect child/source evidence after partial failure. Deterministic tests establish implementation behavior, not a live installation’s deployment success. See [the operational guide](TEST_AND_RELEASE_FLOW.md).
