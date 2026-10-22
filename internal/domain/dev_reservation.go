package domain

// DevReservationRequest is an exact attributed request, never an approval claim.
type DevReservationRequest struct {
	CandidateOperationID   string                `json:"candidate_operation_id,omitempty"`
	EnvironmentID          string                `json:"environment_id,omitempty"`
	ReservationOperationID string                `json:"reservation_operation_id,omitempty"`
	RequestKey             string                `json:"request_key"`
	ExpectedSelection      *EnvironmentSelection `json:"expected_selection"`
}

// DevReservationProof freezes the candidate and the preserved shared baseline.
// Reconciliation quarantine belongs to the receipt, outside this immutable proof.
type DevReservationProof struct {
	Request                DevReservationRequest `json:"request"`
	CandidateOperationID   string                `json:"candidate_operation_id"`
	CandidateHash          string                `json:"candidate_hash"`
	CandidateProof         MainCandidateProof    `json:"candidate_proof"`
	BaselineSelection      EnvironmentSelection  `json:"baseline_selection"`
	ReservedSelection      EnvironmentSelection  `json:"reserved_selection"`
	Epoch                  uint64                `json:"epoch"`
	Environment            Environment           `json:"environment"`
	Targets                []EnvironmentBinding  `json:"targets"`
	ReservationOperationID string                `json:"reservation_operation_id,omitempty"`
}
