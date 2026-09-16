# Incremental delivery and compatibility

This is an architectural implementation sequence, not a mutable progress log. Read application Feature `rcp-lifecycle` for actual work, decisions, verification and handoffs.

## Existing phases

**Phase 1 — semantic development state:** working Product/Feature/Integration lifecycle, decisions/discoveries/blockers/progress/handoff, gates/results/findings, audit, PostgreSQL, embedded UI and shared API/MCP. Completion means ready, not deployed.

**Phase 2 — product topology and desired composition:** working product-local applications/repositories/environments, arbitrary environment names, target metadata, immutable caller-reported integration revisions, ordered composition snapshots and desired selection. Multiple products can coexist; this is logical scope consistency in a trusted workspace, not per-user access isolation. The existing `Application` is the requested Component. Source verification and all provider execution remain future work.

## Next bounded slices

1. **Connections + observation:** persist connection grants/product bindings/secret references and topology policy; implement one read-only Git adapter (select GitHub or GitLab from the first real product). Observe branches/compare/checks, retain freshness and expose branch/attention queries in UI/MCP. Acceptance: a product cannot use another product's ungranted connection; compare against pinned refs; credentials never appear in state/audit.
2. **Managed branches + conflicts:** durable operation attempts and expected-head guards; narrowly scoped branch creation and permitted fast-forward first. Add isolated composition preview, multi-sided semantic conflict context, claim/resolution/evidence and generated branch retention. Acceptance: diverged branches never silently fast-forward; changed refs invalidate a plan; both sides' acceptance/decisions survive context generation; retries do not duplicate external writes.
3. **Artifact observation + retention:** one OCI registry adapter, persistent artifact/provenance/availability history, policy evaluation and UI/MCP warnings. No deletion. Acceptance: externally deleted images remain known; unreachable registry differs from confirmed missing; active/released/rollback pins are evaluated deterministically.
4. **External CI rebuild:** one CI adapter paired with the selected product, durable trigger/run correlation and secret references. Acceptance: available source/workflow and policy required; missing provenance yields an actionable blocker; changed rebuild digest creates a new artifact and plan revision instead of silent substitution.
5. **GitOps deployment + release evidence:** configurable mappings and concrete diff; approval bound to pinned plan; GitOps PR/commit publication; read-only Flux/Kubernetes observations. Acceptance: only correlated revision/digest/verification evidence permits all-component deployed state; partial failures remain visible; immutable successful ReleaseRecord separate from plan; restart/retry tested end-to-end.
6. **Additional adapters and ergonomics:** second Git/CI provider, Harbor/other OCI registries, policy controls, more observations, retention GC only after an explicit future scope. Avoid building all adapters together.

All slices use the existing modular monolith and domain/application layer. Provider interfaces are replaceable ports, not a plugin framework. External system runs remain external.

## Compatibility and migration strategy

- Keep persisted `applications`, `application_id`, `application_snapshots`, command `create_application` and current JSON responses. `Component` is an additive Go/domain alias now. A future `/components` name is a projection/alias first, with the same stable IDs.
- Current persistence groups documents by `records.kind` into `State` JSON keys; unknown kinds are not automatically usable. The current upsert does not change `kind`. A future storage rename therefore needs an explicit SQL migration plus versioned reader/projector, not just a Go type rename.
- Never rewrite historical event command payloads or immutable composition snapshots to modernize naming. Preserve their schema and introduce explicit versioned projections where necessary. Avoid mixed-version writers during any non-compatible storage migration.
- Current `Release` is a planned immutable selection. Preserve it as the legacy release-plan contract. Introduce final `ReleaseRecord` only with verified execution; do not mark old plans released during migration.
- New lifecycle entities in `internal/domain/lifecycle.go` are target contracts. They are not added to `State` or stored automatically. Each activation must add concrete command/query schemas, persisted kinds/indexes/migrations, scoping validation, provenance, transport parity tests and UI evidence together.
- Provider connection/credential migration is additive: existing repository.provider and local environment.cluster remain descriptive legacy values until an explicit connection binding is configured. No guessing credentials or implicitly reusing a connection across products.
- Do not infer environment purpose/promotion order from existing names. Existing unknown observations remain unknown; absent connection/policy means execution unavailable.
- Secrets are references resolved at execution time through an eventual secret backend. Store only sanitized parameters and secret reference identifiers in plans/audit. Token injection by a UI/API request is never a normal domain write.
- Introduce outbox/operation rows with expected external refs and idempotency IDs when enabling the first write adapter. No external request while holding a SQL mutation lock. Version pinned policy/configuration and approvals with execution plans.

## Required validation for each execution capability

Use deterministic domain tests for scope/policy/transitions and real database tests for restart/concurrent updates. Adapter contract tests must exercise normalized not-found/permission/transient/conflict outcomes. Before enabling mutation, use a dedicated test repository/registry/environment and prove idempotent retry, stale-plan rejection and crash recovery. UI and MCP must show the same failure/provenance. Passing a mocked provider test is not proof of a real deployment.
