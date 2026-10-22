package domain

import (
	"releasecontrol/internal/delivery"
	"time"
)

// DevRuntimeRequest requests a server-owned observation, never an acceptance.
type DevRuntimeRequest struct {
	ActivationOperationID string                `json:"activation_operation_id"`
	RequestKey            string                `json:"request_key"`
	ExpectedSelection     *EnvironmentSelection `json:"expected_selection"`
}

// DevRuntimeProof freezes complete operator coverage and immutable activation identity.
type DevRuntimeProof struct {
	Request        DevRuntimeRequest     `json:"request"`
	Generation     uint64                `json:"generation"`
	ActivationHash string                `json:"activation_hash"`
	Components     []DevRuntimeComponent `json:"components"`
	SnapshotHash   string                `json:"snapshot_hash"`
}
type DevRuntimeComponent struct {
	AuthorityHash   string                              `json:"authority_hash"`
	ApplicationID   string                              `json:"application_id"`
	ChildID         string                              `json:"child_id"`
	ArtifactID      string                              `json:"artifact_id"`
	ImageRepository string                              `json:"image_repository"`
	Digest          string                              `json:"digest"`
	GitOps          delivery.GitOpsRequest              `json:"gitops"`
	GitOpsCommit    string                              `json:"gitops_commit"`
	ConfigHash      string                              `json:"config_hash"`
	Scopes          []delivery.RuntimeVerificationScope `json:"scopes"`
}

// DevRuntimeResult contains bounded minimal observations; no raw provider errors.
type DevRuntimeResult struct {
	Reason       string                      `json:"reason"`
	ExpectedHash string                      `json:"expected_hash"`
	Outcome      string                      `json:"outcome"`
	ObservedAt   time.Time                   `json:"observed_at"`
	Components   []DevRuntimeComponentResult `json:"components,omitempty"`
	Hash         string                      `json:"hash"`
}
type DevRuntimeComponentResult struct {
	ApplicationID string                       `json:"application_id"`
	GitOps        DevRuntimeGitOpsObservation  `json:"gitops"`
	Revision      delivery.GitOpsRevisionProof `json:"revision"`
	Roles         []delivery.RuntimeSnapshot   `json:"roles"`
}

type DevRuntimeGitOpsObservation struct {
	ImageRepository string `json:"image_repository"`
	HeadSHA         string `json:"head_sha"`
	BlobSHA         string `json:"blob_sha"`
	ContentHash     string `json:"content_hash"`
	Digest          string `json:"digest"`
}
