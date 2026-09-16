# Release Control Plane — MVP architecture

A standalone modular monolith. Go serves an embedded, dependency-free HTML/CSS/JavaScript UI, HTTP JSON and MCP Streamable HTTP. PostgreSQL is the sole persistence implementation. No infrastructure mutations or automated branch composition.

## Domain and relationships
Product owns Features, Applications, Environments, Repositories, Compositions and Releases. See [dynamic environment composition](ENVIRONMENT_COMPOSITION.md) for source revision and desired-target semantics. Feature owns an ordered plan of Integrations and Verification Gates. Integration is the independently implementable/releasable unit, with dependencies, acceptance criteria, ownership/work areas and repository/branch/commit references. Gates reference integrations; Checks belong to Gates; immutable CheckResults retain evidence and tested revision. Findings reference a CheckResult and can block integrations, gates and releases. Structured Decision, Discovery, Blocker, Progress and Handoff records retain actor, timestamp and optional revision/deployment/session. Resolution adds history. Events accompany all writes atomically.

## MVP and assumptions
Implement the complete create → work → verify/finding → resolve/rerun → complete → handoff/resume vertical slice. Completion means ready, not released. Release records explicitly select ready integrations and are immutable; planning does not deploy. Default lifecycle is planned, working, implemented, verifying, ready, released, isolated in domain policy. Environment desired/reconciled/runtime values are separate observations, initially manually recorded/unknown. Provider interfaces prepare Git, Flux and Kubernetes read adapters; live credentials and automated operations are deferred.

## Invariants
All references must exist and stay in the same product/feature as applicable. Dependencies cannot cycle. Completion requires dependencies ready/released, no open applicable blockers/findings, and latest passing results on all checks in applicable blocking gates (an empty gate does not pass). Resolving a finding never rewrites a failed result; a passing rerun is necessary. Checks/results require actor and provenance; results are append-only. Released integrations and releases cannot be silently rewritten. Every successful mutation and audit event commit together; failures leave no partial state. Concurrent writes serialize through a database lock in this single-workspace MVP, avoiding lost updates across processes.

## Persistence
Versioned embedded SQL migrations. Typed domain entities are stored individually as JSON documents in a relational records table, with kind/id and product/feature indexes, plus append-only events. This pragmatic first schema avoids a single mutable state blob and keeps evolving fields reversible; application validation owns cross-document reference integrity. Transactions lock a singleton revision row, read the current snapshot, apply one validated command, persist changed records and an audit event. This is a deliberately small-scale design; normalized query projections are a future migration, not a distributed event sourcing system.

## Shared application contract
GET /api/state returns the current typed State. GET /api/features/{id}/context returns deterministic resume context. POST /api/commands accepts {action, actor, ...command fields}. Transport adapters invoke the exact same application Execute/State/Resume functions. No transport-specific business policy.

Initial MCP surface: get_state, resume, execute. execute advertises a typed command union for coherent actions rather than dozens of tools; command schemas are also available to UI/API clients. Official Go MCP SDK handles protocol negotiation and streamable HTTP.

## UI information architecture
Product switcher and overview counts → feature list → feature workspace. Workspace shows intent, ordered integration plan, ownership, progress, decisions/discoveries/blockers, verification and evidence, findings, handoffs, Git/environment references and audit timeline. Named forms cover all vertical-slice actions, errors remain visible, and refresh reads canonical state. Responsive, keyboard-accessible, no fabricated health indicators.

## Operational boundary
Local trusted workspace by default: loopback bind, optional bearer token for API/MCP, same-origin browser mutation protection. Actor is attributed client identity, not authenticated RBAC. One process/container plus PostgreSQL. Artifact URLs are references, never fetched by the server. No cloud deployment, Git push or production modification in bootstrap.
