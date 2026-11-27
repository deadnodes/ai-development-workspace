package transport

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
)

var privateMCPReads = map[string]string{
	"create_backup": "CreateBackup", "export_workspace_configuration": "ExportWorkspace",
	"get_state": "StatePage StatePageProduct StatePageFeature", "resume": "ResumeSummary",
	"get_project_context": "ProjectContextSummary", "search_product_knowledge": "SearchKnowledge",
	"search_context": "SearchContext", "get_context_excerpt": "ContextExcerpt", "graph_neighbors": "GraphNeighbors",
	"get_code_map": "GetCodeMap", "search_code": "SearchCode", "get_code_file_outline": "GetCodeFileOutline", "get_code_index_status": "GetCodeIndexStatus",
	"get_execution_group": "get_execution_group", "list_executions": "list_executions", "list_execution_events": "list_execution_events",
	"get_attention_required": "list_attention", "get_feature_graph": "get_feature_graph", "get_operation": "get_operation",
	"get_intelligence_task": "get_intelligence_task", "get_environment_state": "get_environment_state",
	"get_integration_context": "get_integration_context", "get_git_state": "get_integration_git", "get_unlinked_git_commits": "get_unlinked_git_commits",
	"get_product_configuration": "get_product_configuration", "get_artifact_retention": "get_artifact_retention", "get_local_git_state": "LocalGitState",
	"get_worker_job": "get_worker_job", "get_worker_history": "WorkerHistoryJobs WorkerHistoryAttempts WorkerHistoryLinks",
	"get_runtime_report": "GetRuntimeReport", "list_build_profiles": "ListBuildProfiles", "list_builds": "ListBuilds", "get_build": "GetBuild",
	"list_deployments": "ListDeployments", "get_deployment": "GetDeployment", "list_pipelines": "ListPipelines", "list_signatures": "ListSignatures",
	"list_prod_promotions": "ListProdPromotions", "get_prod_promotion": "GetProdPromotion", "sync_pull_request_review": "SyncReviewPreview ReadReferences",
}

var privateHTTPReads = map[string]string{
	"GET /api/state": "State", "GET /api/ui-state": "UIState UIStateProduct UIStateFeature",
	"GET /api/products/{id}/context": "ProjectContext", "GET /api/features/{id}/context": "Resume",
	"GET /api/products/{id}/knowledge/search": "SearchKnowledge", "GET /api/products/{id}/workspace-config": "ExportWorkspace",
	"GET /api/context/search": "SearchContext", "GET /api/context/excerpt": "ContextExcerpt", "GET /api/context/graph": "GraphNeighbors",
	"GET /api/code/map": "GetCodeMap", "GET /api/code/search": "SearchCode", "GET /api/code/outline": "GetCodeFileOutline", "GET /api/code/status": "GetCodeIndexStatus",
	"GET /api/execution-groups/{id}": "get_execution_group", "GET /api/products/{id}/executions": "list_executions", "GET /api/products/{id}/execution-events": "list_execution_events",
	"GET /api/attention": "list_attention", "GET /api/features/{id}/graph": "get_feature_graph", "GET /api/operations/{id}": "get_operation",
	"GET /api/intelligence-tasks/{id}": "get_intelligence_task", "GET /api/environments/{id}/state": "get_environment_state",
	"GET /api/integrations/{id}/context": "get_integration_context", "GET /api/integrations/{id}/git": "get_integration_git", "GET /api/products/{id}/unlinked-git-commits": "get_unlinked_git_commits",
	"GET /api/products/{id}/configuration": "get_product_configuration", "GET /api/products/{id}/retention": "get_artifact_retention", "GET /api/products/{id}/local-checkouts/{checkout}/git": "LocalGitState",
	"GET /api/repositories/{id}/branches": "list_branches", "GET /api/worker-jobs/{id}": "get_worker_job", "GET /api/worker-history": "WorkerHistoryJobs WorkerHistoryAttempts WorkerHistoryLinks",
	"GET /api/runtime-reports": "GetRuntimeReport", "GET /api/build-profiles": "ListBuildProfiles", "GET /api/builds": "ListBuilds", "GET /api/builds/{id}": "GetBuild",
	"GET /api/deployments": "ListDeployments", "GET /api/deployments/{id}": "GetDeployment", "GET /api/pipelines": "ListPipelines", "GET /api/signatures": "ListSignatures",
	"GET /api/prod-promotions": "ListProdPromotions", "GET /api/prod-promotions/{id}": "GetProdPromotion", "GET /api/public/prod-signatures/{digest}": "PublicProdSignatures",
}

