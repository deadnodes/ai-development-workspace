package domain

// ProjectKnowledge is curated current documentation, independent of feature history.
type ProjectKnowledge struct {
	Meta
	Overview      string             `json:"overview"`
	Instructions  string             `json:"instructions"`
	Areas         []ProductArea      `json:"areas"`
	Relationships []AreaRelationship `json:"relationships"`
	Nodes         []KnowledgeNode    `json:"nodes"`
	Edges         []KnowledgeEdge    `json:"edges"`
	Parameters    map[string]string  `json:"parameters"`
}
type ProductArea struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ParentID      string   `json:"parent_id,omitempty"`
	Description   string   `json:"description"`
	RepositoryIDs []string `json:"repository_ids"`
}
type AreaRelationship struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Type     string `json:"type"`
	Contract string `json:"contract"`
}

// KnowledgeNode is a durable, searchable product fact. It is intentionally
// smaller than a generic document or service catalog entry: agents should use
// it for decisions, contracts, architecture constraints and runbook facts
// that need to survive across feature conversations.
type KnowledgeNode struct {
	Meta
	Kind           string   `json:"kind"`
	Title          string   `json:"title"`
	Content        string   `json:"content"`
	Keywords       []string `json:"keywords"`
	AreaID         string   `json:"area_id,omitempty"`
	RepositoryIDs  []string `json:"repository_ids"`
	RelatedNodeIDs []string `json:"related_node_ids"`
	Status         string   `json:"status"`
}

type KnowledgeEdge struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Type        string `json:"type"`
	Description string `json:"description"`
}
type RepositoryDocument struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	SHA256    string `json:"sha256"`
	Truncated bool   `json:"truncated"`
}
type LocalCheckout struct {
	ScanID string `json:"scan_id"`
	Meta
	RepositoryID string              `json:"repository_id"`
	Workspace    string              `json:"workspace"`
	RelativePath string              `json:"relative_path"`
	Branch       string              `json:"branch"`
	Commit       string              `json:"commit"`
	Agents       *RepositoryDocument `json:"agents,omitempty"`
	Errors       []string            `json:"errors"`
}
type WorkspaceDocument struct {
	Meta
	Workspace string              `json:"workspace"`
	Agents    *RepositoryDocument `json:"agents,omitempty"`
}
