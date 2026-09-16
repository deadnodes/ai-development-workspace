package domain

// CompositionConflict retains immutable source and semantic context from the failed operation.
// Resolution is performed externally; passing checks never automatically deploy it.
type CompositionConflict struct {
	Meta
	OperationID      string        `json:"operation_id"`
	RepositoryID     string        `json:"repository_id"`
	CompositionID    string        `json:"composition_id"`
	Source           FlowSource    `json:"source"`
	Integrations     []Integration `json:"integrations"`
	Features         []Feature     `json:"features"`
	Gates            []Gate        `json:"gates"`
	Checks           []Check       `json:"checks"`
	Memories         []Memory      `json:"memories"`
	Status           string        `json:"status"`
	ClaimedBy        string        `json:"claimed_by"`
	ResolutionCommit string        `json:"resolution_commit"`
	Rationale        string        `json:"rationale"`
	ResultIDs        []string      `json:"result_ids"`
}
