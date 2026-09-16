# MVP validation

Validated locally on 2026-09-16.

- `TEST_DATABASE_URL=... make check`: gofmt check, Go vet, frontend syntax and renderer/serialization checks, all race-enabled Go tests, standalone binary build.
- Real PostgreSQL test isolation uses a fresh schema; it exercises the whole HTTP workflow, official MCP Go client initialization/tool listing/handoff/resume, two database connections with 12 concurrent writes, rejected-command rollback, and reopened storage equality.
- Domain tests exercise dependency cycles, scope isolation, evidence provenance, blockers, finding resolution plus required rerun, status/metadata bypasses, historical-ID collisions, immutable release-plan snapshots, release dependency inclusion, compact audit and multiple integration handoffs.
- Browser validation exercised product and feature creation, integration planning/start, gate/check creation, failed result, finding creation/resolution, passing rerun, completion, structured handoff, feature-level decision and resume context. Both executions remain visible and the resolved finding retains its history. Responsive wide/narrow layouts inspected; no horizontal DOM overflow observed. Browser console had no warnings/errors.
- Frontend regression checks exercise output escaping, evidence history, form availability, optional integration-ID serialization, and skip-link focus without changing the feature route.
- Container image built and started successfully, serving the same embedded assets as the source. PostgreSQL persists in the Compose named volume. Dogfooding state is created explicitly by scripts/dogfood.mjs.

Known MVP limits: one trusted workspace; full-state reads and serialized writes target small teams, no pagination/optimistic edit tokens yet. Evidence is recorded by humans/agents, not executed by this application. Artifact URLs are references. Git/Flux/Kubernetes are modelled behind interfaces with no live adapters yet. Release records are immutable plans, not deployment attestations. No hosted CI run, production deployment or Git remote push is claimed.
