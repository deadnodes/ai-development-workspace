package domain

// StatePage is navigation metadata, never a State export or readiness assessment.
type StatePageRequest struct {
	ProductID string `json:"product_id,omitempty"`
	FeatureID string `json:"feature_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}
type StateCard struct {
	ID             string `json:"id"`
	ProductID      string `json:"product_id,omitempty"`
	FeatureID      string `json:"feature_id,omitempty"`
	Name           string `json:"name,omitempty"`
	Title          string `json:"title,omitempty"`
	Truncated      bool   `json:"truncated"`
	RecordedStatus string `json:"recorded_status,omitempty"`
}
type StateScope struct {
	Collection string     `json:"collection"`
	Product    *StateCard `json:"product,omitempty"`
	Feature    *StateCard `json:"feature,omitempty"`
}
type StatePagination struct {
	Returned   int    `json:"returned"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}
type StatePage struct {
	Projection  string          `json:"projection"`
	Scope       StateScope      `json:"scope"`
	Items       []StateCard     `json:"items"`
	Pagination  StatePagination `json:"pagination"`
	Consistency string          `json:"consistency"`
	Guidance    string          `json:"guidance"`
}

// Internal bounded reader contract. Implementations return at most Limit cards,
// with labels bounded before transfer; application validation remains mandatory.
type StateMetadataRequest struct {
	ProductID, FeatureID, Collection, AfterID string
	Limit                                     int
}
type StateMetadata struct {
	Product, Feature *StateCard
	Items            []StateCard
}
