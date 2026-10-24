package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	{"request_candidate_dev_restoration", "POST /api/products/{id}/candidate-dev/request-restoration", "product_id", "product_id", "reservation_operation_id request_key expected_selection", "reservation_operation_id request_key expected_selection", "Request bounded restoration of every original baseline component; advances recovery fencing without renewing candidate authority. Inspect the asynchronous complete plan before a separate attributed confirmation."},
	{"confirm_candidate_dev_restoration", "POST /api/products/{id}/candidate-dev/confirm-restoration", "product_id", "product_id", "restoration_operation_id expected_plan_hash request_key expected_selection", "restoration_operation_id expected_plan_hash request_key expected_selection", "Record ATTRIBUTED_CONTROL_CONFIRMATION for the exact ready restoration plan. Actor and control token do not prove authenticated human text or voice approval. Never merges a Flux PR."},
	{"verify_candidate_dev_restoration", "POST /api/products/{id}/candidate-dev/verify-restoration", "product_id", "product_id", "restoration_operation_id request_key expected_selection", "restoration_operation_id request_key expected_selection", "Request fresh server-owned availability and complete original-image runtime proof after separately confirmed external human-only reviewed Flux merges. Does not unlock the hold."},
	{"release_candidate_dev_after_restoration", "POST /api/products/{id}/candidate-dev/release-after-restoration", "product_id", "product_id", "restoration_operation_id restoration_verification_operation_id request_key expected_selection", "restoration_operation_id restoration_verification_operation_id request_key expected_selection", "Explicit scoped unlock after exact restoration confirmation, latest fresh successful complete proof and final effect settlement. No TTL unlock or physical garbage collection."},

	{"capture_candidate_dev_baseline", "POST /api/products/{id}/candidate-dev/capture-baseline", "product_id", "product_id", "reservation_operation_id request_key expected_selection", "reservation_operation_id request_key expected_selection", "Capture the complete reserved shared Dev baseline with server-owned GitOps, runtime and scoped registry availability evidence before candidate effects. Existing-image receipts grant restoration evidence only, never build provenance, approval, unlock or Prod rights."},
	{"verify_candidate_dev_runtime", "POST /api/products/{id}/candidate-dev/verify-runtime", "product_id", "product_id", "activation_operation_id request_key expected_selection", "activation_operation_id request_key expected_selection", "Read configured complete runtime groups and exact Flux/GitOps/image identity for the same owner's activated Dev candidate. Server-owned point-in-time proof only; no functional acceptance, restoration, unlock, merge or production promotion."},
	{"deploy_candidate_dev", "POST /api/products/{id}/candidate-dev/deploy", "product_id", "product_id", "reservation_operation_id request_key expected_selection", "reservation_operation_id request_key expected_selection", "Prepare PR-only DEV/TEST delivery of exact existing main-candidate artifacts under the same owner's active reservation, fresh verified baseline and CAS. No rebuild, source write, PR merge, runtime acceptance or production promotion. Once admitted, blind reservation release requires a separate verified restoration protocol."},
	{"reserve_candidate_dev", "POST /api/products/{id}/candidate-dev/reserve", "product_id", "product_id", "candidate_operation_id environment_id request_key expected_selection", "candidate_operation_id environment_id request_key expected_selection", "Reserve the current DEV/TEST selection for preparation of an exact native main candidate. Requires CAS; blocks managed deployments without deploying the candidate or accepting Dev. Attribution is not authentication."},
	{"release_candidate_dev", "POST /api/products/{id}/candidate-dev/release", "product_id", "product_id", "reservation_operation_id request_key expected_selection", "reservation_operation_id request_key expected_selection", "Explicitly release an unchanged candidate Dev preparation reservation using owner attribution and exact CAS. Preserves baseline intent and observed state; does not restore a deployment or revive old operations. Restored or uncertain reservations remain fenced."},
	{"update_environment_feature", "POST /api/features/{id}/update-environment", "feature_id", "feature_id", "environment_id revision_ids name idempotency_key expected_selection", "environment_id revision_ids idempotency_key expected_selection", "Atomically patch explicit revisions of one feature into the existing pinned DEV/TEST composition, preserving other features and base. Requires the current desired selection CAS token and a durable idempotency key; queues the existing COMPOSE operation through PR-only targets."},
	{"assemble_environment", "POST /api/products/{id}/assemble-environment", "product_id", "product_id", "environment_id name integration_ids expected_selection", "environment_id", "Assemble current captured active integration revisions into disposable generated branches, build artifacts and apply the configured environment GitOps targets. Returns immediately with a persistent operation; conflicts require resolution."},
	{"claim_composition_conflict", "POST /api/products/{id}/conflicts/claim", "product_id", "product_id", "conflict_id", "conflict_id", "Claim responsibility for externally resolving a composition conflict."},
	{"record_conflict_resolution", "POST /api/products/{id}/conflicts/resolution", "product_id", "product_id", "conflict_id commit rationale", "conflict_id commit rationale", "Record the claim owner resolution commit; this does not deploy or imply verification."},
	{"verify_conflict_resolution", "POST /api/products/{id}/conflicts/verify", "product_id", "product_id", "conflict_id result_ids", "conflict_id result_ids", "Verify exact-commit passing evidence for each affected integration and all blocking checks."},
	{"reconcile_composition", "POST /api/compositions/{id}/reconcile", "composition_id", "id", "resolution_conflict_ids", "", "Execute the selected immutable composition in its configured environment. Queues Git/build/GitOps work; inspect parent and child operation evidence."},
	{"prepare_release_candidate", "POST /api/release-candidates", "product_id", "product_id", "name environment_id components approve_main_update main_sources idempotency_key", "name environment_id", "Build an exact already-merged main snapshot using main_sources and an idempotency_key with attributed RCP source verification; native candidates cannot promote. Legacy GitHub Actions input components/approve_main_update explicitly advances main."},
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
	if spec.action == "record_scenario_run" {
		properties["result"] = map[string]any{"type": "string", "enum": domain.StatusCatalog()["scenario_result"]}
	}
	if restorationAction(spec.action) || spec.action == "reserve_candidate_dev" || spec.action == "release_candidate_dev" || spec.action == "deploy_candidate_dev" || spec.action == "verify_candidate_dev_runtime" || spec.action == "capture_candidate_dev_baseline" {
		properties["request_key"] = map[string]any{"type": "string", "minLength": 16, "maxLength": 128}
	}
	required := append([]string{spec.refName}, strings.Fields(spec.required)...)
	if restorationAction(spec.action) {
		required = append(required, "actor")
		properties["actor"] = map[string]any{"type": "string", "minLength": 1, "pattern": `\S`, "description": "Required attribution; not authenticated human approval."}
	}
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
	if !restorationAction(spec.action) {
		actor = defaultActor(actor)
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
			var raw json.RawMessage
			var input map[string]any
			err := decoder.Decode(&raw)
			if err == nil {
				input, err = decodeFlowInput(raw)
			}
			if err != nil || input == nil {
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
			command := flowCommand(spec, input)
			if err := validateRestorationTransportCommand(command); err != nil {
				write(w, 400, map[string]string{"error": err.Error()})
				return
			}
			v, err := service.Execute(r.Context(), command)
			respond(w, v, err)
		})
	}
}
func registerFlowTools(server *mcp.Server, service Service) {
	for _, spec := range flowActions {
		mcp.AddTool(server, &mcp.Tool{Name: spec.action, Description: spec.description, InputSchema: flowSchema(spec)}, func(ctx context.Context, request *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
			var err error
			if restorationAction(spec.action) {
				if err = uniqueMainCandidateJSON(request.Params.Arguments); err != nil {
					return nil, nil, err
				}
			}
			input, err = decodeFlowInput(request.Params.Arguments)
			if err != nil {
				return nil, nil, err
			}
			command := flowCommand(spec, input)
			if err := validateRestorationTransportCommand(command); err != nil {
				return nil, nil, err
			}
			v, e := service.Execute(ctx, command)
			return nil, v, e
		})
	}
}

