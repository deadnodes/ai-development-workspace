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

// These tools submit mutations or enqueue operations, rather than exposing a
// read endpoint. Approval, replay, and hidden references use command policy.
const privateMCPCommands = `
execute restore_backup import_workspace_configuration scan_workspace match_local_repositories
set_project_knowledge upsert_product_knowledge plan_local_git_sync record_local_git_sync
refresh_environment_runtime deploy_integration deploy_existing_artifact create_integration_branch protect_integration_revision
claim_execution heartbeat_execution report_execution release_execution takeover_execution set_desired_state
create_execution_group add_execution_group_member remove_execution_group_member set_execution_group_harness
set_product_stalled_timeout report_turn_end claim_execution_event_delivery ack_execution_event resolve_execution_event
queue_build cancel_build close_build deploy_build close_deployment reconcile_deployment sign_artifact request_prod_promotion
request_candidate_dev_restoration confirm_candidate_dev_restoration verify_candidate_dev_restoration release_candidate_dev_after_restoration
capture_candidate_dev_baseline verify_candidate_dev_runtime deploy_candidate_dev reserve_candidate_dev release_candidate_dev
update_environment_feature assemble_environment claim_composition_conflict record_conflict_resolution verify_conflict_resolution
reconcile_composition prepare_release_candidate promote_release_candidate create_hotfix record_runtime_observation
create_test_scenario revise_test_scenario record_scenario_run`

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
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	routes := map[string]bool{}
	server := httptest.NewServer(newServer(application.New(store), Options{}, func(pattern string) {
		method, path, hasMethod := strings.Cut(pattern, " ")
		if !hasMethod {
			path = method
		}
		if strings.HasPrefix(path, "/api/") && (!hasMethod || method == "GET") {
			routes[pattern] = true
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
	for _, name := range strings.Fields(privateMCPCommands) {
		nonReaderMCP[name] = "mutation or operation submission, not a Product read endpoint"
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
}
