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

1. `claim_composition_conflict`: product_id plus data.conflict_id. The actor owns the claim.
2. Resolve externally without dropping either side's history or acceptance criteria.
3. `record_conflict_resolution`: same product_id, owner actor, data.conflict_id, commit (full SHA), rationale.
4. Record passing check results on that exact commit using normal gates/checks. Every affected integration needs scoped evidence, including separate integrations of one feature. Every blocking check needs a passing result.
5. `verify_conflict_resolution`: product_id, data.conflict_id, result_ids (array of passing result IDs).
6. `reconcile_composition`: id of the original composition, data.resolution_conflict_ids array. This creates a new operation. It never restarts a historical failed operation. Both worker and GitHub adapter check resolution ancestry contains the base and every selected head before creating the generated branch and running the normal build/deploy path.

Verification alone does not deploy or alter source. The rebuilt composition must use the exact original selected inputs; different selections need fresh conflict analysis. Production candidates still use their explicit approval and verification flow; resolved DEV context is not production approval.