// Preserve only the CAS integer; existing flow field decoding stays unchanged.
func decodeFlowInput(raw []byte) (map[string]any, error) {
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	_, native := input["main_sources"]
	_, reservation := input["reservation_operation_id"]
	_, requestKey := input["request_key"]
	if native || reservation || requestKey {
		if err := uniqueMainCandidateJSON(raw); err != nil {
			return nil, err
		}
	}
	var envelope struct {
		Expected json.RawMessage `json:"expected_selection"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Expected != nil {
		var exact any
		decoder := json.NewDecoder(bytes.NewReader(envelope.Expected))
		decoder.UseNumber()
		if err := decoder.Decode(&exact); err != nil {
			return nil, err
		}
		input["expected_selection"] = exact
	}
	return input, nil
}

// Restoration has no legacy attribution default. This checks only the wire
// shape; the shared application owns ownership, authority and lifecycle policy.
func restorationAction(action string) bool {
	switch action {
	case "request_candidate_dev_restoration", "confirm_candidate_dev_restoration", "verify_candidate_dev_restoration", "release_candidate_dev_after_restoration":
		return true
	}
	return false
}
func validateRestorationTransportCommand(c domain.Command) error {
	if !restorationAction(c.Action) {
		return nil
	}
	if strings.TrimSpace(c.Actor) == "" || strings.TrimSpace(c.ProductID) == "" {
		return fmt.Errorf("restoration requires explicit actor and product_id")
	}
	if c.ID != "" || c.FeatureID != "" || c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return fmt.Errorf("restoration accepts only Product scope")
	}
	var spec flowAction
	for _, candidate := range flowActions {
		if candidate.action == c.Action {
			spec = candidate
			break
		}
	}
	allowed := map[string]bool{}
	for _, field := range strings.Fields(spec.required) {
		allowed[field] = true
	}
	for field := range c.Data {
		if !allowed[field] {
			return fmt.Errorf("unknown restoration field: %s", field)
		}
	}
	for field := range allowed {
		if field == "expected_selection" {
			continue
		}
		value, ok := c.Data[field].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("required restoration field: %s", field)
		}
	}
	key, _ := c.Data["request_key"].(string)
	if len(key) < 16 || len(key) > 128 {
		return fmt.Errorf("request_key must be 16–128 bytes")
	}
	raw, err := json.Marshal(c.Data["expected_selection"])
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 {
		return fmt.Errorf("exact expected_selection required")
	}
	for _, field := range []string{"composition_id", "operation_id", "generation"} {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("exact expected_selection required")
		}
		if bytes.Equal(bytes.TrimSpace(fields[field]), []byte("null")) {
			return fmt.Errorf("invalid null expected_selection field: %s", field)
		}
	}
	var selection domain.EnvironmentSelection
	if err = json.Unmarshal(raw, &selection); err != nil {
		return fmt.Errorf("invalid expected_selection: %w", err)
	}
	return nil
}
