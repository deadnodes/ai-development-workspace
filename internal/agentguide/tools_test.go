package agentguide

import "testing"

func TestRegistryActionsExist(t *testing.T) {
	if err := ValidateActionCoverage(); err != nil {
		t.Fatal(err)
	}
}

func TestPendingDocumentationOnlyShrinks(t *testing.T) {
	if got := len(PendingDocumentation()); got > 35 {
		t.Fatalf("pendingDocumentation grew: got %d; updated bound is 35", got)
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
