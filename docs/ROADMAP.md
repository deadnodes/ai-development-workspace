# First-version delivery

This is an architectural implementation sequence, not a mutable progress log. Discover the relevant Feature in your configured instance for actual work, decisions, verification and handoffs. The initial GitHub execution implementation is documented in [GITHUB_DEV.md](GITHUB_DEV.md); live acceptance remains distinct from implementation.

## Existing phases

**Phase 1 — semantic development state:** working Product/Feature/Integration lifecycle, decisions/discoveries/blockers/progress/handoff, gates/results/findings, audit, PostgreSQL, embedded UI and shared API/MCP. Completion means ready, not deployed.

**Phase 2 — product topology and desired composition:** working product-local applications/repositories/environments, arbitrary environment names, target metadata, immutable caller-reported integration revisions, ordered composition snapshots and desired selection. Multiple products can coexist; this is logical scope consistency in a trusted workspace, not per-user access isolation. The existing `Application` is the requested Component. This phase originally stopped at intent. The subsequent GitHub slice adds source observation and DEV GitOps execution as explicit opt-in operations.

## Updated delivery priority

Next product slice: reusable test scenarios and immutable runs bound to existing environment composition snapshots. Next execution capability: independent environment reconciliation on selection changes (including feature removal): pinned base → fresh generated branch → CI → immutable artifacts → GitOps → runtime observation, with dependency/conflict and superseded-plan guards. It must work for any number of independently configured targets. Production follows selected-work integration into main, main-derived artifact build and explicit release-candidate verification, never blind DEV image promotion. See [source and verification rules](TEST_AND_RELEASE_FLOW.md). The provider rollout below supplies these workflows; it does not define a linear environment promotion chain.

## Next bounded slices

1. **Connections + observation:** persist connection grants/product bindings/secret references and topology policy; implement one read-only Git adapter (select GitHub or GitLab from the first real product). Observe branches/compare/checks, retain freshness and expose branch/attention queries in UI/MCP. Acceptance: a product cannot use another product's ungranted connection; compare against pinned refs; credentials never appear in state/audit.
2. **Managed branches + conflicts:** durable operation attempts and expected-head guards; narrowly scoped branch creation and permitted fast-forward first. Add isolated composition preview, multi-sided semantic conflict context, claim/resolution/evidence and generated branch retention. Acceptance: diverged branches never silently fast-forward; changed refs invalidate a plan; both sides' acceptance/decisions survive context generation; retries do not duplicate external writes.
3. **Artifact observation + retention:** one OCI registry adapter, persistent artifact/provenance/availability history, policy evaluation and UI/MCP warnings. No deletion. Acceptance: externally deleted images remain known; unreachable registry differs from confirmed missing; active/released/rollback pins are evaluated deterministically.
4. **External CI rebuild:** one CI adapter paired with the selected product, durable trigger/run correlation and secret references. Acceptance: available source/workflow and policy required; missing provenance yields an actionable blocker; changed rebuild digest creates a new artifact and plan revision instead of silent substitution.
5. **GitOps deployment + release evidence:** configurable mappings and concrete diff; approval bound to pinned plan; GitOps PR/commit publication; read-only Flux/Kubernetes observations. Acceptance: only correlated revision/digest/verification evidence permits all-component deployed state; partial failures remain visible; immutable successful ReleaseRecord separate from plan; restart/retry tested end-to-end.
6. **Additional adapters and ergonomics:** second Git/CI provider, Harbor/other OCI registries, policy controls, more observations, retention GC only after an explicit future scope. Avoid building all adapters together.

All slices use the existing modular monolith and domain/application layer. Provider interfaces are replaceable ports, not a plugin framework. External system runs remain external.

## First-version schema policy

There are no supported previous installations or public API versions during first-version development. Keep one current model and contract. Change provisional names and schemas directly when required; do not introduce compatibility aliases, legacy readers or parallel versions solely to preserve local prototype data. Local prototype records may be normalized or reset. Maintain the project's development context in the running instance after such changes.

Repository purposes are APPLICATION, LIBRARY, MIXED and GITOPS. Persist an explicit purpose and component kind. SOURCE is rejected. New application defaults are creation defaults, not fallback interpretations of incomplete persisted records.

Product history, event provenance and immutable execution evidence remain product requirements. Permission to replace bootstrap data does not authorize rewriting external Git history or concealing failed checks. Credentials and live provider verification are factual setup requirements, not schema compatibility blockers.

Target contracts become supported functionality only with concrete commands, persistence, scope validation, UI/MCP context and tests. Do not infer runtime health from desired GitOps state.

## Required validation for each execution capability

Use deterministic domain tests for scope/policy/transitions and real database tests for restart/concurrent updates. Adapter contract tests must exercise normalized not-found/permission/transient/conflict outcomes. Before enabling mutation, use a dedicated test repository/registry/environment and prove idempotent retry, stale-plan rejection and crash recovery. UI and MCP must show the same failure/provenance. Passing a mocked provider test is not proof of a real deployment.
