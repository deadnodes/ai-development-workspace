# Release Control Plane — evolving architecture

Status: evolving architecture, 2026-09-16. Revision 3 corrects repository ownership and adds unmanaged dependency context. This evolves the existing Go modular monolith; it does not replace the implemented MVP. Discover authoritative engineering context in the configured instance. See [implementation boundaries and rollout](ROADMAP.md), [command contract](CONTRACT.md), and [composition semantics](ENVIRONMENT_COMPOSITION.md).

## Responsibility

The service is a development metastore and control plane, not an application source editor or business-data processor. See [responsibility boundary](RESPONSIBILITY_BOUNDARY.md) for the agent/executor split and existing GitOps write boundary.

The Control Plane owns semantic development state, desired source/environment compositions, orchestration plans, policy decisions, provenance and audit evidence. It coordinates external Git, CI, registries and GitOps systems. It is not a Git server, container registry, CI runner or Kubernetes operator.

| Stage | Control Plane owns | External system owns |
| --- | --- | --- |
| Feature / Integration | Intent, plan, ownership, decisions, verification, handoff | Source editing by developers/agents |
| Git state | Managed branch intent, pinned inputs, comparisons, conflict context | Git objects, repositories, branches, pull requests |
| Build / Artifact | Workflow request, build provenance, digest identity and availability history | CI execution/logs and registry image bytes |
| Environment composition | Explicit selected component/source/artifact versions | Generated branches and built outputs |
| GitOps / Flux | Mapping, planned diff, approved change and reconciliation observations | GitOps repository and Flux controllers |
| Kubernetes | Deployment evidence, drift and actionable failures | Workload reconciliation and runtime |

Transport remains embedded Web UI, HTTP and MCP in one binary. All use the same application commands/queries and domain policy. No provider adapter may independently decide to merge, rebase, rebuild, deploy or delete.

## Instance and Product

An Instance is one installation and database. It can manage one or many unrelated Products. Product is a first-class **logical isolation boundary**, not a synonym for an installation or a global namespace. Repositories have instance-level identities in a provider-discovered Repository Registry. Products explicitly attach selected registered repositories, and own their components, environments, features, integrations, compositions, conflicts, artifacts, builds, deployments and releases. A repository may be attached to multiple Products without duplicating its provider identity. Reference validation checks product ownership before using a provider.

A Product defines its own topology and defaults. It can use any environment names and counts; there is no hardcoded DEV/STAGE/PROD chain. Environment purpose (`integration`, `test`, `staging`, `production`, `custom`) is explicit configuration, not inferred from its name. Cluster/namespace, Git providers, registries, CI workflows and GitOps mappings are per-product choices. Independent products can share physical infrastructure only through explicit configuration; their feature selection is never implicitly mixed. For example, Product A can define `dev` and `prod`, while Product B independently defines `dev`, `test`, `stage` and `prod`. Each environment selects its own component versions and integration revisions; adding an environment never requires a new global lifecycle enum.

ProviderConnections belong to the instance and describe adapter kind, endpoint, external scope/capabilities and **secret references only**. Explicit product grants and product bindings authorize use of a connection; sharing a connection does not grant visibility across products. A dedicated installation simply has one Product. The current trusted-local deployment exposes all products via `/api/state` and MCP `get_state` under one optional shared token: current logical reference isolation is **not** per-user RBAC. Authentication/authorization for a shared installation must filter reads as well as writes before claiming access isolation.

## Domain model and compatibility

