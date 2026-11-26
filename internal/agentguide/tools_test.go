package agentguide

import "testing"

func TestRegistryActionsExist(t *testing.T) {
	if err := ValidateActionCoverage(); err != nil {
		t.Fatal(err)
	}
}

func TestPendingDocumentationOnlyShrinks(t *testing.T) {
	if got := len(PendingDocumentation()); got > 6 {
		t.Fatalf("pendingDocumentation grew: got %d; updated bound is 6", got)
	}
}

func TestToolGuideResolvesAliases(t *testing.T) {
	for _, name := range []string{
		"execute_action:deploy_integration",
		"mcp_tool:deploy_integration",
		"http_route:POST /api/integrations/{id}/deploy",
		"mcp_tool:create_hotfix",
		"http_route:POST /api/test-scenarios",
		"http_route:POST /api/products/{id}/candidate-dev/confirm-restoration",
		"http_route:GET /api/operations/{id}",
		"mcp_tool:request_candidate_dev_restoration",
		"http_route:POST /api/products/{id}/candidate-dev/request-restoration",
		"http_route:POST /api/products/{id}/candidate-dev/reserve",
		"http_route:POST /api/test-scenarios/revisions",
		"http_route:POST /api/release-candidates",
		"http_route:POST /api/release-candidates/{id}/promote",
		"http_route:POST /api/prod-promotions/{id}/heartbeat",
		"http_route:POST /api/reviews/sync",
		"http_route:POST /api/runtime-reports",
		"http_route:POST /api/scenario-runs",
		"http_route:POST /api/signatures",
		"http_route:POST /api/tools/{name}",
		"http_route:POST /api/workspaces/import",
		"http_route:POST /api/workspaces/match",
		"http_route:POST /api/workspaces/scan",
		"http_route:GET /api/builds",
		"http_route:GET /api/agent-kit",
		"http_route:GET /api/attention",
		"http_route:/mcp",
	} {
		if _, ok := ToolGuide(name).(ToolEntry); !ok {
			t.Fatalf("ToolGuide(%q) did not resolve to an entry", name)
		}
	}
}
