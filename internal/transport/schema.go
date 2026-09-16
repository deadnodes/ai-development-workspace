package transport

import (
	"releasecontrol/internal/domain"
	"strings"
)

// CommandSchema is shared by HTTP discovery and MCP. The domain remains the
// authority for relationship and lifecycle validation.
func CommandSchema() map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	props := map[string]any{}
	for _, k := range strings.Fields("action actor id product_id feature_id integration_id gate_id check_id result_id") {
		props[k] = str()
	}
	props["actor"] = map[string]any{"type": "string", "minLength": 1, "description": "Attribution for this engineering action; a human or agent identifier."}
	data := map[string]any{}
	for _, k := range strings.Fields("name description title problem goal context owner objective rationale status body reason current commit deployment session environment_id mechanism instructions result observations logs severity url provider notes repository_id path cluster namespace branch base_commit head_commit private_key_ref owner api_url registry_credential_ref connection_id full_name role default_branch application_id workflow image_repository workflow_ref purpose ref image_field digest_field revision_id expected_digest team contact component_id external_system_id type base_branch finding_id gitops_commit artifact_digest details scenario_id scenario_version_id composition_id candidate_operation_id kind format registry_url package_name publication_target_id source_commit version checksum uri build_url") {
		data[k] = str()
	}
	for _, k := range strings.Fields("requirements constraints repositories dependencies acceptance_criteria working_areas remaining completed next warnings integration_ids gate_ids excluded_integration_ids interfaces contracts external_system_ids relationship_ids preconditions expected_outcomes finding_ids") {
		data[k] = map[string]any{"type": "array", "items": str()}
	}
	for _, k := range []string{"blocking", "blocks_release", "allow_deploy", "rebuild_missing", "healthy", "approve", "approve_main_update"} {
		data[k] = map[string]any{"type": "boolean"}
	}
	for _, k := range []string{"app_id", "installation_id"} {
		data[k] = map[string]any{"type": "integer"}
	}
	data["inputs"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
	data["position"] = map[string]any{"type": "integer"}
	data["steps"] = map[string]any{"type": "array", "items": str()}
	for _, k := range []string{"desired", "reconciled", "runtime", "composition"} {
		data[k] = map[string]any{"type": "object", "additionalProperties": true}
	}
	for k, fields := range map[string]string{"branches": "repository_id name", "commits": "repository_id sha", "artifacts": "kind url label", "pull_requests": "repository_id id url status"} {
		item := map[string]any{}
		for _, f := range strings.Fields(fields) {
			item[f] = str()
		}
		if k == "pull_requests" {
			item["status"] = map[string]any{"type": "string", "enum": domain.StatusCatalog()["pull_request"]}
		}
		data[k] = map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": item, "additionalProperties": false}}
	}
	// Existing integration edits use repository/SHA objects; immutable revision
	// capture uses an ordered list of commit SHAs. Domain validates per action.
	data["commits"] = map[string]any{"anyOf": []any{data["commits"], map[string]any{"type": "array", "items": str()}}}
	component := map[string]any{}
	for _, key := range []string{"application_id", "base_ref", "base_commit", "target_branch"} {
		component[key] = str()
	}
	component["revision_ids"] = map[string]any{"type": "array", "items": str()}
	compositionComponents := map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "properties": component, "additionalProperties": false,
		"required": []string{"application_id", "base_ref", "base_commit", "target_branch", "revision_ids"},
	}}
	scenarioComponent := map[string]any{}
	for _, key := range []string{"application_id", "operation_id", "source_sha", "artifact_digest", "deployment_evidence"} {
		scenarioComponent[key] = str()
	}
	data["components"] = map[string]any{"anyOf": []any{compositionComponents, map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": scenarioComponent, "additionalProperties": false, "required": []string{"application_id", "operation_id", "source_sha", "artifact_digest"}}}}}
	data["observations"] = map[string]any{"anyOf": []any{str(), map[string]any{"type": "array", "items": str()}}}
	data["role"] = map[string]any{"type": "string", "enum": domain.RepositoryRoles()}
	data["kind"] = map[string]any{"type": "string", "enum": domain.ComponentKinds()}
	data["format"] = map[string]any{"type": "string", "enum": domain.PublicationFormats()}
	props["data"] = map[string]any{"type": "object", "properties": data, "additionalProperties": false}
	actions := []struct{ names, refs, required string }{
		{"reconcile_composition", "id", ""},
		{"prepare_release_candidate", "product_id", "name environment_id components approve_main_update"},
		{"promote_release_candidate", "id", "approve"},
		{"create_hotfix", "feature_id", "title objective"},
		{"record_runtime_observation", "id", "gitops_commit artifact_digest healthy environment_id details"},
		{"create_test_scenario", "product_id", "title objective mechanism steps expected_outcomes"},
		{"revise_test_scenario", "product_id", "scenario_id title objective mechanism steps expected_outcomes"},
		{"record_scenario_run", "product_id", "scenario_version_id components result"},
		{"create_external_system", "", "name"}, {"update_external_system", "id", ""}, {"create_system_relationship", "product_id", "external_system_id type"}, {"set_external_scope", "", ""},
		{"grant_connection", "product_id", "connection_id"},
		{"create_github_connection", "", "name app_id installation_id private_key_ref owner"},
		{"import_repository", "product_id", "connection_id full_name role default_branch"},
		{"classify_repository", "id", "role"},
		{"configure_publication", "product_id", "application_id format registry_url package_name"},
		{"record_package_artifact", "product_id", "application_id publication_target_id source_commit version checksum uri"},
		{"configure_component", "product_id", "application_id connection_id workflow image_repository"},
		{"configure_environment", "product_id", "environment_id purpose connection_id repository_id ref path image_field application_id allow_deploy"},
		{"refresh_integration_git", "integration_id", ""},
		{"deploy_integration", "integration_id", "environment_id"},
		{"create_product", "", "name"}, {"create_feature", "product_id", "title problem goal"}, {"update_feature", "feature_id", ""},
		{"create_integration", "feature_id", "title objective"}, {"update_integration", "integration_id", ""},
		{"start_integration complete_integration", "integration_id", ""}, {"transition_integration", "integration_id", "status"},
		{"record_progress record_discovery add_blocker handoff", "feature_id", ""}, {"record_decision", "feature_id", "reason"},
		{"resolve_blocker resolve_finding", "id", "body"}, {"create_gate", "feature_id", "title reason integration_ids"},
		{"add_check", "gate_id", "title mechanism"}, {"record_check_result", "check_id", "result"},
		{"record_finding", "result_id", "title severity"}, {"create_environment", "product_id", "name"}, {"update_environment", "id", ""},
		{"create_application", "product_id", "name repository_id"},
		{"record_integration_revision", "integration_id", "repository_id branch base_commit head_commit commits"},
		{"plan_composition", "product_id", "environment_id name components"},
		{"select_composition", "id", ""},
		{"create_repository", "product_id", "name url"}, {"plan_release", "product_id", "name integration_ids"},
	}
	var variants []any
	for _, a := range actions {
		for _, name := range strings.Fields(a.names) {
			required := append([]string{"action", "actor", "data"}, strings.Fields(a.refs)...)
			branch := map[string]any{"properties": map[string]any{"action": map[string]any{"const": name}}, "required": required}
			if a.required != "" {
				branch["properties"].(map[string]any)["data"] = map[string]any{"required": strings.Fields(a.required)}
			}

			dataRule := map[string]any{}
			if a.required != "" {
				dataRule["required"] = strings.Fields(a.required)
			}
			switch name {
			case "create_feature":
				dataRule["properties"] = map[string]any{"status": map[string]any{"type": "string", "enum": []string{"planned", "active"}}}
			case "update_feature":
				dataRule["properties"] = map[string]any{"status": map[string]any{"type": "string", "enum": domain.StatusCatalog()["feature"]}}
			case "transition_integration":
				statuses := []string{}
				for _, status := range domain.StatusCatalog()["integration"] {
					if status != "released" {
						statuses = append(statuses, status)
					}
				}
				dataRule["properties"] = map[string]any{"status": map[string]any{"type": "string", "enum": statuses}}
			default:
				dataRule["properties"] = map[string]any{"status": false}
			}
			if name == "record_check_result" {
				dataRule["properties"].(map[string]any)["result"] = map[string]any{"type": "string", "enum": domain.StatusCatalog()["check_result"]}
			}
			if name == "record_scenario_run" {
				dataRule["properties"].(map[string]any)["result"] = map[string]any{"type": "string", "enum": domain.StatusCatalog()["scenario_result"]}
			}
			branch["properties"].(map[string]any)["data"] = dataRule
			variants = append(variants, branch)
		}
	}
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false, "oneOf": variants}
}
