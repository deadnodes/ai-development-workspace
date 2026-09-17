# Shared test compositions; independent releases from main

## Two separate flows

**Testing:** select integrations → plan a versioned environment composition → the configured Git provider assembles source on a disposable branch → CI builds test images → deploy to a shared test environment → execute scenarios and collect findings.

**Release:** select independently ready integrations → check dependencies and review evidence → an explicitly approved server operation fast-forwards only that selection into main → pin resulting main SHA → CI builds release images from that SHA → verify the release candidate → deploy those main-derived images to production.

Never promote the shared DEV branch wholesale or promote a DEV composition image merely because tests passed. The production artifact must have recorded provenance to the selected main commit. For multiple component repositories, pin a main SHA and resulting digest for each component. Already released unchanged components can retain their prior main-derived digest. A rollback can reuse a previously verified main-derived release artifact; it is not a merge of current DEV work.

## Example

DEV composition contains A + B + C for joint testing. Only A is ready for release.

1. Keep A, B and C's source provenance separately; the disposable DEV merge branch is not their source of truth.
2. Verify A's declared source dependencies. If A requires unreleased B, release is blocked unless the required foundation is separately released or the dependency is removed by an agent.
3. Prepare a candidate with explicit approval to fast-forward only A into configured main. Conflicts, protected-branch restrictions and a moved base stop the operation.
4. Build and verify a release candidate from the resulting main SHA. Its digest may differ from DEV and must be recorded independently.
5. Deploy that main-derived artifact. B and C remain active test work.
6. Re-plan DEV from the new main plus the still-active B and C revisions. Do not blindly reapply already integrated changes; the external executor must analyze actual ancestry/provenance.

A feature branch that already contains unrelated DEV changes cannot become independently releasable just by changing metadata. The agent must repair its source topology. Conflict resolution stays outside the metastore; required commit mappings and evidence are recorded here.

## Test scenarios

A Test Scenario describes **what behavior to verify**, independently of the generated branch name:

- Product, title and objective;
- relevant Features/Integrations and external interfaces;
- preconditions and test-data requirements (instructions/references, not business-data storage);
- steps and expected outcomes;
- execution mechanism and required checks;
- regression/compatibility obligations and blocking policy.

A Scenario Run binds a version of that scenario to an **immutable composition/deployment snapshot**: environment, selected integration revisions, component source SHAs, actual image digests, execution actor/time and evidence. Findings link to that run and affected integrations. Updating a scenario or desired composition does not rewrite prior runs.

Several scenarios can test different features on the same deployed A+B+C composition, including cross-feature scenarios. Their checks/results remain separate; the environment has one actual component-version set at a time. Selecting a different composition does not magically host both versions of the same component. Concurrent jobs that require incompatible fixtures or mutate shared test data must coordinate through the external test executor or use isolated namespaces/tenants. The Control Plane records these constraints, not the test data itself.

A gate groups required scenario/check evidence at an engineering boundary. Scenarios are behavior definitions; gates are readiness decisions; runs are immutable execution evidence. Reusable scenario definitions and immutable executions coexist with Gate/Check evidence; one does not silently replace the other.

## Verification applicability

A passing run on DEV A+B+C proves behavior on that exact composition. It does not prove A works alone on main. Release verification must explicitly cover the resulting main source/artifact set, including migrations, API compatibility and required regressions. Prior evidence can inform planning but cannot silently be relabelled as release-candidate evidence.

A green PR review is also not proof of runtime test success. P1/P2 Findings from either feature→dev or selected-work→main PRs stay linked to the relevant work and reviewed commit; their resolution and rerun requirements remain explicit.

## Control Plane boundary and current implementation

The application owns selection, scenarios/plans, source/artifact provenance, policy, context, findings and audit. Agents/Git providers change source/ref topology; CI builds/tests; GitOps/Flux deploy. No application-source editing or business-data processing moves into the service.

Implemented execution paths use durable parent/child operations: DEV/TEST composition, selected-work main candidate preparation, exact-digest production promotion, attributed runtime observations, versioned scenarios/runs and finding-linked hotfix creation. GitHub performs branch-only merges and fast-forward updates; CI builds, and the bounded GitOps writer updates configured image fields. The service does not edit application files or resolve source conflicts.

Implementation and deterministic provider/PostgreSQL tests are not proof of a successful live deployment. Inspect actual Git refs, Actions run/report, registry digest, GitOps commit and recorded runtime evidence for the configured installation. Runtime evidence is currently a human/agent assertion; no automatic Flux/Kubernetes collector or test executor is implied.

## Commands and evidence

