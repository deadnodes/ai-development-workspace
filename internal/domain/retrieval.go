package domain

// Retrieval queries deliberately return cards or text windows, never State or audit payloads.
type ContextSearchRequest struct {
	ProductID string `json:"product_id"`
	FeatureID string `json:"feature_id,omitempty"`
	Query     string `json:"query"`
	Limit     int    `json:"limit,omitempty"`
}
type ContextCard struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	ProductID    string  `json:"product_id"`
	FeatureID    string  `json:"feature_id,omitempty"`
	Title        string  `json:"title"`
	Snippet      string  `json:"snippet,omitempty"`
	Status       string  `json:"recorded_status,omitempty"`
	Score        float64 `json:"score,omitempty"`
	TextChars    int     `json:"text_chars"`
	IndexedChars int     `json:"indexed_chars"`
	Truncated    bool    `json:"truncated"`
}
type ContextSearchResult struct {
	Items    []ContextCard `json:"items"`
	HasMore  bool          `json:"has_more"`
	Guidance string        `json:"guidance"`
}
type ContextExcerptRequest struct {
	ProductID string `json:"product_id"`
	ID        string `json:"id"`
	Offset    int    `json:"offset,omitempty"`
	MaxChars  int    `json:"max_chars,omitempty"`
}
type ContextExcerpt struct {
	ContextCard
	Text       string `json:"text"`
	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset,omitempty"`
}
type GraphNeighborsRequest struct {
	ProductID string `json:"product_id"`
	ID        string `json:"id"`
	Depth     int    `json:"depth,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}
type ContextEdge struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Relation string `json:"relation"`
}
type GraphNeighborsResult struct {
	Seed      ContextCard   `json:"seed"`
	Nodes     []ContextCard `json:"nodes"`
	Edges     []ContextEdge `json:"edges"`
	Truncated bool          `json:"truncated"`
	Guidance  string        `json:"guidance"`
}
