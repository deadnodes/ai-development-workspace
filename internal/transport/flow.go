package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
)

// Flow transports only translate named arguments to the shared command layer.
// Approval, exact code/evidence identity and lifecycle policy remain application rules.
type flowAction struct{ action, pattern, refName, refField, fields, required, description string }

var flowActions = []flowAction{
	{"reconcile_composition", "POST /api/compositions/{id}/reconcile", "composition_id", "id", "", "", "Execute the selected immutable composition in its configured environment. Queues Git/build/GitOps work; inspect parent and child operation evidence."},
	{"prepare_release_candidate", "POST /api/release-candidates", "product_id", "product_id", "name environment_id components approve_main_update", "name environment_id components approve_main_update", "Explicitly approve advancing configured main branches to the selected candidate and building its exact artifacts. This is a real Git mutation. Verification is required before promotion."},
	{"promote_release_candidate", "POST /api/release-candidates/{id}/promote", "candidate_operation_id", "id", "approve", "approve", "Promote this exact candidate and its verified artifact digests to the configured environment. Requires explicit approval; the application rejects missing/stale scenarios and blocking findings."},
	{"create_hotfix", "POST /api/features/{id}/hotfix", "feature_id", "feature_id", "title objective finding_id repositories remaining", "title objective", "Create a small hotfix integration linked to its feature and optional finding. Does not silently deploy or bypass verification."},
	{"record_runtime_observation", "POST /api/operations/{id}/runtime", "operation_id", "id", "gitops_commit artifact_digest healthy environment_id details", "gitops_commit artifact_digest healthy environment_id details", "Record an attributed runtime observation against exact child GitOps commit, image digest and environment. This is caller-supplied evidence, not server-collected Flux telemetry."},
	{"create_test_scenario", "POST /api/test-scenarios", "product_id", "product_id", "title objective mechanism preconditions steps expected_outcomes integration_ids blocking", "title objective mechanism steps expected_outcomes", "Create an immutable scenario version describing the checks and scope required for verification."},
	{"revise_test_scenario", "POST /api/test-scenarios/revisions", "product_id", "product_id", "scenario_id title objective mechanism preconditions steps expected_outcomes integration_ids blocking", "scenario_id title objective mechanism steps expected_outcomes", "Append a new complete scenario version without rewriting prior definitions or execution evidence."},
	{"record_scenario_run", "POST /api/scenario-runs", "product_id", "product_id", "scenario_version_id composition_id candidate_operation_id environment_id components result observations artifacts finding_ids", "scenario_version_id components result", "Record an actual scenario execution against one exact composition or release candidate, covering all component outputs. Failed evidence and linked findings remain historical."},
}

func flowSchema(spec flowAction) map[string]any {
	command := CommandSchema()
	root := command["properties"].(map[string]any)
	data := root["data"].(map[string]any)["properties"].(map[string]any)
	properties := map[string]any{"actor": root["actor"], spec.refName: map[string]any{"type": "string", "minLength": 1}}
	for _, key := range strings.Fields(spec.fields) {
		properties[key] = data[key]
	}
	required := append([]string{"actor", spec.refName}, strings.Fields(spec.required)...)
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func flowCommand(spec flowAction, input map[string]any) domain.Command {
	actor, _ := input["actor"].(string)
	reference, _ := input[spec.refName].(string)
	data := map[string]any{}
	for _, key := range strings.Fields(spec.fields) {
		if value, ok := input[key]; ok {
			data[key] = value
		}
	}
	c := domain.Command{Action: spec.action, Actor: actor, Data: data}
	switch spec.refField {
	case "id":
		c.ID = reference
	case "product_id":
		c.ProductID = reference
	case "feature_id":
		c.FeatureID = reference
	}
	return c
}
func registerFlowRoutes(mux *http.ServeMux, service Service) {
	for _, spec := range flowActions {
		mux.HandleFunc(spec.pattern, func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				write(w, 415, map[string]string{"error": "Use Content-Type: application/json"})
				return
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
			var input map[string]any
			if err := decoder.Decode(&input); err != nil || input == nil {
				write(w, 400, map[string]string{"error": "Expected one JSON object"})
				return
			}
			if decoder.Decode(new(any)) != io.EOF {
				write(w, 400, map[string]string{"error": "Expected one JSON object"})
				return
			}
			allowed := flowSchema(spec)["properties"].(map[string]any)
			for key := range input {
				if _, ok := allowed[key]; !ok {
					write(w, 400, map[string]string{"error": "Unknown field: " + key})
					return
				}
			}
			if id := r.PathValue("id"); id != "" {
				if value, ok := input[spec.refName]; ok && value != id {
					write(w, 400, map[string]string{"error": "Path and body reference differ"})
					return
				}
				input[spec.refName] = id
			}
			v, err := service.Execute(r.Context(), flowCommand(spec, input))
			respond(w, v, err)
		})
	}
}
func registerFlowTools(server *mcp.Server, service Service) {
	for _, spec := range flowActions {
		mcp.AddTool(server, &mcp.Tool{Name: spec.action, Description: spec.description, InputSchema: flowSchema(spec)}, func(ctx context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
			v, e := service.Execute(ctx, flowCommand(spec, input))
			return nil, v, e
		})
	}
}