| Entity | Meaning and relationships | Availability |
| --- | --- | --- |
| Product | Independent topology and ownership boundary | Persisted |
| Component | Deployable application, repository and optional monorepo path | Persisted as `Application`; compatible alias |
| Repository Registry / attachment | Instance provider identity and explicit Product selection; Feature/Integration use narrower subsets | Additive registry; legacy Repository IDs retained as Product attachments |
| Environment | Named product-local target, desired/reconciled/runtime separation | Persisted; target purpose/bindings target |
| EnvironmentComposition | Immutable component selections, pinned source revisions, target snapshot | Persisted as `Composition`; artifacts/execution target |
| Feature / Integration | Intent and independently implementable/releasable work | Persisted |
| WorkClaim | Actor, integration, repositories, branches, file areas, claim/release times | Current owner/areas persisted; separate claim target |
| BranchState | Managed/source ref, pinned base/head/main, known divergence and observation time | Typed target contract |
| IntegrationRevision | Immutable per-integration repository source capture | Persisted, currently caller-reported |
| IntegrationConflict | Pinned sides/base/files/hunks, semantic snapshots, claimant, resolution and checks | Typed target contract |
| Verification Gate / Check / Result / Finding | Required evidence and failure history | Persisted |
| Decision / Discovery / Blocker / Progress / Handoff | Structured provenance and continuity | Persisted |
| ProviderConnection / ProductTopology / Policy | Explicit scoped integration/configuration contracts | Typed target contracts |
| ArtifactRecord / BuildRun | Durable artifact provenance and external build history | Typed target contracts |
| DeploymentRun | Policy-bound orchestration and per-component evidence | Typed target contract |
| ReleaseRecord | Immutable attestation of intentionally deployed selected integrations/artifacts | Typed target contract |
| Release plan | Immutable selection of ready integrations | Currently `Release` with `status: planned` |
| AttentionItem | Scoped actionable state with evidence and next actions | Typed target contract |

`internal/domain/lifecycle.go` and provider interfaces are target contracts, not a claim of persistence, polling or available MCP tools. Existing `State`, commands and stored records remain compatible. Do not rename `applications`, `application_id`, `application_snapshots` or the existing command in place. Do not reinterpret a planned Release as a successful deployment. [ROADMAP.md](ROADMAP.md) specifies additive migrations.

## Source-state principle and branch management

Desired composition exists independently of generated branches. Each repository input pins a base branch **and commit**, ordered integration revision IDs and immutable source snapshots. Applications sharing a repository share the same ordered repository composition. Reconstructing a disappeared generated branch uses these inputs, never inference from its current merge history.

Starting an integration remains a semantic action today. Once configured, its future execution plan can additionally request a managed feature branch from the product's pinned base. Record the intended branch before the external operation, then observe the created branch and SHA. Naming is product-configured and collision-checked; retries must discover the same operation rather than create another branch. Keep the repository, integration, original base, current head, upstream head and observation freshness explicit. Retain provider-side protected refs for source captures; database SHA records alone do not retain Git objects.

Ahead/behind is measured against a specific observed upstream SHA. Unknown is not zero, and stale observations cannot authorize a write. **Ahead > 0 and behind > 0 means divergence; it cannot be repaired by fast-forwarding the feature to main without losing its commits.** Operations are distinct: fast-forward, merge, rebase and force update. Fast-forward requires verified ancestry and an unchanged expected head; merge/rebase need separately approved policies and invalidate earlier verification as appropriate. Rebase/force must never silently rewrite meaningful history. Disposable generated branches have a separate policy from developer-owned branches.

Each planned write includes expected refs, an operation/idempotency ID, actor and the policy/configuration revision. A changed external head makes the plan stale: reobserve and replan rather than forcing it through. The orchestrator does not execute arbitrary shell commands supplied by MCP.

## Conflicts and semantic resolution

A conflict binds a Product, repository, exact base, composition attempt, conflicting revision snapshots, files and optional hunks. More than two integrations may participate. Preserve original evidence even if an external branch later moves.

Conflict context includes each side's Feature intent, Integration objective/rationale/acceptance criteria, relevant decisions/discoveries/blockers, source branch and captured commits. Include base identity, conflict evidence, required verification checks and the instruction to preserve every participating integration's semantics. A context query is read-only; a claim or resolution is a separate audited command.

