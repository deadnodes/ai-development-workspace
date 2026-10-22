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
		{"verify_candidate_dev_runtime", "/api/products/product/candidate-dev/verify-runtime", "product_id", "product", map[string]any{"activation_operation_id": "activation", "request_key": "candidate-dev-runtime-1", "expected_selection": map[string]any{"composition_id": "baseline", "operation_id": "old", "generation": json.Number("9007199254740993")}}},
		{"deploy_candidate_dev", "/api/products/product/candidate-dev/deploy", "product_id", "product", map[string]any{"reservation_operation_id": "reservation", "request_key": "candidate-dev-deploy-1", "expected_selection": map[string]any{"composition_id": "baseline", "operation_id": "old", "generation": json.Number("9007199254740993")}}},
		{"reserve_candidate_dev", "/api/products/product/candidate-dev/reserve", "product_id", "product", map[string]any{"candidate_operation_id": "candidate", "environment_id": "dev", "request_key": "reservation-request-1", "expected_selection": map[string]any{"composition_id": "baseline", "operation_id": "old", "generation": json.Number("9007199254740993")}}},
		{"release_candidate_dev", "/api/products/product/candidate-dev/release", "product_id", "product", map[string]any{"reservation_operation_id": "reservation", "request_key": "reservation-release-1", "expected_selection": map[string]any{"composition_id": "baseline", "operation_id": "old", "generation": json.Number("9007199254740994")}}},
		{"update_environment_feature", "/api/features/feature/update-environment", "feature_id", "feature", map[string]any{"environment_id": "dev", "revision_ids": []string{"A2"}, "idempotency_key": "key", "expected_selection": map[string]any{"composition_id": "baseline", "operation_id": "old", "generation": json.Number("9007199254740993")}}},
		{"reconcile_composition", "/api/compositions/composition/reconcile", "composition_id", "composition", map[string]any{}},
		{"prepare_release_candidate", "/api/release-candidates", "product_id", "product", map[string]any{"name": "Release", "environment_id": "prod", "components": []any{component}, "approve_main_update": true}},
		{"prepare_release_candidate", "/api/release-candidates", "product_id", "product", map[string]any{"name": "Main candidate", "environment_id": "prod", "idempotency_key": "main-key", "main_sources": []any{map[string]any{"application_id": "app", "main_sha": strings.Repeat("c", 40), "native": map[string]any{"build_recipe_id": "build", "publication_recipe_id": "publish", "parameters": map[string]any{}}, "revisions": []any{map[string]any{"revision_id": "revision", "pull_request_number": 17, "verification_result_ids": []string{"result"}}}}}}},
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
			if tc.tool == "deploy_candidate_dev" || tc.tool == "verify_candidate_dev_runtime" {
				generic, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: viaHTTP})
				if err != nil || generic.IsError {
					t.Fatalf("generic execute rejected candidate Dev request: %v %+v", err, generic)
				}
				viaExecute := <-service.commands
				if !reflect.DeepEqual(viaHTTP, viaExecute) {
					t.Fatalf("generic execute differs from named delivery: %+v != %+v", viaExecute, viaHTTP)
				}
			}
			if viaHTTP.Action != tc.tool || viaHTTP.Actor != "human/flow" {
				t.Fatal("command attribution/action lost")
			}
			if tc.tool == "update_environment_feature" {
				raw, _ := json.Marshal(viaMCP.Data["expected_selection"])
				var token domain.EnvironmentSelection
				if err := json.Unmarshal(raw, &token); err != nil || token.Generation != 9007199254740993 || viaMCP.FeatureID != "feature" {
					t.Fatalf("exact patch CAS/scope lost: %s", raw)
				}
			}
			if tc.tool == "reserve_candidate_dev" || tc.tool == "release_candidate_dev" || tc.tool == "deploy_candidate_dev" || tc.tool == "verify_candidate_dev_runtime" {
				raw, _ := json.Marshal(viaMCP.Data["expected_selection"])
				var token domain.EnvironmentSelection
				want := uint64(9007199254740993)
				if tc.tool == "release_candidate_dev" {
					want++
				}
				if err := json.Unmarshal(raw, &token); err != nil || token.Generation != want || viaMCP.ProductID != "product" {
					t.Fatalf("exact reservation CAS/scope lost: %s", raw)
				}
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

func TestMainCandidateDuplicateJSONRejected(t *testing.T) {
	service := &boundaryService{commands: make(chan domain.Command, 1), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	for _, raw := range []string{
		`{"product_id":"p","name":"candidate","environment_id":"prod","idempotency_key":"key","main_sources":[],"main_sources":[]}`,
		`{"product_id":"p","name":"candidate","environment_id":"prod","idempotency_key":"key","main_sources":[{"application_id":"a","application_id":"b"}]}`,
	} {
		response, err := http.Post(server.URL+"/api/release-candidates", "application/json", strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("duplicate candidate input accepted HTTP%d", response.StatusCode)
		}
	}
	if len(service.commands) != 0 {
		t.Fatal("ambiguous input reached command layer")
	}
}

func TestCandidateDevReservationRejectsAmbiguousTransportInput(t *testing.T) {
	service := &boundaryService{commands: make(chan domain.Command, 8), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	for _, tc := range []struct{ path, body string }{
		{"verify-runtime", `{"product_id":"other","request_key":"candidate-runtime-1"}`},
		{"verify-runtime", `{"activation_operation_id":"a","activation_operation_id":"b","request_key":"candidate-runtime-1"}`},
		{"verify-runtime", `{"Activation_operation_id":"a","request_key":"candidate-runtime-1"}`},
		{"verify-runtime", `{"activation_operation_id":"a","request_key":"candidate-runtime-1","healthy":true}`},
		{"verify-runtime", `{"activation_operation_id":"a","request_key":"candidate-runtime-1","targets":[]}`},
		{"verify-runtime", `{"activation_operation_id":"a","request_key":"candidate-runtime-1","expected_selection":{"composition_id":"c","operation_id":"o","generation":1,"generation":2}}`},
		{"deploy", `{"product_id":"other","request_key":"candidate-dev-deploy-1"}`},
		{"deploy", `{"reservation_operation_id":"a","reservation_operation_id":"b","request_key":"candidate-dev-deploy-1"}`},
		{"deploy", `{"request_key":"candidate-dev-deploy-1","expected_selection":{"composition_id":"a","operation_id":"b","generation":1,"generation":2}}`},
		{"deploy", `{"reservation_operation_id":"a","request_key":"candidate-dev-deploy-1","artifact_id":"injected"}`},
		{"deploy", `{"reservation_operation_id":"a","request_key":"candidate-dev-deploy-1","approve":true}`},
		{"deploy", `{"Reservation_operation_id":"a","request_key":"candidate-dev-deploy-1"}`},
		{"reserve", `{"product_id":"other","request_key":"reservation-request-1"}`},
		{"release", `{"product_id":"other","request_key":"reservation-release-1"}`},
		{"reserve", `{"request_key":"reservation-request-1","request_key":"reservation-request-2"}`},
		{"reserve", `{"request_key":"reservation-request-1","expected_selection":{"composition_id":"a","operation_id":"b","generation":1,"generation":2}}`},
		{"release", `{"reservation_operation_id":"a","reservation_operation_id":"b","request_key":"reservation-release-1"}`},
		{"reserve", `{"request_key":"reservation-request-1","reservation_epoch":99}`},
		{"release", `{"request_key":"reservation-release-1","approve":true}`},
	} {
		response, err := http.Post(server.URL+"/api/products/product/candidate-dev/"+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("ambiguous reservation input accepted: %s HTTP%d", tc.body, response.StatusCode)
		}
	}
	if len(service.commands) != 0 {
		t.Fatal("ambiguous reservation transport input reached command layer")
	}
}
