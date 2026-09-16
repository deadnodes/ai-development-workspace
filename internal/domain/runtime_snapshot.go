package domain

import "releasecontrol/internal/delivery"

// RuntimeSnapshot retains a point-in-time read, independently of release execution.
// It never certifies or advances a deployment operation.
type RuntimeSnapshot struct {
	Meta
	OperationID         string                   `json:"operation_id"`
	EnvironmentID       string                   `json:"environment_id"`
	ApplicationID       string                   `json:"application_id"`
	Runtime             delivery.RuntimeSnapshot `json:"runtime"`
	Expected            RuntimeExpected          `json:"expected"`
	ReferenceComparison string                   `json:"reference_comparison"`
	Comparison          string                   `json:"comparison"`
	ArtifactMatches     []DeliveryArtifact       `json:"artifact_matches"`
	ActionsRuns         []delivery.ActionsRun    `json:"actions_runs"`
	ActionsError        string                   `json:"actions_error,omitempty"`
}
type RuntimeExpected struct {
	ImageRepository string `json:"image_repository"`
	Value           string `json:"value"`
	GitOpsCommit    string `json:"gitops_commit"`
	Error           string `json:"error,omitempty"`
}
