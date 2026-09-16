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

## Full compressed backup and migration

`TEST_DATABASE_URL=... make check` passed with gzip/checksum/version/duplicate-ID/expanded-limit rejection, atomic nonempty-target rejection, active-operation cancellation with original audit evidence, and concurrent restores (exactly one succeeds). A two-schema PostgreSQL HTTP-export/MCP-restore test verifies all persisted state and append ordering after reopening the destination. Migration 002 adds explicit record positions so a bulk restore does not reorder effective configuration.

The local running instance was exported through `scripts/backup.mjs` (31,503 compressed bytes) and restored through binary HTTP into a separate temporary process/database schema. Configuration, memory and original audit matched; the target had the additional restore event. The temporary target was stopped and its test schema removed; source data was retained. The updated Compose application is running with the migration. No external Git/CI/deployment actions were triggered by the restore.

## Codex PR review findings

The implementation reads GitHub inline, conversation and review comments using App authentication and imports recognized Codex P1/P2 evidence without fabricated test failures. Tests cover reviewer identity, shared-PR preview/selection, deduplication, unchanged resolved comments, edited-comment reopening, context/attention and PostgreSQL reopen followed by MCP resync. Full make check and additional race-enabled application/transport checks passed. Renderer checks verify review source/commit display and escaped comment text.

A real Codex P2 inline-comment format was inspected through authorized read-only gh access. Control Plane live App review sync remains unverified until App credentials and review permissions are configured. The updated local container was built and applied. Native browser verification could not run because browser connection timed out twice; renderer checks are not presented as browser validation.

## Executable compositions, selected main releases and hotfixes

2026-09-16: PostgreSQL-backed `make check` passes, including formatting/vet, frontend render/serialization checks, workflow contract tests, all Go race tests and binary build. Deterministic Git/CI/GitOps tests execute independent A+B compositions, removal without changing another environment, conflicts, concurrent selection changes, worker restart, selected main candidates, exact-candidate scenario verification, promotion and subsequent hotfix releases within the original Feature. A newly rebuilt digest cannot replace the approved digest during promotion.

Real PostgreSQL reopen and gzip transfer tests retain parent/child operations, frozen source/provider snapshots, ordered scenario versions/results, runtime evidence and immutable releases. Restoring active operations cancels them and preserves the interruption audit; the destination does not execute them.

Native Chrome validation inspected the candidate approval dialog, scenario target/evidence form and saved an advisory scenario with actor/history. No deployment was submitted from the browser. HTTP/MCP parity tests cover all eight new commands and reject contradictory references.

These are implementation checks, not live GitHub acceptance. Live App → Actions → GHCR → GitOps execution remains unverified until the installation is configured. Runtime confirmation currently records attributed human/agent evidence; an automatic Flux/Kubernetes observer is not implemented. Main advancement is non-force fast-forward and can be refused by protected branches; it is not an atomic transaction across repositories.
