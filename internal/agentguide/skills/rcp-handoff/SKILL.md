---
name: rcp-handoff
description: Use Release Control Plane as the canonical current-work context for tracked DeadNodes features, keeping product intent, integrations, progress, evidence, and handoffs synchronized across sessions.
---

# Release Control Plane workflow

Local Codex persistence enforcement is described in
`.workspace/RCP_LOCAL_HOOKS.md`. Save both `record_progress` and `handoff` for the
same real Feature/Integration and read their IDs back via MCP `resume` after
the latest work, before publishing or successful completion. Publishing creates
another checkpoint. Do not manufacture proof or bypass a blocked hook.

Use this skill for any DeadNodes work tracked by a Release Control Plane
Feature. RCP is the source of truth for the current Feature and Integration
state; chat history, local memory, Linear, and repository branches are
supporting context. This skill does not authorize deployments, pushes,
production changes, or new tasks.

## Start with current RCP state

Before implementation, deployment, or broad investigation:

1. Read the workspace binding and use `get_project_context` to identify the
   configured Product.
2. Call `resume` for the relevant Feature.
3. Select the actual Integration being worked on, confirm its owner and scope,
   and record a plan/start event before editing when one is missing.
4. Treat the latest explicit user direction as authoritative. Treat old checks,
   progress entries, and handoffs as historical evidence until the active plan
   confirms that they still apply.

## Synchronize immediately when scope changes

RCP history is append-only and does not infer that a product decision changed
the active Feature plan. A progress event alone is not enough when scope
changes.

When the user changes a price, credit amount, UX flow, entitlement rule,
status matrix, API behavior, or other acceptance behavior:

1. Stop and update the Feature's active `goal`, `requirements`, `constraints`,
   and `context` through `update_feature`.
2. Update every affected Integration's `title`, `objective`,
   `acceptance_criteria`, and `remaining` through `update_integration`.
3. Record the decision or discovery explaining what replaced the old contract.
   Historical checks, progress, and handoffs stay immutable, but stale
   requirements must not remain in the active plan. If RCP has no superseded
   marker, remove the old requirement from active arrays and reference it as
   historical context.
4. Re-read `resume` before continuing. Do not implement while the active
   Feature or Integration criteria still describe the old behavior.

Never silently preserve an earlier implementation plan just because code or
checks already exist. Reconcile the plan first, then decide whether the code,
checks, or deployment evidence must be revised.

## Record implementation and delivery truthfully

After a meaningful implementation checkpoint, merge, deployment, or test:

- record concrete `completed` and `remaining` work through RCP;
- attach exact commits, PRs, workflow runs, artifact digests, GitOps commits,
  runtime observations, or check IDs when available;
- transition each Integration to its truthful status: `working`, `implemented`,
  `verifying`, or `ready`;
- keep manual acceptance, runtime smoke, and unresolved policy decisions
  explicit rather than hiding them in a final note.

`ready` requires the normal blocking gates and is not a synonym for
implemented. The Feature workspace's `ready/released` count is a readiness
metric, not a productivity metric. Implementation progress must be visible in
Integration statuses and progress events.

Do not infer runtime health, release, or production readiness from a handoff,
successful unit tests, a healthy pod, a GitOps commit, or a DEV deployment
alone. Record each kind of evidence separately and label assumptions.

## Finish or pause with a verified handoff

When pausing, transferring context, or finishing tracked work:

1. Reconcile every affected Integration with the actual current acceptance
   criteria, completed work, remaining work, and status.
2. Audit `resume` for stale active requirements, stale next actions, and
   incorrect statuses. Fix the active plan before recording the handoff.
3. Record a concise `handoff` with the current stopping point, exact remaining
   work, first next action, warnings, and references to existing evidence.
4. Call `resume` again and verify that the handoff, scope, and next actions are
   visible and consistent.

Do not mark a Feature complete unless all of its Integrations satisfy the
normal RCP readiness requirements. Do not call work blocked merely because it
is incomplete; use `blocked` only for a real unresolved execution blocker.
