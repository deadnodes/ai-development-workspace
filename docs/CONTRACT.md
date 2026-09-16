# Shared application contract

`domain.Command` is `{action, actor, id?, product_id?, feature_id?, integration_id?, gate_id?, check_id?, result_id?, data: {...}}`. Actor is mandatory. IDs supplied for creates are optional; generated when absent. Execute returns created/updated entity. State uses arrays (always non-null): products, features, integrations, gates, checks, results, findings, memories, environments, repositories, releases, events. All entities have snake_case JSON keys. Entity IDs globally unique.

Actions and data:
- create_product: name, description
- create_feature: product_id top-level; title, problem, goal, requirements[], constraints[], context, repositories[], owner, status
- update_feature: feature_id; same fields, sparse update
- create_integration / update_integration: feature_id / integration_id; title, objective, rationale, dependencies[], acceptance_criteria[], repositories[], branches[] (objects repository_id,name), commits[] (objects repository_id,sha), owner, working_areas[], position, remaining[], status (create only defaults planned; update status use transition)
- start_integration / complete_integration / transition_integration: integration_id; transition data.status
- record_progress / record_decision / record_discovery / add_blocker / handoff: feature_id and optional integration_id; title, body, reason, completed[], current, remaining[], next[], warnings[], commit, deployment, session. Memory kind is progress/decision/discovery/blocker/handoff. progress updates integration completed/remaining. Handoff is historical structured memory. add_blocker defaults open.
- resolve_blocker: id (memory ID); body resolution
- create_gate: feature_id; title, reason, integration_ids[], position, blocking (default true), environment_id, commit, deployment
- add_check: gate_id; title, mechanism, instructions
- record_check_result: check_id; result (passed/failed/blocked/skipped), commit, environment_id, deployment, steps[], observations, logs, artifacts[] ({kind,url,label})
- record_finding: result_id (failed/blocked result); title, severity, body, integration_ids[], gate_ids[], blocks_release (default true)
- resolve_finding: id; body resolution, commit fix
- create_environment / update_environment: product_id / id; name, desired, reconciled, runtime (objects), composition (object)
- create_repository: product_id; name,url,provider
- plan_release: product_id; name,integration_ids[],excluded_integration_ids[],notes (immutable intent record; does not deploy or mark released)

State entity relationship fields product_id,feature_id,integration_id etc. Each entity has id, created_at, updated_at and actor. Events include id, action, actor, at, entity_id, feature_id, product_id, data (full command). Resume returns feature, integrations, progress {total,ready,released}, memories, gates, checks, results, findings, environments, repositories, other_active_work, next_actions, events. Integration carries completed[], remaining[], owner, working_areas[], branches[],commits[]. Gate result computed from latest results, not mutable tested flag. Default statuses planned/working/implemented/verifying/ready/released.

Go APIs: application.New(store) *Service; State(ctx) (domain.State,error); Execute(ctx,domain.Command) (any,error); Resume(ctx,featureID) (any,error). persistence.Open(ctx,url) (*Store,error); store.Close(). `domain.Actions` exported []string. `application.ErrNotFound`, `application.ErrValidation` support errors.Is. Domain commands use Data map[string]any.

Validation details: check results require at least one of commit/deployment. Gates must name one or more integration_ids; applicability is explicit. Decision requires reason. Integration edits conservatively return a ready integration to working. Unknown or non-lowercase data fields and metadata injection are rejected. Entity IDs are unique across all kinds, including append-only records. Release has `status: planned` and immutable `snapshots` of selected integrations; it is an intentional release plan, not evidence of deployment. Completion reevaluates latest applicable check evidence and requires linked commits to match check provenance when commits are provided.

Resume compactness: `events` contains only the latest 20 audit metadata summaries (id, action, actor, at, entity_id, product_id, feature_id), without duplicated command data; `events_total` and `events_truncated` indicate omitted history. Full audit remains in state. Integrations and gates sort by position then ID. `next_actions` combines the latest feature handoff and latest handoff for each integration in plan order, with active integration remaining-work fallback when no handoff exists; duplicate actions are removed. All structured memories and check results remain available in resume.

## Application and environment composition planning

Environments are arbitrary per-product records, not a fixed DEV/STAGE/PROD enumeration. `create_environment` / `update_environment` accept `cluster` and `namespace`; these are target metadata and never provision infrastructure. `desired_composition_id` is read-only except through `select_composition`. Legacy `composition` maps remain readable but typed compositions are authoritative for selected branch plans.

Additional commands:
- `create_application`: top-level product_id; data `{name, repository_id, path?}`. Path is optional clean repository-relative monorepo path.
- `record_integration_revision`: top-level integration_id; data `{repository_id, branch, base_commit, head_commit, commits: string[]}`. Immutable manually captured provenance. Full lowercase 40- or 64-hex hashes required; one hash format per revision, commits nonempty, unique and containing head_commit. Source branch and repository remain recorded after branch merges/deletion. These assertions are not Git-provider-verified.
- `plan_composition`: top-level product_id; data `{environment_id, name, components: [{application_id, base_ref, base_commit, target_branch, revision_ids: string[]}]}`. At least one component; empty revision_ids means base-only app. Target branch must use `generated/` and differ from base_ref. Base commit is a full hash. Same-repository applications must select identical base_ref/base_commit/target_branch and ordered revision_ids lists because one repository produces one composed branch. Only one selected revision per integration/repository. Dependencies must also be selected or already released. Cross-product/repository references rejected.
- `select_composition`: top-level id is composition ID; data `{}`. Sets only the target environment's desired_composition_id and audit metadata. Does not alter reconciled/runtime state or claim deployment, merge success or readiness.

State adds `applications`, `integration_revisions`, `compositions` arrays. Application fields: id/product_id/meta, name, repository_id, path. IntegrationRevision fields: id/product_id/feature_id/meta, integration_id, repository_id, branch, base_commit, head_commit, commits. Composition fields: id/product_id/meta, environment_id, name, status=`planned`, components, application_snapshots (Application[]), revision_snapshots (IntegrationRevision[]), environment_snapshot (Environment). Snapshots and revisions have no update action. Recording a newer integration revision never rewrites prior compositions.

Resume includes feature-linked integration_revisions, relevant applications, and compositions containing revision snapshots for that feature. Automatic branch construction, Git verification, Flux reconciliation and Kubernetes changes remain pending provider work.

Environment names are trimmed and unique case-insensitively within each product (the same name in different products is allowed). Changing cluster or namespace clears desired_composition_id; selecting an older plan whose captured cluster/namespace differs is rejected and requires replanning. Revision capture must use an integration's declared repository when its repositories list is nonempty; otherwise any repository in the product is permitted.
