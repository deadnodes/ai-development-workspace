package domain

import "releasecontrol/internal/delivery"

// DevActivationProof binds a new Dev execution to original, immutable Prod
// build provenance. It does not grant merge, acceptance or restoration rights.
type DevActivationProof struct {
	BaselineCaptureID      string                   `json:"baseline_capture_id"`
	BaselineCaptureHash    string                   `json:"baseline_capture_hash"`
	OriginalBaselineHash   string                   `json:"original_baseline_hash"`
	Request                DevReservationRequest    `json:"request"`
	ReservationOperationID string                   `json:"reservation_operation_id"`
	ReservationHash        string                   `json:"reservation_hash"`
	CandidateOperationID   string                   `json:"candidate_operation_id"`
	CandidateHash          string                   `json:"candidate_hash"`
	Components             []DevActivationComponent `json:"components"`
	Groups                 []DevActivationGroup     `json:"groups"`
	SnapshotHash           string                   `json:"snapshot_hash"`
}
type DevActivationComponent struct {
	GroupID         string                      `json:"group_id"`
	Selector        delivery.GitOpsRequest      `json:"selector"`
	Baseline        DevRuntimeGitOpsObservation `json:"baseline"`
	ChildID         string                      `json:"child_id"`
	OriginalChildID string                      `json:"original_child_id"`
	Artifact        DeliveryArtifact            `json:"artifact"`
	Snapshot        DeliverySnapshot            `json:"snapshot"`
}

// DevActivationGroup is server-owned immutable physical destination membership.
type DevActivationGroup struct {
	ID         string              `json:"id"`
	LeaderID   string              `json:"leader_id"`
	MemberIDs  []string            `json:"member_ids"`
	Connection delivery.Connection `json:"connection"`
	Repository string              `json:"repository"`
	Ref        string              `json:"ref"`
}