var nonProductHTTPReads = map[string]string{
	"GET /api/statuses": "static lifecycle schema", "GET /api/schema": "static command schema",
	"GET /api/tools": "static tool catalog", "GET /api/code-sdk/rcp.ts": "generated tool declarations",
	"GET /api/agent-kit":                     "static repository setup guidance",
	"GET /api/connections/{id}/repositories": "instance-owned provider inventory; no Product bindings or history",
	"/api/worker/":                           "dispatch prefix; worker endpoints are registered separately",
}

var nonProductMCPReads = map[string]string{
	"get_status_schema": "static lifecycle schema", "get_code_api": "generated tool declarations",
	"get_tool_guide": "static tool catalog", "get_agent_kit": "static repository setup guidance",
	"get_agent_skill": "fixed skill text", "get_backup_capabilities": "static archive protocol",
}

const privateMCPCommands = `
execute
refresh_environment_runtime deploy_integration deploy_existing_artifact create_integration_branch protect_integration_revision
claim_execution heartbeat_execution report_execution release_execution takeover_execution set_desired_state
create_execution_group add_execution_group_member remove_execution_group_member set_execution_group_harness
set_product_stalled_timeout report_turn_end claim_execution_event_delivery ack_execution_event resolve_execution_event
request_candidate_dev_restoration confirm_candidate_dev_restoration verify_candidate_dev_restoration release_candidate_dev_after_restoration
capture_candidate_dev_baseline verify_candidate_dev_runtime deploy_candidate_dev reserve_candidate_dev release_candidate_dev
update_environment_feature assemble_environment claim_composition_conflict record_conflict_resolution verify_conflict_resolution
reconcile_composition prepare_release_candidate promote_release_candidate create_hotfix record_runtime_observation
create_test_scenario revise_test_scenario record_scenario_run`

var privateMCPDirectMutations = map[string]string{
	"queue_build": "QueueBuild", "cancel_build": "CancelBuild", "close_build": "CloseBuild",
	"deploy_build": "DeployBuild", "close_deployment": "CloseDeployment", "reconcile_deployment": "ReconcileDeployment",
	"sign_artifact": "SignArtifact", "request_prod_promotion": "RequestProdPromotion",
	"scan_workspace": "ScanWorkspace", "match_local_repositories": "MatchWorkspace",
	"set_project_knowledge": "SetProjectKnowledge", "upsert_product_knowledge": "UpsertKnowledgeNode",
	"upsert_code_index":   "UpsertCodeIndex",
	"plan_local_git_sync": "PlanLocalGitSync", "record_local_git_sync": "RecordLocalGitSync", "sync_pull_request_review": "SyncReview",
	"import_workspace_configuration": "TestPrivateProductOwnershipGrantsAndImport", "restore_backup": "TestPrivateRestoreEvidence",
}

