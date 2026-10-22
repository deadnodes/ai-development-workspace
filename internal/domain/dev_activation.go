package domain

// DevActivationProof binds a new Dev execution to original, immutable Prod
// build provenance. It does not grant merge, acceptance or restoration rights.
type DevActivationProof struct {
	Request                DevReservationRequest    `json:"request"`
	ReservationOperationID string                   `json:"reservation_operation_id"`
	ReservationHash        string                   `json:"reservation_hash"`
	CandidateOperationID   string                   `json:"candidate_operation_id"`
	CandidateHash          string                   `json:"candidate_hash"`
	Components             []DevActivationComponent `json:"components"`
	SnapshotHash           string                   `json:"snapshot_hash"`
}
type DevActivationComponent struct {
	ChildID         string           `json:"child_id"`
	OriginalChildID string           `json:"original_child_id"`
	Artifact        DeliveryArtifact `json:"artifact"`
	Snapshot        DeliverySnapshot `json:"snapshot"`
}