Both named MCP tools and `execute` reach the same application commands. HTTP has equivalent named routes; the UI submits `/api/commands`. Every mutation requires `actor`.

| Command | Envelope and data | Outcome |
| --- | --- | --- |
| `assemble_environment` | `product_id`, `environment_id`, optional `integration_ids` | Create a fresh composition from latest active revisions and queue its COMPOSE parent; empty integration_ids means all active captured work |
| `reconcile_composition` | `id`: composition ID; `data: {}` | COMPOSE parent with child Git/build/GitOps work for the immutable selection |
| `prepare_release_candidate` | `product_id`; `data`: name, PROD environment_id, components, **approve_main_update: true** | Advances configured main branches and builds selected candidate artifacts |
| `promote_release_candidate` | `id`: candidate operation; `data: {approve: true}` | Reuses the verified candidate digests for the frozen PROD target |
| `record_runtime_observation` | `id`: deployment child; `data`: environment_id, gitops_commit, artifact_digest, healthy, details | Attributed exact-output runtime evidence |
| `create_hotfix` | `feature_id`; `data`: title, objective, optional finding_id, repositories, remaining | Planned hotfix integration preserving the finding link |
| `create_test_scenario` | `product_id`; `data`: title, objective, mechanism, preconditions, steps, expected_outcomes, integration_ids, blocking | Immutable first scenario version |
| `revise_test_scenario` | Same full definition plus scenario_id | New immutable version, retaining prior runs |
| `record_scenario_run` | `product_id`; exact version, target and component evidence below | Immutable observed execution and linked findings |

Named MCP tools use flat arguments: `composition_id`, `candidate_operation_id`, `operation_id`, `feature_id` or `product_id` instead of the command envelope’s `id`; their remaining data fields are top-level arguments. For example:

```json
{"name":"reconcile_composition","arguments":{"actor":"agent/release","composition_id":"COMPOSITION_ID"}}
```

A candidate’s `components` use the same shape as `plan_composition`: application_id, base_ref, full base_commit, generated target_branch and ordered revision_ids. The server generates a unique execution branch. Select ready/released work, include every repository of selected integrations, and include unreleased dependencies. Candidate preparation requires explicit PROD configuration and approval because it changes main **before** release verification. A failed later build/test does not undo main. Multi-repository updates are not an atomic Git transaction; inspect per-source evidence and repair externally if only part succeeds.

Private GHCR evidence expires after ten minutes. With `rebuild_missing` enabled, promotion dispatches a fresh correlated workflow probe after verification; it may reconstruct missing bytes but must report the already approved digest. A different digest stops promotion and requires a new verified candidate. With rebuild disabled, an unavailable or stale artifact proof blocks promotion.

Poll `get_operation` using the returned parent ID; inspect `child_ids`. Do not re-submit a mutation merely because a build is pending. Candidate children stop with `ARTIFACT_READY`; the parent becomes `SUCCEEDED / READY_FOR_VERIFICATION`. COMPOSE and RELEASE_PROMOTION parents require matching healthy runtime evidence for every child before `DEPLOYED`. A child’s `GITOPS_APPLIED` means desired state was written, not that runtime is healthy.

For a scenario run, provide `scenario_version_id`, exactly one of `composition_id` or `candidate_operation_id`, result (`passed`, `failed`, `blocked`), observations and/or artifacts, and optional finding_ids. Supply the **complete** exact component set:

```json
{"application_id":"APP_ID","operation_id":"CHILD_OPERATION_ID","source_sha":"FULL_BUILT_SOURCE_SHA","artifact_digest":"sha256:ACTUAL_DIGEST","deployment_evidence":"Observed environment and execution references"}
```

Composition runs require a completed deployed composition and per-component deployment evidence. Candidate runs require a built candidate; execution environment and per-component deployment evidence are optional, but actual observations/artifacts are mandatory. Source and artifact identities come from the child operation, never from a branch label. The latest blocking scenario versions must cover every candidate integration with fresh passing runs for that exact candidate. Open blockers/findings and ordinary readiness rules still apply. Changing a scenario definition requires fresh evidence; a DEV run cannot authorize production promotion.

## Multiple independent test targets

The testing flow applies independently to any number of configured environments, not one mandatory DEV branch. Each target selects its own integrations. Removing work requests a new base-plus-selection composition, fresh generated branch/CI build and deployment to that target only. See [environment reconciliation contract](ENVIRONMENT_COMPOSITION.md#independent-environment-reconciliation). Call `reconcile_composition` to execute the new immutable plan. Planning or selecting desired state alone does not start execution.
