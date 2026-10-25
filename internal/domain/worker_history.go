package domain

// WorkerHistoryRequest addresses native worker metadata only. Each collection
// has an independent, scope-bound live keyset cursor.
type WorkerHistoryRequest struct {
	ProductID  string `json:"product_id"`
	FeatureID  string `json:"feature_id,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	Collection string `json:"collection,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}

type WorkerHistoryItem struct {
	DeliveryID     string `json:"delivery_id,omitempty"`
	ParentID       string `json:"parent_id,omitempty"`
	SnapshotHash   string `json:"snapshot_hash,omitempty"`
	ParentHash     string `json:"parent_hash,omitempty"`
	ID             string `json:"id"`
	ProductID      string `json:"product_id"`
	JobID          string `json:"job_id,omitempty"`
	OperationID    string `json:"operation_id,omitempty"`
	IntegrationID  string `json:"integration_id,omitempty"`
	FeatureID      string `json:"feature_id,omitempty"`
	RevisionID     string `json:"revision_id,omitempty"`
	RepositoryID   string `json:"repository_id,omitempty"`
	SourceSHA      string `json:"source_sha,omitempty"`
	RecipeHash     string `json:"recipe_hash,omitempty"`
	InputHash      string `json:"input_hash,omitempty"`
	Capability     string `json:"capability,omitempty"`
	RecordedStatus string `json:"recorded_status,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	Provenance     string `json:"provenance,omitempty"`
	Attempts       int    `json:"attempts,omitempty"`
	Links          int    `json:"links,omitempty"`
}

type WorkerHistoryPage struct {
	Projection  string              `json:"projection"`
	ProductID   string              `json:"product_id"`
	FeatureID   string              `json:"feature_id,omitempty"`
	JobID       string              `json:"job_id,omitempty"`
	Collection  string              `json:"collection"`
	Items       []WorkerHistoryItem `json:"items"`
	Pagination  StatePagination     `json:"pagination"`
	Consistency string              `json:"consistency"`
	Guidance    string              `json:"guidance"`
}

type WorkerHistoryMetadataRequest struct {
	ProductID, FeatureID, JobID, Collection, AfterID string
	Limit                                            int
}

type WorkerHistoryMetadata struct {
	ProductFound, FeatureFound, JobFound bool
	Items                                []WorkerHistoryItem
}
