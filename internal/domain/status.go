package domain

import "slices"

// StatusCatalog returns fresh copies; caller configuration cannot extend it.
func StatusCatalog() map[string][]string {
	return map[string][]string{
		"composition_conflict": {"requires_resolution", "claimed", "verification_required", "resolved"},
		"feature":              {"planned", "active", "blocked", "completed", "archived"},
		"integration":          {"planned", "working", "implemented", "verifying", "ready", "released"},
		"blocker":              {"open", "resolved"}, "finding": {"open", "resolved"},
		"check_result": {"passed", "failed", "blocked", "skipped"}, "gate_result": {"pending", "passed", "failed", "blocked", "skipped", "stale"},
		"scenario_result": {"passed", "failed", "blocked"}, "composition": {"planned"}, "release": {"planned", "released"},
		"operation": {"PENDING", "RUNNING", "SUCCEEDED", "FAILED", "CANCELLED"}, "operation_step": {"PENDING", "RUNNING", "SUCCEEDED", "FAILED", "CANCELLED"},
		"deployment_state": {"PENDING", "COMPOSING", "SOURCE_READY", "UPDATING_MAIN", "WAITING_FOR_COMPONENTS", "WAITING_FOR_ARTIFACT", "BUILDING", "ARTIFACT_READY", "GITOPS_PENDING", "GITOPS_APPLIED", "PENDING_RECONCILIATION", "READY_FOR_VERIFICATION", "DEPLOYED", "SUCCEEDED", "FAILED", "BLOCKED", "CANCELLED"},
		"review_sync":      {"NO_FINDINGS_OBSERVED", "FINDINGS_OBSERVED"}, "git_observation": {"identical", "ahead", "behind", "diverged", "unknown"},
		"pull_request": {"open", "draft", "closed", "merged"}, "artifact_availability": {"PRESENT", "MISSING"},
	}
}
func ValidStatus(kind, value string) bool { return slices.Contains(StatusCatalog()[kind], value) }

// Command transitions. Release is intentionally absent: only verified execution
// may perform ready -> released. No generic workflow configuration is accepted.
func StatusTransitions() map[string]map[string][]string {
	return map[string]map[string][]string{
		"feature": {
			"planned": {"active", "blocked", "archived"}, "active": {"blocked", "completed", "archived"}, "blocked": {"active", "planned", "archived"}, "completed": {"active", "archived"}, "archived": {"active", "planned"},
		},
		"integration": {
			"planned": {"working"}, "working": {"planned", "implemented", "verifying", "ready"}, "implemented": {"planned", "working", "verifying", "ready"}, "verifying": {"planned", "working", "implemented", "ready"}, "ready": {"working", "verifying"}, "released": {},
		},
	}
}
func CanTransition(kind, from, to string) bool {
	if !ValidStatus(kind, from) || !ValidStatus(kind, to) {
		return false
	}
	if from == to {
		return true
	}
	return slices.Contains(StatusTransitions()[kind][from], to)
}
