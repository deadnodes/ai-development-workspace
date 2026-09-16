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
