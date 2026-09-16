package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/transport"
)

func TestExecutionFlowHTTPMCPParity(t *testing.T) {
	service := &boundaryService{commands: make(chan domain.Command, 16), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "flow-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	component := map[string]any{"application_id": "app", "base_ref": "main", "base_commit": strings.Repeat("a", 40), "target_branch": "generated/release", "revision_ids": []string{"revision"}}
	definition := map[string]any{"title": "Payment flow", "objective": "Exactly one charge", "mechanism": "browser", "steps": []string{"Pay twice"}, "expected_outcomes": []string{"One charge"}, "integration_ids": []string{"integration"}, "blocking": true}
	revision := map[string]any{}
	for k, v := range definition {
		revision[k] = v
	}
	revision["scenario_id"] = "scenario"
	for _, tc := range []struct {
		tool, path, refName, ref string
		data                     map[string]any
	}{
		{"reconcile_composition", "/api/compositions/composition/reconcile", "composition_id", "composition", map[string]any{}},
		{"prepare_release_candidate", "/api/release-candidates", "product_id", "product", map[string]any{"name": "Release", "environment_id": "prod", "components": []any{component}, "approve_main_update": true}},
		{"promote_release_candidate", "/api/release-candidates/candidate/promote", "candidate_operation_id", "candidate", map[string]any{"approve": true}},
		{"create_hotfix", "/api/features/feature/hotfix", "feature_id", "feature", map[string]any{"title": "Fix double charge", "objective": "Keep callbacks idempotent", "finding_id": "finding", "repositories": []string{"repo"}, "remaining": []string{"Regression check"}}},
		{"record_runtime_observation", "/api/operations/child/runtime", "operation_id", "child", map[string]any{"environment_id": "dev", "gitops_commit": strings.Repeat("b", 40), "artifact_digest": "sha256:" + strings.Repeat("d", 64), "healthy": false, "details": "Observed one unavailable replica"}},
		{"create_test_scenario", "/api/test-scenarios", "product_id", "product", definition},
		{"revise_test_scenario", "/api/test-scenarios/revisions", "product_id", "product", revision},
		{"record_scenario_run", "/api/scenario-runs", "product_id", "product", map[string]any{"scenario_version_id": "version", "candidate_operation_id": "candidate", "components": []any{map[string]any{"application_id": "app", "operation_id": "child", "source_sha": strings.Repeat("a", 40), "artifact_digest": "sha256:" + strings.Repeat("d", 64)}}, "result": "passed", "observations": []string{"Exactly one charge"}}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			input := map[string]any{"actor": "human/flow", tc.refName: tc.ref}
			for k, v := range tc.data {
				input[k] = v
			}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.Post(server.URL+tc.path, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("HTTP %d", response.StatusCode)
			}
			viaHTTP := <-service.commands
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: input})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("MCP rejected valid %s: %+v", tc.tool, result)
			}
			viaMCP := <-service.commands
			if !reflect.DeepEqual(viaHTTP, viaMCP) {
				t.Fatalf("different shared domain commands HTTP=%+v MCP=%+v", viaHTTP, viaMCP)
			}
			if viaHTTP.Action != tc.tool || viaHTTP.Actor != "human/flow" {
				t.Fatal("command attribution/action lost")
			}
			if tc.tool == "record_runtime_observation" && viaHTTP.Data["healthy"] != false {
				t.Fatal("unhealthy observation altered")
			}
			if _, ok := viaHTTP.Data[tc.refName]; ok {
				t.Fatalf("envelope reference copied into data: %s", tc.refName)
			}
		})
	}
}
func TestFlowRoutesRejectContradictoryReferencesAndUnknownFields(t *testing.T) {
	service := &boundaryService{commands: make(chan domain.Command, 2), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	for _, body := range []string{`{"actor":"human","composition_id":"other"}`, `{"actor":"human","force":true}`, `{"actor":"human"} {}`, `null`} {
		response, err := http.Post(server.URL+"/api/compositions/exact/reconcile", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("body %s got %d", body, response.StatusCode)
		}
	}
	if len(service.commands) != 0 {
		t.Fatal("invalid flow request reached shared application")
	}
}