Target lifecycle: detected → requires_resolution → claimed → verification_required → resolved. Claiming records the actor and lease/release metadata. Recording a resolution commit does not resolve the conflict: verify that it derives from the exact inputs, run required checks on that candidate, then rebuild the composition. Deployment is a separate downstream run; resolving a conflict does not mean DEV is deployed. Failed verification returns actionable state and retains prior attempts. Input changes supersede the candidate and require fresh analysis. Never guess semantic resolutions automatically.

## Builds, artifact identity and reconstruction

Artifact identity is an immutable registry/repository/digest, preferably `image@sha256:…`. Tags are observations/aliases. Artifact metadata remains in the Control Plane if registry bytes disappear. Availability is a timestamped observation (`unknown`, `present`, `missing`, `deleted`, or inaccessible/error as evidence), never inferred from having a digest. Provider failure/permission denial must not be treated as confirmed deletion.

Every ArtifactRecord references Product, Component, Repository, exact source commit and composition, build definition/version, CI connection/provider/workflow, build parameters, creation time and original digest. Capture base image digests, dependency lock identities, toolchain/build context and build arguments where relevant. Secret inputs use references; normal domain tables, MCP context and audit must not contain token values or secret build arguments.

BuildRun represents an external CI run, not a local build engine. Record request/configuration snapshots, trigger idempotency key, provider run ID, source, status, log/artifact references and failure evidence. Rebuildability requires **current evidence** that source, build definition/workflow, dependencies and required credentials are available. Provenance presence alone does not prove rebuildability. Bit-for-bit reproducibility is a separate, explicitly evidenced property; default unknown/unproven.

When an expected image is unavailable, evaluate provenance, current source/workflow availability and product rebuild policy. If sufficient, create a rebuild request and place the deployment in waiting/building; otherwise expose the exact blocker. A successful CI run is insufficient until the registry artifact is observed and its provenance is checked.

A rebuilt image is a **new ArtifactRecord** linked to the original. Retain original requested digest and rebuilt digest. If they differ, do not rewrite the original artifact, deployment plan or approval. Create an explicit replacement plan revision, rerun applicable verification and obtain the required policy/human approval. Report “reconstructed from the recorded source; original digest unavailable,” not “identical image.” Rebuilding is not proof of bit-for-bit identity.

## Retention policy

Retention scope can be Product, Component, Environment or registry connection. Structured fields cover development/released keep counts, current production pinning, previous successful production versions and failed-build TTL. Explicit manual pins, active deployments and rollback requirements protect artifacts. Policy layering/precedence must be deterministic and revisioned; overlapping protection wins over eviction eligibility.

The first artifact slice evaluates policy without deleting: expected retained artifacts, eviction candidates, missing protected artifacts and rebuild-required warnings. Rank only comparable artifacts in the same scope and use recorded successful deployment order for previous-production retention. Unknown availability, absent policy or incomplete history must not produce a destructive recommendation. Actual registry garbage collection is a later capability, excluded from current provider write ports and MVP execution. Current work introduces representation; evaluator persistence/UI/MCP follows the artifact slice.

## GitOps and deployment

GitProvider manipulates generic Git; GitOpsProvider owns deployment file semantics. Product mappings identify Component + Environment, provider connection/repository, path, target kind (such as Flux HelmRelease) and configurable image repository/tag/digest field paths. PlanChange returns a concrete diff plus expected repository head. ApplyChange publishes the approved commit or PR through Git, and PR creation/approval is not considered an applied deployment until the merged desired revision is observed. Never hardcode a single repository layout.

Normal releases change GitOps desired state, not Kubernetes objects directly. Flux observes the committed configuration; Kubernetes confirms actual rollout. Both observations must correlate to the requested GitOps revision and component artifacts, not merely an unrelated healthy controller or existing pod.

Release planning resolves pinned source and revisions, artifact availability/rebuild actions, current source divergence, applicable gates/findings, policy requirements and the concrete GitOps diff. Show the full plan before policy-controlled execution. Approval binds exact input/artifact digests, configuration/policy revision and diff hash; any changed approved input invalidates it. Production classification is explicit environment policy, not a name comparison.

