package domain

import "releasecontrol/internal/delivery"

type FlowSource struct {
	RepositoryID string                 `json:"repository_id"`
	Connection   delivery.Connection    `json:"connection"`
	Plan         delivery.SourcePlan    `json:"plan"`
	Result       *delivery.SourceResult `json:"result,omitempty"`
	MainAdvanced bool                   `json:"main_advanced"`
}
type RuntimeObservation struct {
	Meta
	OperationID    string `json:"operation_id"`
	EnvironmentID  string `json:"environment_id"`
	GitOpsCommit   string `json:"gitops_commit"`
	ArtifactDigest string `json:"artifact_digest"`
	Healthy        bool   `json:"healthy"`
	Details        string `json:"details"`
}
