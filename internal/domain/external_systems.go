package domain

// ExternalSystem is shared instance context for an independently owned system.
// It deliberately owns no managed repositories, branches, artifacts or environments.
type ExternalSystem struct {
	Meta
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Team        string   `json:"team"`
	Contact     string   `json:"contact"`
	Interfaces  []string `json:"interfaces"`
	Contracts   []string `json:"contracts"`
	Notes       string   `json:"notes"`
}
type SystemRelationship struct {
	Meta
	ComponentID      string `json:"component_id,omitempty"`
	ExternalSystemID string `json:"external_system_id"`
	Type             string `json:"type"`
	Notes            string `json:"notes"`
}

// ExternalScope is an append-only scope snapshot. Latest scope for the same
// feature/integration/gate target is current; earlier snapshots remain auditable.
type ExternalScope struct {
	Meta
	IntegrationID     string   `json:"integration_id,omitempty"`
	GateID            string   `json:"gate_id,omitempty"`
	ExternalSystemIDs []string `json:"external_system_ids"`
	RelationshipIDs   []string `json:"relationship_ids"`
}
