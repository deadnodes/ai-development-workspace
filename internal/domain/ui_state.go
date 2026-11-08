package domain

// UIState is a bounded read model, never an export or execution input.
type UIStateRequest struct {
	ProductID         string
	FeatureID         string
	RestrictProducts  bool
	AllowedProductIDs []string
}
type UIState struct {
	State
	Projection UIProjection `json:"projection"`
}
type UIProjection struct {
	Kind            string   `json:"kind"`
	ProductID       string   `json:"product_id"`
	FeatureID       string   `json:"feature_id,omitempty"`
	CollectionLimit int      `json:"collection_limit"`
	HistoryLimit    int      `json:"history_limit"`
	Truncated       []string `json:"truncated"`
	Guidance        string   `json:"guidance"`
}
