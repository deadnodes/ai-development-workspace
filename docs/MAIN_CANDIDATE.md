# Native main-derived build candidates

`prepare_release_candidate` accepts `main_sources` for **build-only native**
work. This selects an explicit already-merged main snapshot. It never merges a
branch, composes source, dispatches Actions, or writes GitOps. The legacy
`components` + `approve_main_update` route still belongs to GitHub Actions;
it remains unavailable for native components. Mixing these inputs is rejected.

```json
{
  "product_id": "product",
  "name": "Selected main artifact",
  "environment_id": "prod",
  "idempotency_key": "selected-main-17",
  "main_sources": [{
    "application_id": "application",
    "main_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "native": {
      "build_recipe_id": "application-build",
      "publication_recipe_id": "application-prod-publish",
      "parameters": {}
    },
    "revisions": [{
      "revision_id": "selected-revision",
      "pull_request_number": 17,
      "verification_result_ids": ["source-check-result"]
    }]
  }]
}
```

Use `POST /api/release-candidates`, the named MCP tool, or the same application
command. Lists are bounded to 20 components, 20 revisions per component and 100
result references per revision. The idempotency key is scoped to Product, exact
payload and attributed actor. An identical replay returns the original persisted
operation even after completion or restore; it does not refresh execution rights.

The configured physical numeric repository and PR base repository must match.
Every selected PR must be merged into the configured main/master, with the exact
captured head and a full merge SHA and merged timestamp. The selected main SHA
must equal one selected PR merge and contain the others. It can be a historical
ancestor of current main, including after source branch deletion. A subsequent
descendant main commit does not change the selected snapshot. A force rewrite or
changed PR identity blocks new admission.

Source evidence is `ATTRIBUTED_RCP_VERIFICATION`. Existing operator-trusted RCP
checks cover every check of the selected blocking source gates and match the
selected PR heads. Every other blocking gate still must pass its own current
commit/environment/deployment applicability, including Dev gates on a squash
main SHA. All blocking gate/check definitions, coverage and current result
identities are frozen.
Candidate readiness uses persisted result append order, so a later result with
an older timestamp invalidates an earlier reference. This evidence does **not**
attest GitHub human review, branch rules, squash compliance, or ownership of all
source changes. Findings, blockers, dependency/repository coverage and existing
gate commit/environment/deployment applicability still apply.

Each explicit native binding selects trusted recipes. The publication recipe
owns the effective artifact profile and image destination. The current global
`ComponentBuild` is frozen separately as the configuration baseline; selecting
a Prod recipe does not change the Dev binding. Both baseline and effective build
are bound through parent/snapshot/worker provenance. Recipe, configuration,
source evidence, grants, lease/deadline, cancellation and restore fences remain
active at managed worker and artifact admission boundaries. Git observations run
outside database transactions before child creation and around publication.
Physical registry bytes cannot be atomically rolled back across network races.

Children finish `ARTIFACT_READY`; the existing `RELEASE_CANDIDATE` parent finishes
`READY_FOR_VERIFICATION`. Neither state selects desired configuration, proves
runtime, accepts Dev, approves Prod, or marks a release. Even successful candidate
scenarios do not enable any native promotion route. Cancel an active parent or
child through `cancel_native_delivery`; in-flight physical work may still require
bounded reconciliation and is never blindly rerun.

The full shared-Dev/selective-Prod pilot, deployment authorization, protected Flux
merge policy, and runtime acceptance remain separate from this build-only path.
