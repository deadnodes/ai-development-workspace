package transport

import "strings"

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
	for _, k := range strings.Fields("name description title problem goal context owner objective rationale status body reason current commit deployment session environment_id mechanism instructions result observations logs severity url provider notes repository_id path cluster namespace branch base_commit head_commit") {
		data[k] = str()
	}
	for _, k := range strings.Fields("requirements constraints repositories dependencies acceptance_criteria working_areas remaining completed next warnings integration_ids gate_ids excluded_integration_ids") {
		data[k] = map[string]any{"type": "array", "items": str()}
	}
	for _, k := range []string{"blocking", "blocks_release"} {
		data[k] = map[string]any{"type": "boolean"}
	}
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
	data["components"] = map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "properties": component, "additionalProperties": false,
		"required": []string{"application_id", "base_ref", "base_commit", "target_branch", "revision_ids"},
	}}
	props["data"] = map[string]any{"type": "object", "properties": data, "additionalProperties": false}
	actions := []struct{ names, refs, required string }{
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
			variants = append(variants, branch)
		}
	}
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false, "oneOf": variants}
}
