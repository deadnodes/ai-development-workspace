package domain

import (
	"releasecontrol/internal/delivery"
	"time"
)

type DevBaselineRequest struct {
	ReservationOperationID string                `json:"reservation_operation_id"`
	RequestKey             string                `json:"request_key"`
	ExpectedSelection      *EnvironmentSelection `json:"expected_selection"`
}
type DevBaselineProof struct {
	Request                DevBaselineRequest     `json:"request"`
	ReservationOperationID string                 `json:"reservation_operation_id"`
	ReservationHash        string                 `json:"reservation_hash"`
	CompositionID          string                 `json:"composition_id"`
	CompositionHash        string                 `json:"composition_hash"`
	Generation             uint64                 `json:"generation"`
	Components             []DevBaselineComponent `json:"components"`
	SnapshotHash           string                 `json:"snapshot_hash"`
}
type DevBaselineComponent struct {
	ApplicationID       string                              `json:"application_id"`
	RepositoryID        string                              `json:"repository_id"`
	ConnectionID        string                              `json:"connection_id"`
	ConfigHash          string                              `json:"config_hash"`
	AuthorityHash       string                              `json:"authority_hash"`
	ImageRepository     string                              `json:"image_repository"`
	Digest              string                              `json:"digest,omitempty"`
	GitOps              delivery.GitOpsRequest              `json:"gitops"`
	Scopes              []delivery.RuntimeVerificationScope `json:"scopes"`
	ObservedGitOps      *DevRuntimeGitOpsObservation        `json:"observed_gitops,omitempty"`
	Revision            *delivery.GitOpsRevisionProof       `json:"revision,omitempty"`
	ArtifactID          string                              `json:"artifact_id,omitempty"`
	ArtifactHash        string                              `json:"artifact_hash,omitempty"`
	RegistryJobID       string                              `json:"registry_job_id,omitempty"`
	RegistryInputHash   string                              `json:"registry_input_hash,omitempty"`
	RegistryRootSetHash string                              `json:"registry_root_set_hash,omitempty"`
}
type DevBaselineResult struct {
	ExpectedHash string                       `json:"expected_hash"`
	Outcome      string                       `json:"outcome"`
	Reason       string                       `json:"reason"`
	ObservedAt   time.Time                    `json:"observed_at"`
	Runtime      *DevRuntimeResult            `json:"runtime,omitempty"`
	Components   []DevBaselineComponentResult `json:"components,omitempty"`
	Hash         string                       `json:"hash"`
}
type DevBaselineComponentResult struct {
	ApplicationID   string                      `json:"application_id"`
	ImageRepository string                      `json:"image_repository"`
	Digest          string                      `json:"digest"`
	GitOps          DevRuntimeGitOpsObservation `json:"gitops"`
	ArtifactID      string                      `json:"artifact_id,omitempty"`
	ArtifactHash    string                      `json:"artifact_hash,omitempty"`
	Availability    *delivery.ArtifactResult    `json:"availability,omitempty"`
	Registry        *DevBaselineRegistryReceipt `json:"registry,omitempty"`
}
type DevBaselineRegistryReceipt struct {
	ClosureKind             string    `json:"closure_kind"`
	ContentProofHash        string    `json:"content_proof_hash"`
	MetadataRootClosure     string    `json:"metadata_root_closure"`
	MetadataDiscovery       string    `json:"metadata_discovery"`
	MetadataBlockers        []string  `json:"metadata_blockers"`
	GlobalInventoryComplete bool      `json:"global_inventory_complete"`
	JobID                   string    `json:"job_id"`
	OperationID             string    `json:"operation_id"`
	AttemptID               string    `json:"attempt_id"`
	Fence                   uint64    `json:"fence"`
	InputHash               string    `json:"input_hash"`
	RootSetHash             string    `json:"root_set_hash"`
	InventoryID             string    `json:"inventory_id"`
	GraphHash               string    `json:"graph_hash"`
	ObservedAt              time.Time `json:"observed_at"`
}
