package agentguide

import (
	"encoding/json"
	"fmt"
	"strings"

	"releasecontrol/internal/domain"
)

// ToolEntry is the canonical, generated agent reference record. Names are the
// exact MCP tool, execute action, or Go 1.22 ServeMux pattern.
type ToolEntry struct {
	Name      string      `json:"name"`
	Kind      string      `json:"kind"`
	Area      string      `json:"area"`
	Purpose   string      `json:"purpose"`
	When      string      `json:"when_to_use"`
	Inputs    []Field     `json:"inputs"`
	Output    string      `json:"output"`
	Errors    []ErrorInfo `json:"errors"`
	Example   string      `json:"example"`
	Related   []string    `json:"related"`
	Authority string      `json:"authority"`
}
type Field struct {
	Name     string `json:"field"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Meaning  string `json:"meaning"`
}
type ErrorInfo struct {
	Code    string `json:"code"`
	Meaning string `json:"meaning"`
}

// These are the areas completed in Stage 5A. Fields mirror the oneOf command
// branches in transport.CommandSchema; shared envelope fields are included.
var documented = []ToolEntry{
	{Name: "search_context", Kind: "mcp_tool", Area: "context_read", Purpose: "Search indexed authored Feature, Integration, Memory, and Repository text within a Product.", When: "Use to find relevant recorded text before following graph links or reading an excerpt; do not use it as release acceptance evidence.", Inputs: []Field{{"product_id", "string", true, "Product scope"}, {"feature_id", "string", false, "Optional Feature scope"}, {"query", "string", true, "Search text, 1..256 UTF-8 characters"}, {"limit", "integer", false, "1..20, default 10"}}, Output: "Bounded matching source cards with snippets, recorded status, scores, truncation, has_more, and guidance.", Errors: []ErrorInfo{{"VALIDATION", "Messages include 'exact product_id required', 'query must contain 1..256 UTF-8 characters', 'invalid feature_id', 'search limit must be 1..20'; correct inputs."}, {"FORBIDDEN", "Product read is not permitted; use an authorized scope."}, {"NOT_FOUND", "Refresh Product/Feature context and verify IDs."}}, Example: `{"product_id":"PRODUCT","query":"retry policy","limit":5}`, Related: []string{"graph_neighbors", "get_context_excerpt", "get_project_context"}, Authority: "Read only. Matching text is historical evidence and grants no source-edit, shell, deployment, or release authority."},
	{Name: "get_context_excerpt", Kind: "mcp_tool", Area: "context_read", Purpose: "Read a bounded Unicode-character window from one curated authored context source.", When: "Use after search_context or graph_neighbors identifies an exact source ID; paginate with next_offset.", Inputs: []Field{{"product_id", "string", true, "Product scope"}, {"id", "string", true, "Exact source ID"}, {"offset", "integer", false, "Unicode character offset, 0..10000000, default 0"}, {"max_chars", "integer", false, "Window size 1..8000, default 2000"}}, Output: "Source card plus text window, offset, and optional next_offset.", Errors: []ErrorInfo{{"VALIDATION", "Messages include 'exact product_id required', 'exact source id required', and 'excerpt offset must be 0..10000000 and max_chars 1..8000'; correct inputs."}, {"NOT_FOUND", "Source is unavailable; search again or verify Product and ID."}, {"FORBIDDEN", "Use an authorized Product scope."}, {"VALIDATION", "'context result exceeds byte budget; use a smaller limit or text window'; reduce max_chars."}}, Example: `{"product_id":"PRODUCT","id":"SOURCE","max_chars":500}`, Related: []string{"search_context", "graph_neighbors"}, Authority: "Read only; curated context is not raw provider data, audit history, credentials, or authorization."},
	{Name: "graph_neighbors", Kind: "mcp_tool", Area: "context_read", Purpose: "Traverse stored Product, Feature, Integration, Memory, and Repository relationships.", When: "Use to explore typed recorded links around a source; then read relevant sources with get_context_excerpt.", Inputs: []Field{{"product_id", "string", true, "Product scope"}, {"id", "string", true, "Exact seed source ID"}, {"depth", "integer", false, "1..2, default 1"}, {"limit", "integer", false, "1..30 nodes/edges, default 15"}}, Output: "Seed card, bounded related nodes and typed edges, truncation flag, and guidance.", Errors: []ErrorInfo{{"VALIDATION", "Messages include 'exact product_id required', 'exact source id required', 'graph depth must be 1..2 and limit 1..30'; correct inputs."}, {"NOT_FOUND", "Seed source is unavailable; search again."}, {"FORBIDDEN", "Use an authorized Product scope."}, {"VALIDATION", "'context result exceeds byte budget; use a smaller limit or text window'; lower limit."}}, Example: `{"product_id":"PRODUCT","id":"FEATURE","depth":1}`, Related: []string{"search_context", "get_context_excerpt", "get_integration_context"}, Authority: "Read only. Edges are stored links, not inferred claims or permission to change linked records."},
	{Name: "get_integration_context", Kind: "mcp_tool", Area: "context_read", Purpose: "Read implementation context, provider observations, and deployment operations for one Integration.", When: "Use when resuming implementation or checking the Integration's recorded delivery context.", Inputs: []Field{{"integration_id", "string", true, "Exact Integration ID"}}, Output: "Application query projection of Integration context and related observations/operations.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify Integration ID and parent scope."}, {"VALIDATION", "Correct malformed or out-of-scope identifier; refresh context."}, {"FORBIDDEN", "Use an authorized Integration scope."}}, Example: `{"integration_id":"INTEGRATION"}`, Related: []string{"resume", "get_git_state", "get_operation"}, Authority: "Read only; recorded context does not establish current readiness or authorize source changes, deployment, or release."},
	{Name: "get_project_context", Kind: "mcp_tool", Area: "context_read", Purpose: "Read bounded Product overview, installation parameters, repository/component/environment cards, and knowledge-node metadata.", When: "Use as a Product entry point; search text or graph links for specific authored facts.", Inputs: []Field{{"product_id", "string", true, "Exact Product ID"}}, Output: "Project summary capped at 32 KiB; full AGENTS/source documents are omitted.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify Product ID."}, {"VALIDATION", "'bounded project summary reader required'; operator must update service implementation."}, {"FORBIDDEN", "Use authorized Product scope."}}, Example: `{"product_id":"PRODUCT"}`, Related: []string{"get_state", "search_context", "graph_neighbors"}, Authority: "Read only; project guidance documents do not grant execution or release authority."},
	{Name: "get_state", Kind: "mcp_tool", Area: "context_read", Purpose: "Navigate compact bounded Product, Feature, or Integration cards.", When: "Use initially to discover IDs, then use scoped context/query tools for details.", Inputs: []Field{{"product_id", "string", false, "List Features under Product; max 128 chars"}, {"feature_id", "string", false, "List Integrations under Feature; max 128 chars; must match product_id if both given"}, {"limit", "integer", false, "1..100, default 25"}, {"cursor", "string", false, "Opaque cursor from same scope, 1..2048 chars"}}, Output: "Compact cards with pagination; projection <=32 KiB and complete MCP result <=128 KiB.", Errors: []ErrorInfo{{"VALIDATION", "'Invalid get_state arguments: use exact product_id/feature_id, limit 1..100 and a cursor from the same scope'; pass exact allowed keys and values."}, {"VALIDATION", "'get_state requires a bounded metadata reader; update the service implementation'; operator action required."}, {"VALIDATION", "'get_state metadata read failed; ask the owning operator to inspect the store'; report unavailable metadata reader/store."}, {"VALIDATION", "Projection/result exceeded navigation/wire budget; narrow scope or limit."}}, Example: `{"product_id":"PRODUCT","limit":25}`, Related: []string{"get_project_context", "resume", "get_integration_context"}, Authority: "Read only navigation. Status is not readiness or deployment proof; cursor is not authorization."},
	{Name: "get_feature_graph", Kind: "mcp_tool", Area: "context_read", Purpose: "Read a Feature delivery graph of repository-scoped PRs, source revisions, compositions, artifacts, and deployment observations.", When: "Use to inspect recorded delivery relationships and separate historical reports from observed runtime.", Inputs: []Field{{"feature_id", "string", true, "Exact Feature ID"}}, Output: "Feature delivery graph projection.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify Feature ID and refresh scoped context."}, {"VALIDATION", "Correct identifier or scope."}, {"FORBIDDEN", "Use authorized Feature scope."}}, Example: `{"feature_id":"FEATURE"}`, Related: []string{"get_integration_context", "get_git_state", "get_operation"}, Authority: "Read only; missing runtime is unknown and graph history does not authorize merge, deploy, or release."},
	{Name: "get_git_state", Kind: "mcp_tool", Area: "git", Purpose: "Read recorded provider observations for an Integration's branch, pull request, and checks.", When: "Use to inspect last observed Git state; request refresh_integration_git when a new observation is needed.", Inputs: []Field{{"integration_id", "string", true, "Exact Integration ID"}}, Output: "Stored Git observation; unknown means no synchronized observation, not synchronized state.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify Integration ID and refresh scoped context."}, {"VALIDATION", "Correct malformed identifier or scope."}, {"FORBIDDEN", "Use an authorized Integration scope."}}, Example: `{"integration_id":"INTEGRATION"}`, Related: []string{"get_integration_context", "execute"}, Authority: "Read only; observations can be stale and do not authorize Git mutations, merge, deployment, or release."},
	{Name: "get_unlinked_git_commits", Kind: "mcp_tool", Area: "git", Purpose: "List discovered commits in attached repositories that are not linked to a Feature or Integration.", When: "Use to identify repository work that needs explicit linking in project context.", Inputs: []Field{{"product_id", "string", true, "Product ID"}}, Output: "Product scoped unlinked commit observations.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify Product ID."}, {"VALIDATION", "Correct Product scope."}, {"FORBIDDEN", "Use authorized Product scope."}}, Example: `{"product_id":"PRODUCT"}`, Related: []string{"get_git_state", "get_project_context", "execute"}, Authority: "Read only; listing a commit does not link it, validate its source, or authorize merge/release."},
	{Name: "get_local_git_state", Kind: "mcp_tool", Area: "git", Purpose: "Observe branch, HEAD, dirty state, cached upstream SHA, and ahead/behind for a registered local checkout.", When: "Use before planning local Git synchronization; cached refs are not live remote evidence.", Inputs: []Field{{"product_id", "string", true, "Product scope"}, {"checkout_id", "string", true, "Registered checkout ID"}}, Output: "Observed local Git state for the configured workspace checkout.", Errors: []ErrorInfo{{"VALIDATION", "'server workspace not configured' or 'workspace unavailable'; configure/recover the server workspace."}, {"NOT_FOUND", "'local checkout' missing; rematch workspace or verify ID."}, {"VALIDATION", "'origin no longer matches registered repository; rematch workspace'; rematch before proceeding."}, {"FORBIDDEN", "Use an authorized Product scope."}}, Example: `{"product_id":"PRODUCT","checkout_id":"CHECKOUT"}`, Related: []string{"match_local_repositories", "plan_local_git_sync", "record_local_git_sync"}, Authority: "Read only; does not fetch or prove remote freshness."},
	{Name: "plan_local_git_sync", Kind: "mcp_tool", Area: "git", Purpose: "Persist a bounded FETCH or FAST_FORWARD plan for an external host agent.", When: "Use only after inspecting local checkout state and exact expected HEAD; external executor must review Git configuration and preconditions.", Inputs: []Field{{"product_id", "string", true, "Product scope"}, {"checkout_id", "string", true, "Registered checkout ID"}, {"actor", "string", false, "Attribution, not identity"}, {"mode", "string", true, "FETCH or FAST_FORWARD"}, {"expected_head", "string", true, "Exact current HEAD SHA"}}, Output: "Persisted plan with PLANNED status, executor EXTERNAL_AGENT, observed state, Git arguments, and preconditions.", Errors: []ErrorInfo{{"VALIDATION", "'actor and mode FETCH or FAST_FORWARD required' or 'expected_head must match current exact HEAD'; correct request."}, {"VALIDATION", "'fast-forward requires a clean attached branch without submodules strictly behind origin upstream'; use FETCH or refresh/review checkout."}, {"NOT_FOUND", "Verify Product/checkout registration."}, {"FORBIDDEN", "Use authorized Product write scope."}}, Example: `{"product_id":"PRODUCT","checkout_id":"CHECKOUT","mode":"FETCH","expected_head":"FULL_SHA"}`, Related: []string{"get_local_git_state", "record_local_git_sync"}, Authority: "Records a plan only. The Control Plane does not execute Git, edit source, push, reset, rebase, or run hooks; external agent must enforce listed guards."},
	{Name: "record_local_git_sync", Kind: "mcp_tool", Area: "git", Purpose: "Re-observe a checkout and append the observed result of an external agent's local Git plan.", When: "Call after the external agent has completed its planned operation; provide the exact plan ID.", Inputs: []Field{{"product_id", "string", true, "Product scope"}, {"checkout_id", "string", true, "Registered checkout ID"}, {"actor", "string", false, "Attribution"}, {"plan_id", "string", true, "Previously recorded plan ID"}}, Output: "Append-only OBSERVED result with plan, mode, current state, and FETCH evidence limitation.", Errors: []ErrorInfo{{"VALIDATION", "'actor and plan_id required'; supply both."}, {"NOT_FOUND", "'local sync plan' missing; verify plan ID, checkout, and Product."}, {"VALIDATION", "'fast-forward result does not match planned clean branch and target SHA'; inspect checkout and do not claim completion."}, {"FORBIDDEN", "Use authorized Product write scope."}}, Example: `{"product_id":"PRODUCT","checkout_id":"CHECKOUT","plan_id":"PLAN"}`, Related: []string{"plan_local_git_sync", "get_local_git_state"}, Authority: "Records observation only. FETCH network execution cannot be proven from cached refs; no Git command is run by this tool."},
	{Name: "refresh_integration_git", Kind: "execute_action", Area: "git", Purpose: "Queue a provider refresh of one Integration's Git observation.", When: "Use when the last recorded branch/PR/check observation may be stale.", Inputs: env("integration_id"), Output: "Persistent asynchronous operation handle for provider observation.", Errors: commonErrors, Example: `{"action":"refresh_integration_git","integration_id":"INTEGRATION","data":{}}`, Related: []string{"get_git_state", "get_operation"}, Authority: "Requests observation only; does not merge, edit source, deploy, or release."},
	{Name: "refresh_repository_git", Kind: "execute_action", Area: "git", Purpose: "Queue repository branch/commit discovery for an attached code repository.", When: "Use to refresh discovered commits, including commits not yet linked to work.", Inputs: env("product_id", "repository_id"), Output: "Persistent asynchronous repository scan operation.", Errors: commonErrors, Example: `{"action":"refresh_repository_git","product_id":"PRODUCT","data":{"repository_id":"REPOSITORY"}}`, Related: []string{"get_unlinked_git_commits", "get_operation"}, Authority: "Requests provider observation only; does not edit source or link commits to Features/Integrations."},
	{Name: "record_integration_revision", Kind: "execute_action", Area: "git", Purpose: "Record an immutable caller-supplied source revision for an Integration and repository.", When: "Use after obtaining exact source branch and commit SHAs; include the full commit list ending at head.", Inputs: env("integration_id", "repository_id", "branch", "base_commit", "head_commit", "commits"), Output: "Persisted IntegrationRevision with provenance fields and supplied commit sequence.", Errors: []ErrorInfo{{"NOT_FOUND", "Integration/repository missing; refresh scoped context."}, {"VALIDATION", "Messages include 'revision repository outside feature scope', 'revision repository must be declared by integration', 'valid source branch required', 'base_commit, head_commit and nonempty commits require full lowercase SHA40 or SHA64', 'invalid, mixed-format or duplicate commit', 'head_commit must occur in commits, using the base commit hash format'; correct scope and values."}}, Example: `{"action":"record_integration_revision","integration_id":"INTEGRATION","data":{"repository_id":"REPOSITORY","branch":"feature/work","base_commit":"FULL_SHA","head_commit":"FULL_SHA","commits":["FULL_SHA"]}}`, Related: []string{"get_integration_context", "get_git_state", "plan_composition"}, Authority: "Records caller-supplied claims; it does not verify Git/provider state, edit source, merge, build, deploy, or release."},
	{Name: "create_integration_branch", Kind: "execute_action", Area: "git", Purpose: "Queue explicit approved creation of a managed Integration source branch from an exact commit.", When: "Use only when creating an Integration branch is intended and explicit approve is true.", Inputs: []Field{{"action", "string", true, "Exact action name"}, {"actor", "string", false, "Attribution"}, {"integration_id", "string", true, "Target Integration"}, {"data", "object", true, "Action input"}, {"application_id", "string", true, "Integration-scoped component"}, {"base_commit", "string", true, "Exact source SHA40/SHA64"}, {"approve", "boolean", true, "Explicit approval"}}, Output: "Persistent CREATE_BRANCH operation; worker verifies configured base and created SHA before binding ref.", Errors: []ErrorInfo{{"VALIDATION", "Messages include 'explicit approval and integration-scoped component required', 'attached source repository required', 'source ref operation already active', 'integration already bound', 'exact source commit required'; correct prerequisites."}, {"NOT_FOUND", "Verify Integration, component, repository, and connection."}}, Example: `{"action":"create_integration_branch","integration_id":"INTEGRATION","data":{"application_id":"APP","base_commit":"FULL_SHA","approve":true}}`, Related: []string{"get_operation", "get_integration_context", "protect_integration_revision"}, Authority: "Queues a create-only provider ref operation; does not rewrite history or guarantee administrator-enforced branch protection."},
	{Name: "protect_integration_revision", Kind: "execute_action", Area: "git", Purpose: "Queue create-only provider pinning of an existing Integration revision under a managed ref.", When: "Use when an exact persisted revision must be pinned for later immutable composition.", Inputs: []Field{{"action", "string", true, "Exact action name"}, {"actor", "string", false, "Attribution"}, {"integration_id", "string", true, "Target Integration"}, {"data", "object", true, "Action input"}, {"application_id", "string", true, "Integration-scoped component"}, {"revision_id", "string", true, "Revision belonging to Integration and component repository"}, {"approve", "boolean", true, "Explicit approval"}}, Output: "Persistent PIN_REVISION operation for revision head SHA.", Errors: []ErrorInfo{{"VALIDATION", "Messages include 'explicit approval and integration-scoped component required', 'attached source repository required', 'source ref operation already active', 'revision scope mismatch'; correct prerequisites."}, {"NOT_FOUND", "Verify Integration, component, repository, revision, and connection."}}, Example: `{"action":"protect_integration_revision","integration_id":"INTEGRATION","data":{"application_id":"APP","revision_id":"REVISION","approve":true}}`, Related: []string{"get_operation", "record_integration_revision", "create_integration_branch"}, Authority: "Queues a create-only pinned ref; does not rewrite history or create administrator-enforced GitHub branch protection."},
	{Name: "execute", Kind: "mcp_tool", Area: "work_tracking", Purpose: "Submit one action through the shared domain command application layer.", When: "Use after reading the action schema and relevant scoped context.", Inputs: []Field{{"action", "string", true, "Exact action name"}, {"actor", "string", false, "Attribution; not authentication"}, {"product_id", "string", false, "Product scope when required"}, {"feature_id", "string", false, "Feature scope when required"}, {"integration_id", "string", false, "Integration scope when required"}, {"data", "object", true, "Action-specific fields; GET /api/schema is authoritative"}}, Output: "Action-specific persisted result or asynchronous operation handle.", Errors: commonErrors, Example: `{"action":"execute","actor":"agent/backend","feature_id":"FEATURE","data":{"action":"record_progress","body":"Implemented parser"}}`, Related: []string{"get_tool_guide", "resume"}, Authority: guideAuthority},
	{Name: "POST /api/commands", Kind: "http_route", Area: "work_tracking", Purpose: "Submit one JSON command using the same shared application behavior as MCP execute.", When: "Use for direct HTTP clients that need the command API.", Inputs: []Field{{"Content-Type", "string", true, "Must be application/json"}, {"body", "domain.Command JSON", true, "Command envelope validated by GET /api/schema; body limit 1 MiB"}}, Output: "JSON command result or structured error response.", Errors: []ErrorInfo{{"400", "Invalid JSON, multiple JSON values, or invalid transport command."}, {"415", "Set Content-Type: application/json."}, {"application error", "Correct the command using GET /api/schema and current state."}}, Example: `POST /api/commands {"action":"create_product","actor":"agent/setup","data":{"name":"Payments"}}`, Related: []string{"execute", "GET /api/schema"}, Authority: guideAuthority},
	{Name: "GET /api/features/{id}/context", Kind: "http_route", Area: "work_tracking", Purpose: "Read the full Feature context and history through the operator HTTP context endpoint.", When: "Use when MCP resume and bounded excerpts are insufficient and the caller is permitted to access the operator endpoint.", Inputs: []Field{{"id", "path string", true, "Feature ID"}}, Output: "Feature resume/context projection returned by the application service.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify the Feature ID and Product scope."}}, Example: `GET /api/features/FEATURE/context`, Related: []string{"resume", "GET /api/context/search"}, Authority: "Read only. Context is historical evidence and grants no mutation or release authority."},
	{Name: "get_execution_group", Kind: "mcp_tool", Area: "execution", Purpose: "Restore all members of an execution group with current Integration and execution state.", When: "Call after a supervisor wake or when resuming an orchestrator session.", Inputs: []Field{{"group_id", "string", true, "Execution group ID"}}, Output: "Execution group and current member snapshots.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify the group ID and Product."}}, Example: `{"group_id":"GROUP"}`, Related: []string{"list_executions", "list_execution_events"}, Authority: "Read only; group state does not authorize takeover or external effects."},
	{Name: "list_executions", Kind: "mcp_tool", Area: "execution", Purpose: "List Product-scoped Integration execution records.", When: "Use to inspect active and recent execution leases for a Product.", Inputs: []Field{{"product_id", "string", true, "Product ID"}}, Output: "Product-scoped execution list.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify the Product ID."}}, Example: `{"product_id":"PRODUCT"}`, Related: []string{"get_execution_group", "list_execution_events"}, Authority: "Read only; observation does not grant execution ownership."},
	{Name: "list_execution_events", Kind: "mcp_tool", Area: "execution", Purpose: "List Product-scoped execution events with cursor and delivery filters.", When: "Use to inspect supervisor wake events and their delivery state.", Inputs: []Field{{"product_id", "string", true, "Product ID"}, {"cursor", "string", false, "Stable created_at,id cursor"}, {"limit", "integer", false, "Page size"}, {"unresolved_only", "boolean", false, "Filter unresolved events"}, {"due_only", "boolean", false, "Filter events due for delivery"}}, Output: "Bounded event page with cursor.", Errors: []ErrorInfo{{"VALIDATION", "Correct Product, limit, or cursor."}}, Example: `{"product_id":"PRODUCT","limit":25}`, Related: []string{"claim_execution_event_delivery", "ack_execution_event"}, Authority: "Read only; event observation alone does not acknowledge or claim delivery."},
	{Name: "report_turn_end", Kind: "mcp_tool", Area: "execution", Purpose: "Report a harness turn end for supervisor continuation handling.", When: "Call from the harness turn-end hook with stable session and turn identity when available.", Inputs: []Field{{"product_id", "string", true, "Product ID"}, {"actor", "string", false, "Attribution"}, {"data", "object", true, "harness_kind plus session_ref or tmux_target; turn_id and cwd optional"}}, Output: "Persisted turn-end report and reconciliation status.", Errors: commonErrors, Example: `{"product_id":"PRODUCT","data":{"harness_kind":"codex","session_ref":"SESSION","turn_id":"TURN"}}`, Related: []string{"get_execution_group", "list_execution_events"}, Authority: "Reporting a turn end does not request arbitrary work or grant execution authority."},
	{Name: "claim_execution_event_delivery", Kind: "mcp_tool", Area: "execution", Purpose: "Claim a due supervisor event before waking its registered target.", When: "Claim once before delivery; use the returned delivery_id for acknowledgement.", Inputs: []Field{{"product_id", "string", true, "Product ID"}, {"data", "object", true, "event_id and supervisor identity"}}, Output: "Delivery claim with delivery_id and wake target.", Errors: commonErrors, Example: `{"product_id":"PRODUCT","data":{"event_id":"EVENT","supervisor":"worker-1"}}`, Related: []string{"list_execution_events", "ack_execution_event"}, Authority: "A claim authorizes only delivery of the event to its stored target; it does not change member ownership."},
	{Name: "ack_execution_event", Kind: "mcp_tool", Area: "execution", Purpose: "Acknowledge durable acceptance of a claimed supervisor event.", When: "Call only after the target has accepted the wake; pass the exact delivery_id.", Inputs: []Field{{"product_id", "string", true, "Product ID"}, {"data", "object", true, "event_id and claimed delivery_id"}}, Output: "Acknowledgement result; the event condition remains independently unresolved until work proceeds.", Errors: commonErrors, Example: `{"product_id":"PRODUCT","data":{"event_id":"EVENT","delivery_id":"DELIVERY"}}`, Related: []string{"claim_execution_event_delivery", "get_execution_group"}, Authority: "Acknowledgement records delivery only; it does not resolve the event or authorize takeover."},
	{Name: "resolve_execution_event", Kind: "mcp_tool", Area: "execution", Purpose: "Record resolution of a supervisor event after its underlying condition is addressed.", When: "Use only after verifying the condition is resolved in current execution state.", Inputs: []Field{{"product_id", "string", true, "Product ID"}, {"data", "object", true, "event_id and resolution evidence"}}, Output: "Resolved event record.", Errors: commonErrors, Example: `{"product_id":"PRODUCT","data":{"event_id":"EVENT","resolution":"member resumed"}}`, Related: []string{"get_execution_group", "list_execution_events"}, Authority: "Event resolution records orchestration state; it does not complete Integration acceptance."},
	{Name: "GET /api/execution-groups/{id}", Kind: "http_route", Area: "execution", Purpose: "Read one execution group and its current members.", When: "Use from an HTTP client when resuming a known group.", Inputs: []Field{{"id", "path string", true, "Execution group ID"}}, Output: "Group and member state.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify the group ID."}}, Example: `GET /api/execution-groups/GROUP`, Related: []string{"get_execution_group"}, Authority: "Read only."},
	{Name: "GET /api/products/{id}/executions", Kind: "http_route", Area: "execution", Purpose: "List Product execution records over HTTP.", When: "Use to inspect executions without an MCP client.", Inputs: []Field{{"id", "path string", true, "Product ID"}}, Output: "Product-scoped execution records.", Errors: []ErrorInfo{{"NOT_FOUND", "Verify the Product ID."}}, Example: `GET /api/products/PRODUCT/executions`, Related: []string{"list_executions"}, Authority: "Read only."},
	{Name: "GET /api/products/{id}/execution-events", Kind: "http_route", Area: "execution", Purpose: "List Product execution events with cursor and event filters.", When: "Use to inspect orchestration delivery events over HTTP.", Inputs: []Field{{"id", "path string", true, "Product ID"}, {"cursor", "query string", false, "Stable event cursor"}, {"limit", "query integer", false, "Page size; default 100"}, {"unresolved_only", "query boolean", false, "Filter unresolved"}, {"due_only", "query boolean", false, "Filter due events"}}, Output: "Filtered execution event page.", Errors: []ErrorInfo{{"VALIDATION", "Correct query values and cursor."}}, Example: `GET /api/products/PRODUCT/execution-events?limit=25`, Related: []string{"list_execution_events"}, Authority: "Read only."},
	{Name: "create_execution_group", Kind: "execute_action", Area: "execution", Purpose: "Create an orchestrator-owned group for coordinating multiple Integration members.", When: "Start multi-feature work when one orchestrator coordinates member executions.", Inputs: env("product_id", "orchestrator_harness", "name"), Output: "Created execution group with group_id and initial state.", Errors: commonErrors, Example: `{"action":"create_execution_group","actor":"agent/orchestrator","product_id":"PRODUCT","data":{"name":"release","orchestrator_harness":{"kind":"tmux","target":"session:window.pane"}}}`, Related: []string{"add_execution_group_member", "set_execution_group_harness", "get_execution_group"}, Authority: guideAuthority},
	{Name: "add_execution_group_member", Kind: "execute_action", Area: "execution", Purpose: "Add an Integration to an execution group with its desired completion state.", When: "Use once per member selected for the coordinated work.", Inputs: env("product_id", "integration_id", "group_id", "desired_state"), Output: "Updated group member and desired state.", Errors: commonErrors, Example: `{"action":"add_execution_group_member","actor":"agent/orchestrator","product_id":"PRODUCT","integration_id":"INTEGRATION","data":{"group_id":"GROUP","desired_state":"complete"}}`, Related: []string{"create_execution_group", "claim_execution", "get_execution_group"}, Authority: guideAuthority},
	{Name: "remove_execution_group_member", Kind: "execute_action", Area: "execution", Purpose: "Remove a member from an execution group.", When: "Use when the group scope changes before the member is complete.", Inputs: env("product_id", "integration_id", "group_id"), Output: "Updated execution group.", Errors: commonErrors, Example: `{"action":"remove_execution_group_member","actor":"agent/orchestrator","product_id":"PRODUCT","integration_id":"INTEGRATION","data":{"group_id":"GROUP"}}`, Related: []string{"get_execution_group", "add_execution_group_member"}, Authority: guideAuthority},
	{Name: "set_execution_group_harness", Kind: "execute_action", Area: "execution", Purpose: "Register or replace the orchestrator harness target for a group.", When: "Set at group creation or after the orchestrator restarts in a new session.", Inputs: env("product_id", "group_id", "orchestrator_harness"), Output: "Updated harness registration.", Errors: commonErrors, Example: `{"action":"set_execution_group_harness","actor":"agent/orchestrator","product_id":"PRODUCT","data":{"group_id":"GROUP","orchestrator_harness":{"kind":"tmux","target":"session:window.pane"}}}`, Related: []string{"create_execution_group", "get_execution_group"}, Authority: guideAuthority},
	{Name: "claim_execution", Kind: "execute_action", Area: "execution", Purpose: "Claim an Integration execution and obtain its current fencing token.", When: "Claim before doing member work; retain the returned fence for fenced writes.", Inputs: env("product_id", "integration_id", "executor", "execution_group_id"), Output: "Execution claim including execution_fence and lease state.", Errors: commonErrors, Example: `{"action":"claim_execution","actor":"agent/worker","product_id":"PRODUCT","integration_id":"INTEGRATION","data":{"executor":"agent/worker","execution_group_id":"GROUP"}}`, Related: []string{"heartbeat_execution", "record_progress", "transition_integration"}, Authority: guideAuthority},
	{Name: "heartbeat_execution", Kind: "execute_action", Area: "execution", Purpose: "Renew the current executor's active execution lease using its fence.", When: "Call while actively working and before the lease expires.", Inputs: env("product_id", "integration_id", "execution_fence"), Output: "Updated lease and execution state.", Errors: commonErrors, Example: `{"action":"heartbeat_execution","actor":"agent/worker","product_id":"PRODUCT","integration_id":"INTEGRATION","data":{"execution_fence":1}}`, Related: []string{"claim_execution", "takeover_execution", "release_execution"}, Authority: guideAuthority},
	{Name: "takeover_execution", Kind: "execute_action", Area: "execution", Purpose: "Take over another executor's lost or stalled execution after the delivered event supports takeover.", When: "Use only after checking current execution/group state and the relevant delivered event.", Inputs: env("product_id", "integration_id", "executor"), Output: "New claim and incremented execution fence.", Errors: commonErrors, Example: `{"action":"takeover_execution","actor":"agent/recovery","product_id":"PRODUCT","integration_id":"INTEGRATION","data":{"executor":"agent/recovery"}}`, Related: []string{"get_execution_group", "heartbeat_execution", "claim_execution"}, Authority: guideAuthority},
	{Name: "report_execution", Kind: "execute_action", Area: "execution", Purpose: "Record waiting, blocked, paused, or active execution status and its next action.", When: "Use to explain a nonterminal state to the orchestrator and supervisor.", Inputs: env("product_id", "integration_id", "status", "next_action"), Output: "Persisted execution status event.", Errors: commonErrors, Example: `{"action":"report_execution","actor":"agent/worker","product_id":"PRODUCT","integration_id":"INTEGRATION","data":{"status":"waiting","next_action":"wait for review"}}`, Related: []string{"get_execution_group", "release_execution"}, Authority: guideAuthority},
	{Name: "release_execution", Kind: "execute_action", Area: "execution", Purpose: "Release the active execution lease with its outcome.", When: "Release when stopping; use completed only after the member reaches desired_state.", Inputs: env("product_id", "integration_id", "outcome"), Output: "Persisted release event and execution state.", Errors: commonErrors, Example: `{"action":"release_execution","actor":"agent/worker","product_id":"PRODUCT","integration_id":"INTEGRATION","data":{"outcome":"completed"}}`, Related: []string{"heartbeat_execution", "get_execution_group"}, Authority: guideAuthority},
	{Name: "set_desired_state", Kind: "execute_action", Area: "execution", Purpose: "Set the target lifecycle state for an Integration execution.", When: "Set the completion target before coordinating member execution.", Inputs: env("product_id", "integration_id", "desired_state"), Output: "Updated desired state.", Errors: commonErrors, Example: `{"action":"set_desired_state","actor":"agent/orchestrator","product_id":"PRODUCT","integration_id":"INTEGRATION","data":{"desired_state":"complete"}}`, Related: []string{"add_execution_group_member", "complete_integration"}, Authority: guideAuthority},
	{Name: "set_product_stalled_timeout", Kind: "execute_action", Area: "execution", Purpose: "Configure the Product's stalled-execution timeout.", When: "Use only when the operator has selected a Product-specific timeout policy.", Inputs: env("product_id", "timeout_seconds"), Output: "Updated timeout setting.", Errors: commonErrors, Example: `{"action":"set_product_stalled_timeout","actor":"operator","product_id":"PRODUCT","data":{"timeout_seconds":3600}}`, Related: []string{"get_execution_group", "list_executions"}, Authority: guideAuthority},
	{Name: "record_progress", Kind: "execute_action", Area: "work_tracking", Purpose: "Append a progress update to a Feature or Integration record.", When: "Record meaningful implementation progress and pass execution_fence when working under a claimed execution.", Inputs: env("feature_id", "integration_id", "execution_fence", "body", "completed", "remaining", "next", "warnings"), Output: "Persisted progress record ID.", Errors: commonErrors, Example: `{"action":"record_progress","actor":"agent/backend","feature_id":"FEATURE","integration_id":"INTEGRATION","data":{"body":"Implemented parser","execution_fence":1}}`, Related: []string{"resume", "handoff", "claim_execution"}, Authority: guideAuthority},
	{Name: "record_decision", Kind: "execute_action", Area: "work_tracking", Purpose: "Append a durable engineering decision to a Feature.", When: "Use for a decision future work must preserve, with its rationale.", Inputs: env("feature_id", "integration_id", "reason", "rationale", "body"), Output: "Persisted decision record ID.", Errors: commonErrors, Example: `{"action":"record_decision","actor":"agent/backend","feature_id":"FEATURE","data":{"reason":"Keep v1","rationale":"Existing clients cannot migrate atomically"}}`, Related: []string{"resume", "record_progress"}, Authority: guideAuthority},
	{Name: "record_discovery", Kind: "execute_action", Area: "work_tracking", Purpose: "Append a discovery or observed fact to a Feature's history.", When: "Use for evidence-backed findings that affect implementation or follow-up.", Inputs: env("feature_id", "integration_id", "title", "body", "observations"), Output: "Persisted discovery record ID.", Errors: commonErrors, Example: `{"action":"record_discovery","actor":"agent/backend","feature_id":"FEATURE","data":{"title":"Legacy path exists","body":"Found callers in the compatibility package"}}`, Related: []string{"resume", "record_progress"}, Authority: guideAuthority},
	{Name: "add_blocker", Kind: "execute_action", Area: "work_tracking", Purpose: "Record a blocker on a Feature or Integration.", When: "Use when progress depends on a concrete external or internal condition.", Inputs: env("feature_id", "integration_id", "title", "body", "owner", "severity"), Output: "Persisted blocker ID.", Errors: commonErrors, Example: `{"action":"add_blocker","actor":"agent/backend","feature_id":"FEATURE","data":{"title":"Waiting on API access","body":"Need the staging credential reference"}}`, Related: []string{"resolve_blocker", "handoff", "resume"}, Authority: guideAuthority},
	{Name: "resolve_blocker", Kind: "execute_action", Area: "work_tracking", Purpose: "Append a resolution for an existing blocker.", When: "Use after the blocking condition is actually resolved; preserve the resolution evidence.", Inputs: env("id", "body"), Output: "Updated blocker status and resolution event.", Errors: commonErrors, Example: `{"action":"resolve_blocker","actor":"agent/backend","id":"BLOCKER","data":{"body":"Access was granted"}}`, Related: []string{"add_blocker", "resume"}, Authority: guideAuthority},
	{Name: "handoff", Kind: "execute_action", Area: "work_tracking", Purpose: "Append a structured checkpoint for the next agent or session.", When: "Use before ending, pausing, or transferring work; include current and next steps.", Inputs: env("feature_id", "integration_id", "completed", "current", "remaining", "next", "warnings"), Output: "Persisted handoff record ID.", Errors: commonErrors, Example: `{"action":"handoff","actor":"agent/backend","feature_id":"FEATURE","integration_id":"INTEGRATION","data":{"completed":["parser"],"current":"tests pending","remaining":["run checks"],"next":["run go test"],"warnings":[]}}`, Related: []string{"resume", "record_progress"}, Authority: guideAuthority},
	{Name: "create_product", Kind: "execute_action", Area: "work_tracking", Purpose: "Create a Product root for project context and tracked work.", When: "Use only when no matching Product exists; first inspect get_state.", Inputs: env("name", "description"), Output: "Created Product ID.", Errors: commonErrors, Example: `{"action":"create_product","actor":"agent/setup","data":{"name":"Payments","description":"Payments service"}}`, Related: []string{"get_state", "create_feature"}, Authority: guideAuthority},
	{Name: "create_feature", Kind: "execute_action", Area: "work_tracking", Purpose: "Create a Feature with problem, goal, and optional repository scope.", When: "Use for a distinct outcome; select an existing Product first.", Inputs: env("product_id", "title", "problem", "goal", "context", "repositories", "acceptance_criteria"), Output: "Created Feature ID.", Errors: commonErrors, Example: `{"action":"create_feature","actor":"agent/setup","product_id":"PRODUCT","data":{"title":"Checkout retry","problem":"Transient failures lose orders","goal":"Retry safely"}}`, Related: []string{"get_state", "create_integration", "resume"}, Authority: guideAuthority},
	{Name: "update_feature", Kind: "execute_action", Area: "work_tracking", Purpose: "Update a Feature's editable descriptive fields and status.", When: "Use after reading its current scoped context.", Inputs: env("feature_id", "title", "problem", "goal", "context", "status", "repositories", "acceptance_criteria"), Output: "Updated Feature record.", Errors: commonErrors, Example: `{"action":"update_feature","actor":"agent/backend","feature_id":"FEATURE","data":{"goal":"Retry safely"}}`, Related: []string{"resume", "get_state"}, Authority: guideAuthority},
	{Name: "create_integration", Kind: "execute_action", Area: "work_tracking", Purpose: "Create a scoped implementation Integration under a Feature.", When: "Create a trackable unit before implementation and start it when work begins.", Inputs: env("feature_id", "title", "objective", "context", "repositories", "dependencies", "acceptance_criteria", "working_areas"), Output: "Created Integration ID.", Errors: commonErrors, Example: `{"action":"create_integration","actor":"agent/backend","feature_id":"FEATURE","data":{"title":"Add retry policy","objective":"Retry transient errors"}}`, Related: []string{"start_integration", "resume", "create_feature"}, Authority: guideAuthority},
	{Name: "update_integration", Kind: "execute_action", Area: "work_tracking", Purpose: "Update an Integration's editable fields and scope.", When: "Use after reading current Integration context; preserve provenance and lifecycle rules.", Inputs: env("integration_id", "title", "objective", "context", "repositories", "dependencies", "acceptance_criteria", "working_areas", "remaining"), Output: "Updated Integration record.", Errors: commonErrors, Example: `{"action":"update_integration","actor":"agent/backend","integration_id":"INTEGRATION","data":{"objective":"Retry transient errors safely"}}`, Related: []string{"resume", "transition_integration"}, Authority: guideAuthority},
	{Name: "start_integration", Kind: "execute_action", Area: "work_tracking", Purpose: "Record that implementation has started for an Integration.", When: "Start the Integration before editing its implementation.", Inputs: env("integration_id"), Output: "Updated Integration lifecycle state and event.", Errors: commonErrors, Example: `{"action":"start_integration","actor":"agent/backend","integration_id":"INTEGRATION","data":{}}`, Related: []string{"record_progress", "transition_integration"}, Authority: guideAuthority},
	{Name: "transition_integration", Kind: "execute_action", Area: "work_tracking", Purpose: "Request a lifecycle transition for an Integration.", When: "Use for supported non-released transitions; include the active execution_fence when claimed.", Inputs: env("integration_id", "status", "execution_fence"), Output: "Updated Integration status and event.", Errors: commonErrors, Example: `{"action":"transition_integration","actor":"agent/backend","integration_id":"INTEGRATION","data":{"status":"active","execution_fence":1}}`, Related: []string{"start_integration", "complete_integration"}, Authority: guideAuthority},
	{Name: "complete_integration", Kind: "execute_action", Area: "work_tracking", Purpose: "Mark an Integration complete under its acceptance and lifecycle rules.", When: "Use only after its intended work is done and acceptance evidence is recorded.", Inputs: env("integration_id"), Output: "Updated Integration status and completion event.", Errors: commonErrors, Example: `{"action":"complete_integration","actor":"agent/backend","integration_id":"INTEGRATION","data":{}}`, Related: []string{"record_progress", "handoff"}, Authority: guideAuthority},
	{Name: "create_gate", Kind: "execute_action", Area: "work_tracking", Purpose: "Create a Feature gate with required integrations and release conditions.", When: "Use when the Feature needs an explicit review or acceptance gate.", Inputs: env("feature_id", "title", "reason", "integration_ids", "preconditions", "expected_outcomes", "blocking", "blocks_release"), Output: "Created Gate ID.", Errors: commonErrors, Example: `{"action":"create_gate","actor":"agent/backend","feature_id":"FEATURE","data":{"title":"API review","reason":"Verify compatibility","integration_ids":["INTEGRATION"]}}`, Related: []string{"add_check", "record_check_result"}, Authority: guideAuthority},
	{Name: "add_check", Kind: "execute_action", Area: "work_tracking", Purpose: "Add a verification check to a Gate.", When: "Define the required check before recording its result.", Inputs: env("gate_id", "title", "mechanism", "instructions", "blocking"), Output: "Created Check ID.", Errors: commonErrors, Example: `{"action":"add_check","actor":"agent/backend","gate_id":"GATE","data":{"title":"Unit tests","mechanism":"command","instructions":"go test ./..."}}`, Related: []string{"record_check_result", "create_gate"}, Authority: guideAuthority},
	{Name: "record_check_result", Kind: "execute_action", Area: "work_tracking", Purpose: "Append a result for a Gate check.", When: "Record the actual outcome and evidence; rerun blocking checks after relevant changes.", Inputs: env("check_id", "result", "body", "evidence", "logs", "url"), Output: "Persisted CheckResult ID.", Errors: commonErrors, Example: `{"action":"record_check_result","actor":"agent/backend","check_id":"CHECK","data":{"result":"passed","body":"go test ./... passed"}}`, Related: []string{"record_finding", "create_gate"}, Authority: guideAuthority},
	{Name: "record_finding", Kind: "execute_action", Area: "work_tracking", Purpose: "Record a finding linked to a CheckResult.", When: "Use for a concrete issue observed during verification or review.", Inputs: env("result_id", "title", "severity", "body", "url"), Output: "Persisted Finding ID.", Errors: commonErrors, Example: `{"action":"record_finding","actor":"agent/backend","result_id":"RESULT","data":{"title":"Missing retry cap","severity":"major","body":"Retries are unbounded"}}`, Related: []string{"resolve_finding", "record_check_result"}, Authority: guideAuthority},
	{Name: "resolve_finding", Kind: "execute_action", Area: "work_tracking", Purpose: "Append resolution evidence for a Finding.", When: "Use after fixing or otherwise resolving the finding; blocking gates still require a passing rerun.", Inputs: env("id", "body"), Output: "Updated Finding status and resolution event.", Errors: commonErrors, Example: `{"action":"resolve_finding","actor":"agent/backend","id":"FINDING","data":{"body":"Added a bounded retry count"}}`, Related: []string{"record_finding", "record_check_result"}, Authority: guideAuthority},
}

var commonErrors = []ErrorInfo{{"VALIDATION", "Correct the action envelope or values using GET /api/schema."}, {"NOT_FOUND", "Refresh scoped context and verify the ID."}, {"CONFLICT", "Re-read current state; do not replay with changed intent."}, {"STALE_FENCE", "Resume the same executor with the current fence or follow takeover policy."}}

const guideAuthority = "Documentation is not authorization. This command does not authorize arbitrary source edits, shell/SQL execution, deployment, release, or bypass of domain policy and required approval."

func env(fields ...string) []Field {
	out := []Field{{Name: "action", Type: "string", Required: true, Meaning: "Exact action name"}, {Name: "actor", Type: "string", Required: false, Meaning: "Attribution; not authentication"}, {Name: "data", Type: "object", Required: true, Meaning: "Action-specific values; see GET /api/schema"}}
	for _, n := range fields {
		out = append(out, Field{Name: n, Type: "string", Required: false, Meaning: "Action-specific value; exact constraints are enforced by GET /api/schema"})
	}
	return out
}

// pendingDocumentation is deliberately explicit. Each value is kind:name and
// will be replaced by later briefs with complete records.
var pendingDocumentation = []string{
	"mcp_tool:assemble_environment",
	"mcp_tool:capture_candidate_dev_baseline",
	"mcp_tool:claim_composition_conflict",
	"mcp_tool:confirm_candidate_dev_restoration",
	"mcp_tool:create_backup",
	"mcp_tool:create_hotfix",
	"mcp_tool:create_test_scenario",
	"mcp_tool:deploy_candidate_dev",
	"mcp_tool:deploy_existing_artifact",
	"mcp_tool:deploy_integration",
	"mcp_tool:export_workspace_configuration",
	"mcp_tool:get_agent_kit",
	"mcp_tool:get_agent_skill",
	"mcp_tool:get_artifact_retention",
	"mcp_tool:get_attention_required",
	"mcp_tool:get_backup_capabilities",
	"mcp_tool:get_code_api",
	"mcp_tool:get_environment_state",
	"mcp_tool:get_intelligence_task",
	"mcp_tool:get_operation",
	"mcp_tool:get_product_configuration",
	"mcp_tool:get_runtime_report",
	"mcp_tool:get_status_schema",
	"mcp_tool:get_tool_guide",
	"mcp_tool:get_worker_history",
	"mcp_tool:get_worker_job",
	"mcp_tool:import_workspace_configuration",
	"mcp_tool:list_pipelines",
	"mcp_tool:list_signatures",
	"mcp_tool:match_local_repositories",
	"mcp_tool:prepare_release_candidate",
	"mcp_tool:promote_release_candidate",
	"mcp_tool:reconcile_composition",
	"mcp_tool:record_conflict_resolution",
	"mcp_tool:record_runtime_observation",
	"mcp_tool:record_scenario_run",
	"mcp_tool:refresh_environment_runtime",
	"mcp_tool:release_candidate_dev",
	"mcp_tool:release_candidate_dev_after_restoration",
	"mcp_tool:request_candidate_dev_restoration",
	"mcp_tool:reserve_candidate_dev",
	"mcp_tool:restore_backup",
	"mcp_tool:resume",
	"mcp_tool:revise_test_scenario",
	"mcp_tool:scan_workspace",
	"mcp_tool:search_product_knowledge",
	"mcp_tool:set_project_knowledge",
	"mcp_tool:sign_artifact",
	"mcp_tool:sync_pull_request_review",
	"mcp_tool:update_environment_feature",
	"mcp_tool:upsert_product_knowledge",
	"mcp_tool:verify_candidate_dev_restoration",
	"mcp_tool:verify_candidate_dev_runtime",
	"mcp_tool:verify_conflict_resolution",
	"execute_action:assemble_environment",
	"execute_action:cancel_intelligence_task",
	"execute_action:cancel_native_delivery",
	"execute_action:cancel_worker_job",
	"execute_action:capture_candidate_dev_baseline",
	"execute_action:claim_composition_conflict",
	"execute_action:classify_repository",
	"execute_action:configure_artifact_retention",
	"execute_action:configure_component",
	"execute_action:configure_environment",
	"execute_action:configure_publication",
	"execute_action:confirm_candidate_dev_restoration",
	"execute_action:create_application",
	"execute_action:create_environment",
	"execute_action:create_external_system",
	"execute_action:create_github_connection",
	"execute_action:create_hotfix",
	"execute_action:create_repository",
	"execute_action:create_system_relationship",
	"execute_action:create_test_scenario",
	"execute_action:delete_product",
	"execute_action:deploy_existing_artifact",
	"execute_action:deploy_integration",
	"execute_action:grant_connection",
	"execute_action:import_repository",
	"execute_action:pin_artifact",
	"execute_action:plan_composition",
	"execute_action:plan_release",
	"execute_action:prepare_release_candidate",
	"execute_action:promote_release_candidate",
	"execute_action:queue_intelligence_task",
	"execute_action:queue_worker_job",
	"execute_action:queue_worker_publication",
	"execute_action:reconcile_composition",
	"execute_action:reconcile_worker_job",
	"execute_action:reconcile_worker_publication",
	"execute_action:record_conflict_resolution",
	"execute_action:record_package_artifact",
	"execute_action:record_release",
	"execute_action:record_runtime_observation",
	"execute_action:record_scenario_run",
	"execute_action:refresh_environment_runtime",
	"execute_action:refresh_registry_inventory",
	"execute_action:release_artifact_pin",
	"execute_action:release_candidate_dev",
	"execute_action:release_candidate_dev_after_restoration",
	"execute_action:request_candidate_dev_restoration",
	"execute_action:reserve_candidate_dev",
	"execute_action:revise_test_scenario",
	"execute_action:select_composition",
	"execute_action:set_external_scope",
	"execute_action:update_environment",
	"execute_action:update_environment_feature",
	"execute_action:update_external_system",
	"execute_action:verify_candidate_dev_restoration",
	"execute_action:verify_candidate_dev_runtime",
	"execute_action:verify_conflict_resolution",
	"http_route:/mcp",
	"http_route:GET /api/agent-kit",
	"http_route:GET /api/attention",
	"http_route:GET /api/build-profiles",
	"http_route:GET /api/builds",
	"http_route:GET /api/builds/{id}",
	"http_route:GET /api/code-sdk/rcp.ts",
	"http_route:GET /api/connections/{id}/repositories",
	"http_route:GET /api/context/excerpt",
	"http_route:GET /api/context/graph",
	"http_route:GET /api/context/search",
	"http_route:GET /api/deployments",
	"http_route:GET /api/deployments/{id}",
	"http_route:GET /api/environments/{id}/state",
	"http_route:GET /api/features/{id}/graph",
	"http_route:GET /api/integrations/{id}/context",
	"http_route:GET /api/integrations/{id}/git",
	"http_route:GET /api/intelligence-tasks/{id}",
	"http_route:GET /api/operations/{id}",
	"http_route:GET /api/pipelines",
	"http_route:GET /api/products/{id}/configuration",
	"http_route:GET /api/products/{id}/context",
	"http_route:GET /api/products/{id}/knowledge/search",
	"http_route:GET /api/products/{id}/local-checkouts/{checkout}/git",
	"http_route:GET /api/products/{id}/retention",
	"http_route:GET /api/products/{id}/unlinked-git-commits",
	"http_route:GET /api/products/{id}/workspace-config",
	"http_route:GET /api/repositories/{id}/branches",
	"http_route:GET /api/runtime-reports",
	"http_route:GET /api/schema",
	"http_route:GET /api/signatures",
	"http_route:GET /api/state",
	"http_route:GET /api/statuses",
	"http_route:GET /api/tools",
	"http_route:GET /api/ui-state",
	"http_route:GET /api/worker-history",
	"http_route:GET /api/worker-jobs/{id}",
	"http_route:GET /healthz",
	"http_route:POST /api/backups/export",
	"http_route:POST /api/backups/restore",
	"http_route:POST /api/builds",
	"http_route:POST /api/builds/{id}/cancel",
	"http_route:POST /api/builds/{id}/close",
	"http_route:POST /api/compositions/{id}/reconcile",
	"http_route:POST /api/connections/{id}/test",
	"http_route:POST /api/deployments",
	"http_route:POST /api/deployments/{id}/close",
	"http_route:POST /api/deployments/{id}/reconcile",
	"http_route:POST /api/features/{id}/hotfix",
	"http_route:POST /api/features/{id}/update-environment",
	"http_route:POST /api/integrations/{id}/deploy",
	"http_route:POST /api/local-git/plan",
	"http_route:POST /api/local-git/record",
	"http_route:POST /api/operations/{id}/runtime",
	"http_route:POST /api/products/{id}/assemble-environment",
	"http_route:POST /api/products/{id}/candidate-dev/capture-baseline",
	"http_route:POST /api/products/{id}/candidate-dev/confirm-restoration",
	"http_route:POST /api/products/{id}/candidate-dev/deploy",
	"http_route:POST /api/products/{id}/candidate-dev/release",
	"http_route:POST /api/products/{id}/candidate-dev/release-after-restoration",
	"http_route:POST /api/products/{id}/candidate-dev/request-restoration",
	"http_route:POST /api/products/{id}/candidate-dev/reserve",
	"http_route:POST /api/products/{id}/candidate-dev/verify-restoration",
	"http_route:POST /api/products/{id}/candidate-dev/verify-runtime",
	"http_route:POST /api/products/{id}/conflicts/claim",
	"http_route:POST /api/products/{id}/conflicts/resolution",
	"http_route:POST /api/products/{id}/conflicts/verify",
	"http_route:POST /api/products/{id}/knowledge",
	"http_route:POST /api/products/{id}/knowledge/nodes",
	"http_route:POST /api/release-candidates",
	"http_route:POST /api/release-candidates/{id}/promote",
	"http_route:POST /api/reviews/sync",
	"http_route:POST /api/runtime-reports",
	"http_route:POST /api/scenario-runs",
	"http_route:POST /api/signatures",
	"http_route:POST /api/test-scenarios",
	"http_route:POST /api/test-scenarios/revisions",
	"http_route:POST /api/tools/{name}",
	"http_route:POST /api/workspaces/import",
	"http_route:POST /api/workspaces/match",
	"http_route:POST /api/workspaces/scan",
}

func PendingDocumentation() []string { return append([]string(nil), pendingDocumentation...) }

func Registry() []ToolEntry {
	out := append([]ToolEntry(nil), documented...)
	for i := range out {
		e := &out[i]
		if e.Kind != "execute_action" {
			continue
		}
		for j := range e.Inputs {
			f := &e.Inputs[j]
			f.Required = requiredScope[e.Name][f.Name] || requiredData[e.Name][f.Name] || f.Name == "action" || f.Name == "data"
			if t := fieldTypes[f.Name]; t != "" {
				f.Type = t
			}
		}
	}
	// These domain actions are also registered as named MCP wrappers. Their
	// wrapper uses the same action data schema with the scopes lifted to fields.
	for _, e := range documented {
		if e.Kind != "execute_action" || e.Area != "execution" {
			continue
		}
		mcpEntry := e
		mcpEntry.Kind = "mcp_tool"
		mcpEntry.Purpose = "Named MCP wrapper for the " + e.Name + " execution command. " + e.Purpose
		mcpEntry.Example = strings.Replace(e.Example, `"action":"`+e.Name+`",`, ``, 1)
		fields := mcpEntry.Inputs[:0]
		for _, f := range mcpEntry.Inputs {
			if f.Name != "action" {
				fields = append(fields, f)
			}
		}
		mcpEntry.Inputs = fields
		out = append(out, mcpEntry)
	}
	return out
}

// Required fields are transcribed from CommandSchema's per-action oneOf table.
var requiredScope = map[string]map[string]bool{
	"create_execution_group": {"product_id": true}, "add_execution_group_member": {"product_id": true, "integration_id": true},
	"remove_execution_group_member": {"product_id": true, "integration_id": true}, "set_execution_group_harness": {"product_id": true},
	"claim_execution": {"product_id": true, "integration_id": true}, "heartbeat_execution": {"product_id": true, "integration_id": true},
	"takeover_execution": {"product_id": true, "integration_id": true}, "report_execution": {"product_id": true, "integration_id": true},
	"release_execution": {"product_id": true, "integration_id": true}, "set_desired_state": {"product_id": true, "integration_id": true}, "set_product_stalled_timeout": {"product_id": true},
	"record_progress": {"feature_id": true}, "record_decision": {"feature_id": true}, "record_discovery": {"feature_id": true}, "add_blocker": {"feature_id": true}, "handoff": {"feature_id": true},
	"create_product": {}, "create_feature": {"product_id": true}, "update_feature": {"feature_id": true}, "create_integration": {"feature_id": true}, "update_integration": {"integration_id": true},
	"start_integration": {"integration_id": true}, "transition_integration": {"integration_id": true}, "complete_integration": {"integration_id": true},
	"resolve_blocker": {"id": true}, "resolve_finding": {"id": true}, "create_gate": {"feature_id": true}, "add_check": {"gate_id": true}, "record_check_result": {"check_id": true}, "record_finding": {"result_id": true},
	"create_environment": {"product_id": true}, "update_environment": {"id": true},
}
var requiredData = map[string]map[string]bool{
	"create_product": {"name": true}, "create_feature": {"title": true, "problem": true, "goal": true}, "create_integration": {"title": true, "objective": true},
	"transition_integration": {"status": true}, "record_decision": {"reason": true}, "resolve_blocker": {"body": true}, "resolve_finding": {"body": true},
	"create_gate": {"title": true, "reason": true, "integration_ids": true}, "add_check": {"title": true, "mechanism": true}, "record_check_result": {"result": true},
	"record_finding": {"title": true, "severity": true}, "create_environment": {"name": true},
}
var fieldTypes = map[string]string{"completed": "array[string]", "remaining": "array[string]", "next": "array[string]", "warnings": "array[string]", "integration_ids": "array[string]", "repositories": "array[string]", "acceptance_criteria": "array[string]", "preconditions": "array[string]", "expected_outcomes": "array[string]", "blocking": "boolean", "blocks_release": "boolean", "execution_fence": "integer", "timeout_seconds": "integer", "orchestrator_harness": "object"}

// ToolGuide returns either the area-grouped compact index or an exact entry.
func ToolGuide(name string) any {
	entries := Registry()
	if name != "" {
		for _, e := range entries {
			if e.Name == name {
				b, _ := json.Marshal(e)
				if len(b) <= 32*1024 {
					return e
				}
				return map[string]string{"error": "guide entry exceeds output bound"}
			}
		}
		return map[string]string{"error": "unknown guide name"}
	}
	type item struct {
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		Purpose string `json:"purpose"`
	}
	areas := map[string][]item{}
	for _, e := range entries {
		areas[e.Area] = append(areas[e.Area], item{e.Name, e.Kind, e.Purpose})
	}
	b, _ := json.Marshal(areas)
	if len(b) > 32*1024 {
		return map[string]string{"error": "guide index exceeds output bound"}
	}
	return areas
}

func ValidateActionCoverage() error {
	known := map[string]bool{}
	for _, a := range domain.Actions {
		known[a] = true
	}
	for _, e := range documented {
		if e.Kind == "execute_action" && !known[e.Name] {
			return fmt.Errorf("registry action %q does not exist", e.Name)
		}
	}
	return nil
}