Target deployment states: `planned`, `waiting_for_artifact`, `building`, `artifact_ready`, `gitops_pending`, `reconciling`, `verifying`, `deployed`; failures use `failed`, `blocked`, `cancelled` with reason/evidence. An already available artifact can skip waiting/building. A GitOps PR can remain pending until merge. Retries are new attempts linked to the same immutable plan, not erasures of previous failures. Reconstructed digests require a new plan revision as above.

A DeploymentRun has per-component progress and evidence: selected source; requested/resolved artifact and registry observation; intended/applied GitOps revision; Flux readiness/observed revision; actual runtime image and health; required checks. One component succeeding does not make the whole run deployed. Unknown, stale, mismatched or partial observations prevent success. Final ReleaseRecord freezes selected Integration IDs, exact source/composition/artifacts, deployment evidence, actor, approval and timestamps after all required outcomes are verified. A rollback is a new desired/execution run referencing an earlier composition, subject to current artifact/gate/policy checks.

## Policies and operation execution

Policies are simple versioned structs/configuration, not a generic language. Keep explicit controls for automatic fast-forward, merge, rebase, force, missing-artifact rebuild, required verification gates, production human approval and composition rebuild on base changes. Defaults are conservative. Approval grants a specific operation, not unrestricted provider access. Missing configuration, ambiguous source, unknown observation or unsupported provider capability fails closed.

The future application executor persists a planned operation/outbox entry and audit before contacting a provider. It performs network I/O outside the database transaction/lock, then persists provider IDs, observations and outcomes atomically. Adapters normalize provider errors and capabilities; the application owns decisions, retry policy, backoff and compensation. After a crash or ambiguous timeout, reconcile by operation ID/provider run or external refs before retrying. Do not hold the current global write lock across Git/CI/registry calls. No distributed event sourcing or general workflow engine is required: a durable bounded worker inside the monolith is sufficient.

## MCP and human information architecture

MCP retains `get_state`, `resume`, `execute` and adds the concrete GitHub slice queries/actions documented in [GITHUB_DEV.md](GITHUB_DEV.md). Do not advertise target-only actions as executable.

Incremental semantic queries: product state, feature/integration resume, environment composition, branch state, conflicts, conflict context, release readiness and actionable attention. Target actions include branch creation/update, integration requests, conflict claim/resolution, build/dev-deployment requests, verification and release planning. All actions use the same application services/policy and scoped connections as the UI; MCP supplies structured requests, never arbitrary commands.

Attention entries include `BRANCH_OUTDATED`, `MERGE_CONFLICT`, `ARTIFACT_MISSING`, `VERIFICATION_REQUIRED`, evidence freshness, owner and next action. Derive/deduplicate them from authoritative state and clear them only after a resolving observation/event. Start with pull queries; subscriptions are later. Conflict context must contain semantic snapshots from all sides, not just markers.

UI evolution: product topology/connections → feature workspace and branch divergence → conflict resolution workspace → component artifact/build history → environment source/artifact/GitOps/Flux/runtime columns → release plan/approval/execution timeline. Every view distinguishes desired, observed and unknown. Drift compares pinned desired digests/revisions against correlated observations; it is not inferred from tag strings alone.

## Persistence and invariant summary

Keep PostgreSQL and the current versioned migrations, individual typed JSON documents in `records`, and append-only `events`. Current commands serialize mutations through a revision row and append audit atomically; reads use a consistent snapshot. This deliberately small-team implementation can gain indexed projections as needed. Add concrete persisted kinds only with their first working vertical slice, not merely because target Go structs exist.

Core invariants remain: same-product references, acyclic dependencies, independently releasable integrations, blocking gates with latest applicable evidence, immutable failed results and source/composition snapshots, attribution on all actions and no audit for rejected writes. New invariants: explicit connection grants; no secret values in records; expected-head checks for mutations; no fast-forward of diverged history; no inferred registry/runtime success; no silent digest substitution; no release attestation before verified deployment.

