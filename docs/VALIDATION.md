# MVP validation

Validated locally on 2026-09-16.

- `TEST_DATABASE_URL=... make check`: gofmt check, Go vet, frontend syntax and renderer/serialization checks, all race-enabled Go tests, standalone binary build.
- Real PostgreSQL test isolation uses a fresh schema; it exercises the whole HTTP workflow, official MCP Go client initialization/tool listing/handoff/resume, two database connections with 12 concurrent writes, rejected-command rollback, and reopened storage equality.
- Domain tests exercise dependency cycles, scope isolation, evidence provenance, blockers, finding resolution plus required rerun, status/metadata bypasses, historical-ID collisions, immutable release-plan snapshots, release dependency inclusion, compact audit and multiple integration handoffs.
- Browser validation exercised product and feature creation, integration planning/start, gate/check creation, failed result, finding creation/resolution, passing rerun, completion, structured handoff, feature-level decision and resume context. Both executions remain visible and the resolved finding retains its history. Responsive wide/narrow layouts inspected; no horizontal DOM overflow observed. Browser console had no warnings/errors.
- Frontend regression checks exercise output escaping, evidence history, form availability, optional integration-ID serialization, and skip-link focus without changing the feature route.
- Container image built and started successfully, serving the same embedded assets as the source. PostgreSQL persists in the Compose named volume. Dogfooding state is created explicitly by scripts/dogfood.mjs.

Known MVP limits: one trusted workspace; full-state reads and serialized writes target small teams, no pagination/optimistic edit tokens yet. Evidence is recorded by humans/agents, not executed by this application. Artifact URLs are references. Git/Flux/Kubernetes are modelled behind interfaces with no live adapters yet. Release records are immutable plans, not deployment attestations. No hosted CI run, production deployment or Git remote push is claimed.

## Dynamic environment compositions

The next slice adds application mappings, arbitrary product-local environment targets, immutable integration revision capture and versioned composition plans. Race-enabled PostgreSQL/MCP tests cover separate products with two and four environments, two features in one repository composition, later source recapture without rewriting old snapshots, desired-only selection, target changes invalidating stale plans, context retrieval and restart persistence. Domain tests additionally reject invalid Git refs, incompatible ordered selections for applications sharing a repository, missing dependencies and cross-product references. Frontend checks exercise multi-application form serialization and exact snapshot rendering. Browser validation covers application/DEV/PROD creation, base-only composition planning and desired selection while runtime remains unknown.

## Intent-to-runtime architecture contracts

Architecture revision 2 preserves existing State, persisted kinds and commands. New target-only domain contracts cover scoped connections/topology, branch divergence, multi-sided conflicts, artifact provenance/rebuild replacement, retention representation, deployments and final release records. Pure tests reject diverged fast-forward, unobserved artifact presence, silent digest replacement, invalid conflict verification and incomplete/mismatched deployment evidence. These guards validate evidence structure; live application authorization, freshness and provider behavior are not implemented or claimed.

`TEST_DATABASE_URL=... make check` passed with real PostgreSQL, race tests, vet, formatting, frontend regression checks and binary build. No UI behavior changed in this revision. No provider adapter or external mutation was exercised.
