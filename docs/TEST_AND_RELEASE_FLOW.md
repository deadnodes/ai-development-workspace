# Shared test compositions; independent releases from main

## Two separate flows

**Testing:** select integrations → plan a versioned environment composition → external Git executor assembles source → CI builds test images → deploy to a shared test environment → execute scenarios and collect findings.

**Release:** select independently ready integrations → check dependencies and review evidence → external Git workflow merges only that selection into main → pin resulting main SHA → CI builds release images from that SHA → verify the release candidate → deploy those main-derived images to production.

Never promote the shared DEV branch wholesale or promote a DEV composition image merely because tests passed. The production artifact must have recorded provenance to the selected main commit. For multiple component repositories, pin a main SHA and resulting digest for each component. Already released unchanged components can retain their prior main-derived digest. A rollback can reuse a previously verified main-derived release artifact; it is not a merge of current DEV work.

## Example

DEV composition contains A + B + C for joint testing. Only A is ready for release.

1. Keep A, B and C's source provenance separately; the disposable DEV merge branch is not their source of truth.
2. Verify A's declared source dependencies. If A requires unreleased B, release is blocked unless the required foundation is separately released or the dependency is removed by an agent.
3. Merge only A into main through the configured external Git/PR workflow.
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

A gate groups required scenario/check evidence at an engineering boundary. Scenarios are behavior definitions; gates are readiness decisions; runs are immutable execution evidence. Existing Gate/Check objects are the initial building blocks, not yet a complete reusable scenario library.

## Verification applicability

A passing run on DEV A+B+C proves behavior on that exact composition. It does not prove A works alone on main. Release verification must explicitly cover the resulting main source/artifact set, including migrations, API compatibility and required regressions. Prior evidence can inform planning but cannot silently be relabelled as release-candidate evidence.

A green PR review is also not proof of runtime test success. P1/P2 Findings from either feature→dev or selected-work→main PRs stay linked to the relevant work and reviewed commit; their resolution and rerun requirements remain explicit.

## Control Plane boundary and current implementation

The application owns selection, scenarios/plans, source/artifact provenance, policy, context, findings and audit. Agents/Git providers change source/ref topology; CI builds/tests; GitOps/Flux deploy. No application-source editing or business-data processing moves into the service.

Implemented foundations: versioned environment composition plans, per-integration source revisions, Gates/Checks/results, review Findings, immutable release selection plans, and one explicit GitHub DEV integration deployment path.

Still to implement: reusable versioned scenarios/runs, automatic external execution of multi-integration compositions, selected-work merge-to-main orchestration, main-source release provenance enforcement and release-candidate verification/execution. The current DEV deployment command operates on one bound integration and does not execute a multi-feature composition. Production execution remains unavailable. This document sets the target behavior; it does not claim those operations are already shipped.