var privateHTTPDirectMutations = map[string]string{
	"POST /api/builds": "QueueBuild", "POST /api/builds/{id}/cancel": "CancelBuild", "POST /api/builds/{id}/close": "CloseBuild",
	"POST /api/deployments": "DeployBuild", "POST /api/deployments/{id}/close": "CloseDeployment", "POST /api/deployments/{id}/reconcile": "ReconcileDeployment",
	"POST /api/signatures": "SignArtifact", "POST /api/prod-promotions": "RequestProdPromotion",
	"POST /api/prod-promotions/claim": "ClaimProdPromotion", "POST /api/prod-promotions/{id}/result": "RecordProdPromotionResult", "POST /api/prod-promotions/{id}/heartbeat": "HeartbeatProdPromotion",
	"POST /api/workspaces/scan": "ScanWorkspace", "POST /api/workspaces/match": "MatchWorkspace",
	"POST /api/products/{id}/knowledge": "SetProjectKnowledge", "POST /api/products/{id}/knowledge/nodes": "UpsertKnowledgeNode",
	"POST /api/code/index":     "UpsertCodeIndex",
	"POST /api/local-git/plan": "PlanLocalGitSync", "POST /api/local-git/record": "RecordLocalGitSync", "POST /api/reviews/sync": "SyncReview",
	"POST /api/runtime-reports": "ReportRuntime", "POST /api/workspaces/import": "TestPrivateProductOwnershipGrantsAndImport",
	"POST /api/backups/restore": "TestPrivateRestoreEvidence",
	"POST /api/worker/claim":    "ClaimWorker", "POST /api/worker/start": "StartWorker", "POST /api/worker/heartbeat": "HeartbeatWorker", "POST /api/worker/report": "ReportWorker",
}

const privateHTTPCommandPaths = `
/api/commands /api/compositions/{id}/reconcile /api/features/{id}/hotfix /api/features/{id}/update-environment
/api/integrations/{id}/deploy /api/operations/{id}/runtime /api/products/{id}/assemble-environment
/api/products/{id}/candidate-dev/capture-baseline /api/products/{id}/candidate-dev/confirm-restoration /api/products/{id}/candidate-dev/deploy
/api/products/{id}/candidate-dev/release /api/products/{id}/candidate-dev/release-after-restoration /api/products/{id}/candidate-dev/request-restoration
/api/products/{id}/candidate-dev/reserve /api/products/{id}/candidate-dev/verify-restoration /api/products/{id}/candidate-dev/verify-runtime
/api/products/{id}/conflicts/claim /api/products/{id}/conflicts/resolution /api/products/{id}/conflicts/verify
/api/release-candidates /api/release-candidates/{id}/promote /api/scenario-runs /api/test-scenarios /api/test-scenarios/revisions`

var nonProductHTTPMutations = map[string]string{
	"POST /api/backups/export":        "read-only, covered by CreateBackup",
	"POST /api/connections/{id}/test": "instance-owned provider connection, no Product data",
	"POST /api/tools/{name}":          "dispatch to the inventoried tool catalog",
	"/mcp":                            "dispatch to the inventoried MCP tools",
	"/api/worker/":                    "fallback for separately registered POST routes; no other mutations",
}

func privateMatrixEntries(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../application/private_product_leak_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]bool{}
	add := func(expr ast.Expr) {
		if literal, ok := expr.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			name, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			entries[name] = true
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.ValueSpec:
			if len(value.Names) == 1 && value.Names[0].Name == "privateProductReadMatrix" {
				for _, entry := range value.Values[0].(*ast.CompositeLit).Elts {
					add(entry.(*ast.KeyValueExpr).Key)
				}
			}
		case *ast.FuncDecl:
			if value.Name.Name != "checkPrivateAggregateReads" {
				return false
			}
			ast.Inspect(value.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.KeyValueExpr:
					add(value.Key)
				case *ast.CallExpr:
					if call, ok := value.Fun.(*ast.SelectorExpr); ok && call.Sel.Name == "Run" {
						add(value.Args[0])
					}
				}
				return true
			})
			return false
		}
		return true
	})
	return entries
}

