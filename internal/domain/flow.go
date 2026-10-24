package domain

import (
	"releasecontrol/internal/delivery"
	"time"
)

type SourceRevisionRetention struct {
	RevisionID string `json:"revision_id"`
	Branch     string `json:"branch"`
	HeadCommit string `json:"head_commit"`
}
type SourceRetentionIntent struct {
	ProductID         string                    `json:"product_id"`
	OperationID       string                    `json:"operation_id"`
	EnvironmentID     string                    `json:"environment_id"`
	RepositoryID      string                    `json:"repository_id"`
	OriginOperationID string                    `json:"origin_operation_id"`
	InputHash         string                    `json:"input_hash"`
	Revisions         []SourceRevisionRetention `json:"revisions"`
}
type SourceRetentionObservation struct {
	IntentHash string    `json:"intent_hash"`
	RevisionID string    `json:"revision_id"`
	Branch     string    `json:"branch"`
	HeadCommit string    `json:"head_commit"`
	ObservedAt time.Time `json:"observed_at"`
}
type FlowSource struct {
	Retention *SourceRetentionIntent       `json:"retention,omitempty"`
	Retained  []SourceRetentionObservation `json:"retained,omitempty"`

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
