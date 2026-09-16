# MVP validation

Validated locally on 2026-09-16.

- `TEST_DATABASE_URL=... make check`: gofmt check, Go vet, frontend syntax and renderer/serialization checks, all race-enabled Go tests, standalone binary build.
- Real PostgreSQL test isolation uses a fresh schema; it exercises the whole HTTP workflow, official MCP Go client initialization/tool listing/handoff/resume, two database connections with 12 concurrent writes, rejected-command rollback, and reopened storage equality.
- Domain tests exercise dependency cycles, scope isolation, evidence provenance, blockers, finding resolution plus required rerun, status/metadata bypasses, historical-ID collisions, immutable release-plan snapshots, release dependency inclusion, compact audit and multiple integration handoffs.
- Browser validation exercised product and feature creation, integration planning/start, gate/check creation, failed result, finding creation/resolution, passing rerun, completion, structured handoff, feature-level decision and resume context. Both executions remain visible and the resolved finding retains its history. Responsive wide/narrow layouts inspected; no horizontal DOM overflow observed. Browser console had no warnings/errors.
- Frontend regression checks exercise output escaping, evidence history, form availability, optional integration-ID serialization, and skip-link focus without changing the feature route.
- Container image built and started successfully, serving the same embedded assets as the source. PostgreSQL persists in the Compose named volume. Dogfooding state is created explicitly by scripts/dogfood.mjs.

Known MVP limits: one trusted workspace; full-state reads and serialized writes target small teams, no pagination/optimistic edit tokens yet. Evidence is recorded by humans/agents, not executed by this application. Artifact URLs are references. Flux/Kubernetes observation remains pending; the GitHub adapter milestone is described below. Release records are immutable plans, not deployment attestations. No hosted CI run, production deployment or Git remote push is claimed.

## Dynamic environment compositions

The next slice adds application mappings, arbitrary product-local environment targets, immutable integration revision capture and versioned composition plans. Race-enabled PostgreSQL/MCP tests cover separate products with two and four environments, two features in one repository composition, later source recapture without rewriting old snapshots, desired-only selection, target changes invalidating stale plans, context retrieval and restart persistence. Domain tests additionally reject invalid Git refs, incompatible ordered selections for applications sharing a repository, missing dependencies and cross-product references. Frontend checks exercise multi-application form serialization and exact snapshot rendering. Browser validation covers application/DEV/PROD creation, base-only composition planning and desired selection while runtime remains unknown.

## Intent-to-runtime architecture contracts

Architecture revision 2 preserves existing State, persisted kinds and commands. New target-only domain contracts cover scoped connections/topology, branch divergence, multi-sided conflicts, artifact provenance/rebuild replacement, retention representation, deployments and final release records. Pure tests reject diverged fast-forward, unobserved artifact presence, silent digest replacement, invalid conflict verification and incomplete/mismatched deployment evidence. These guards validate evidence structure; live application authorization, freshness and provider behavior are not implemented or claimed.

`TEST_DATABASE_URL=... make check` passed with real PostgreSQL, race tests, vet, formatting, frontend regression checks and binary build. No UI behavior changed in this revision. No provider adapter or external mutation was exercised.

## GitHub DEV execution and repository/external-system correction

`TEST_DATABASE_URL=... make check` passed on 2026-09-16: formatting, vet, JavaScript checks, four workflow-contract Python tests, race-enabled Go tests with real PostgreSQL, and binary build.

Deterministic provider tests cover App JWT/token handling, numeric repository identity, pinned Git observation, workflow correlation/report validation, GHCR availability and digest verification, and constrained YAML mutation. Orchestration tests cover explicit build/deployment policy, source provenance, missing/rebuilt artifacts, branch policy, asynchronous scheduling and failure history. PostgreSQL HTTP/MCP tests reopen storage during dispatch and GitOps recovery and verify persistent operations, evidence and no duplicate external mutation. Live App tests are explicitly opt-in and were not run.

Registry tests cover shared repository identity across installations/products and narrowed Feature/Integration scopes. External-system tests cover affected relationships, context packages and fresh verification after changed external obligations. Browser dogfooding created an unmanaged dependency, linked a component and affected Feature, inspected Agent context, and observed a persistent failed Git refresh for an unconfigured Integration. That failure is configuration evidence, not a successful GitHub integration.

Live acceptance remains incomplete: a configured GitHub App, workflow and target environment are required for live verification. Instance-specific setup and blockers belong in the application, not this repository. No real workflow was dispatched, image built, or GitOps commit written. The supported DEV endpoint stops at confirmed GitOps application / pending Flux reconciliation; it never infers runtime deployment. See GITHUB_DEV.md for the build contract and remaining setup.

## Dedicated Kubernetes installation manifests

The generic Kustomize base and two independent product overlays render successfully with `kubectl kustomize`. Checked distinct namespaces, immutable image substitutions, generated ConfigMap references, external Secret references and probe Host header. The normal PostgreSQL-backed `make check` passed. No target Kubernetes cluster, published image or deployment database was configured for a live rollout; these checks do not claim Kubernetes deployment success. See KUBERNETES.md for the required inputs and agent commands.

## Authoritative configuration mirror

Configuration export tests cover Product isolation, effective configuration selection, deterministic revisions, observation exclusion and preservation of historical records. PostgreSQL reopen tests preserve the generated snapshot; HTTP and MCP delegation tests cover the export query. Full `make check` passed. The updated local container successfully exported existing Product configuration twice with byte-identical results. Export is one-way; automatic Git push, restore/import and full-history backup are not claimed.