func TestPrivateProductReadSurface(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenSQLite(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	routes := map[string]bool{}
	mutations := map[string]bool{}
	server := httptest.NewServer(newServer(application.New(store), Options{}, func(pattern string) {
		method, path, hasMethod := strings.Cut(pattern, " ")
		if !hasMethod {
			path = method
		}
		if strings.HasPrefix(path, "/api/") && (!hasMethod || method == "GET") {
			routes[pattern] = true
		}
		if hasMethod && method != "GET" || path == "/mcp" || path == "/api/worker/" {
			mutations[pattern] = true
		}
	}))
	defer server.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "private-product-inventory", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools := map[string]bool{}
	params := &mcp.ListToolsParams{}
	for {
		result, err := session.ListTools(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range result.Tools {
			tools[tool.Name] = true
		}
		if result.NextCursor == "" {
			break
		}
		params.Cursor = result.NextCursor
	}
	nonReaderMCP := maps.Clone(nonProductMCPReads)
	mcpWrites := maps.Clone(privateMCPDirectMutations)
	for _, name := range strings.Fields(privateMCPCommands) {
		if mcpWrites[name] != "" {
			t.Errorf("duplicate mutation classification: %s", name)
		}
		mcpWrites[name] = "Execute"
	}
	for name := range mcpWrites {
		if privateMCPReads[name] == "" {
			nonReaderMCP[name] = "membership-checked mutation"
		}
	}
	entries := privateMatrixEntries(t)
	for _, surface := range []struct {
		actual             map[string]bool
		covered, notReader map[string]string
	}{{tools, privateMCPReads, nonReaderMCP}, {routes, privateHTTPReads, nonProductHTTPReads}} {
		for name := range surface.actual {
			if (surface.covered[name] == "") == (surface.notReader[name] == "") {
				t.Errorf("%s requires exactly one private Product classification", name)
			}
		}
		for name, coverage := range surface.covered {
			if !surface.actual[name] {
				t.Errorf("classified reader no longer registered: %s", name)
			}
			for _, entry := range strings.Fields(coverage) {
				if !entries[entry] {
					t.Errorf("%s references missing leak matrix entry %s", name, entry)
				}
			}
		}
		for name := range surface.notReader {
			if !surface.actual[name] {
				t.Errorf("classified non-reader no longer registered: %s", name)
			}
		}
	}
	httpWrites := maps.Clone(privateHTTPDirectMutations)
	for _, path := range strings.Fields(privateHTTPCommandPaths) {
		pattern := "POST " + path
		if httpWrites[pattern] != "" {
			t.Errorf("duplicate mutation classification: %s", pattern)
		}
		httpWrites[pattern] = "Execute"
	}
	for route := range mutations {
		if (httpWrites[route] == "") == (nonProductHTTPMutations[route] == "") {
			t.Errorf("%s requires exactly one mutation classification", route)
		}
	}
	for route := range nonProductHTTPMutations {
		if !mutations[route] {
			t.Errorf("classified non-Product route no longer registered: %s", route)
		}
	}
	mutationEntries := privateMutationMatrixEntries(t)
	for _, surface := range []struct {
		actual  map[string]bool
		covered map[string]string
	}{{tools, mcpWrites}, {mutations, httpWrites}} {
		for name, entry := range surface.covered {
			if !surface.actual[name] || !mutationEntries[entry] {
				t.Errorf("%s is not registered or has no mutation regression %s", name, entry)
			}
		}
	}
}

func privateMutationMatrixEntries(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../application/private_product_review_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !strings.HasPrefix(fn.Name.Name, "TestPrivate") {
			continue
		}
		entries[fn.Name.Name] = true
		if fn.Name.Name != "TestPrivateDirectMutationSurface" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != 1 {
				return true
			}
			name, ok := assignment.Lhs[0].(*ast.Ident)
			if !ok || name.Name != "calls" {
				return true
			}
			for _, entry := range assignment.Rhs[0].(*ast.CompositeLit).Elts {
				value := entry.(*ast.KeyValueExpr).Key.(*ast.BasicLit).Value
				name, err := strconv.Unquote(value)
				if err != nil {
					t.Fatal(err)
				}
				entries[name] = true
			}
			return false
		})
	}
	return entries
}