The initial pure guards check supplied evidence shape, pinned digest/revision agreement and complete component coverage. They do not authenticate evidence, resolve product ownership, enforce freshness windows or authorize provider calls. Future application services must load trusted persisted observations and enforce those checks; a caller-supplied `ready` flag is never sufficient authority.

The concrete GitHub slice now implements `internal/domain/external.go`, `internal/delivery`, `internal/providers/github` and the application worker. Its connection registry, grants, repository attachments, Git observations, operations/steps, build and artifact observations are persisted through the existing JSON record store. No SQL schema migration is required for these additive document kinds. The broader lifecycle.go types in the table remain target contracts; concrete DEV operations stop at GitOps applied pending reconciliation. Track live acceptance in the configured instance; implemented adapter code and deterministic tests do not constitute a real deployment. Production execution is excluded.


## Repository registry and unmanaged dependencies

Provider discovery populates one installation-wide Repository Registry keyed by provider identity and numeric external repository ID. Product repository records are attachments to registry entries, not exclusive ownership of source repositories. Existing IDs remain valid attachment IDs for compatibility. Features select the attached repositories relevant to their intent; Integrations select only the repositories they modify within that Feature scope. Reusing a registered repository across Products never implicitly shares their development memory or deployment policy.

ExternalSystem is an instance-level unmanaged dependency with name, description, owning team/contact, interfaces/contracts and notes. Relationships link a managed Product/Component to it using DEPENDS_ON, CONSUMES, PROVIDES_TO or SHARES_DATA_WITH. A Feature can mark external systems and relationships as affected; an Integration can narrow that scope. Verification Gates can reference external compatibility obligations; existing blocking checks/results provide release readiness enforcement and audit. The context projection includes the relevant unmanaged systems, relationships and gate obligations so an agent sees affected contracts/teams.

An ExternalSystem ID is never accepted as a repository, managed Component or deployment target. It has no managed branches, artifacts, environments or releases. This is development/release context, not project synchronization or a generic service catalog.

## Dedicated product deployments

A supported deployment topology is one instance per Product: its own Kubernetes namespace, Deployment/Service, API token, provider secrets and PostgreSQL database/user. All product environments share that instance. Instance scope remains the boundary for repository registry and provider connections; no cross-instance synchronization is implied. Multiple Products in one instance remain supported. See [Kubernetes deployment](KUBERNETES.md).

## Authoritative project configuration

PostgreSQL is authoritative for project configuration and development history. Git mirrors are generated projections with no implicit reverse import. Source Git and executable GitOps retain their separate responsibilities. See [authority and current export contract](CONFIGURATION_AUTHORITY.md).

## Testing versus production source

Test environments host selected integration compositions. Production release candidates are built from main after explicitly selected work is integrated there; shared DEV images/branches are not promoted wholesale. Exact composition-bound test evidence is distinct from main-candidate verification. See [test/release flow and scenario model](TEST_AND_RELEASE_FLOW.md).

## Local workspace mode

The same domain/application/UI/MCP run against an embedded bbolt file by default or PostgreSQL when DATABASE_URL is configured. Both implement the same transactional Store interface; transport and business rules do not branch by backend. Local storage is single-process and bounded; the shared deployment uses PostgreSQL.

An operator-configured workspace root enables read-only Git/AGENTS discovery. The scanner never clones or executes repository commands. Local checkout observations are distinct from portable repository URLs. Curated ProductKnowledge contains nested product areas, repository scope, interface relationships and nonsecret parameters independently of Feature history. AGENTS documents remain attributed source context, never implicit execution authorization.

Portable workspace configuration contains current setup and documentation, excludes history and deployment authorizations, and imports atomically as a new Product. Full gzip backup remains the whole-instance history transfer. No new compatibility versions or alternate domain implementation were introduced. See LOCAL_WORKSPACE.md.
