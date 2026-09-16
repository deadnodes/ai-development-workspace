package application

import (
	"releasecontrol/internal/domain"
	"slices"
	"sort"
	"strings"
)

func externalSystem(st *domain.State, id string) *domain.ExternalSystem {
	for i := range st.ExternalSystems {
		if st.ExternalSystems[i].ID == id {
			return &st.ExternalSystems[i]
		}
	}
	return nil
}
func systemRelationship(st *domain.State, id string) *domain.SystemRelationship {
	for i := range st.SystemRelationships {
		if st.SystemRelationships[i].ID == id {
			return &st.SystemRelationships[i]
		}
	}
	return nil
}
func applyExternalSystems(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	switch c.Action {
	case "create_external_system", "update_external_system":
		// Instance ownership cannot accidentally inherit the currently selected product.
		m.ProductID = ""
		m.FeatureID = ""
		var input struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Team        string   `json:"team"`
			Contact     string   `json:"contact"`
			Interfaces  []string `json:"interfaces"`
			Contracts   []string `json:"contracts"`
			Notes       string   `json:"notes"`
		}
		var previous *domain.ExternalSystem
		if c.Action == "update_external_system" {
			previous = externalSystem(st, c.ID)
			if previous == nil {
				return nil, m, missing("external system", c.ID)
			}
			input.Name = previous.Name
			input.Description = previous.Description
			input.Team = previous.Team
			input.Contact = previous.Contact
			input.Interfaces = previous.Interfaces
			input.Contracts = previous.Contracts
			input.Notes = previous.Notes
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Name == "" {
			return nil, m, invalid("external system name required")
		}
		for _, system := range st.ExternalSystems {
			if (previous == nil || system.ID != previous.ID) && strings.EqualFold(system.Name, input.Name) {
				return nil, m, invalid("external system name already exists")
			}
		}
		value := domain.ExternalSystem{Meta: m, Name: input.Name, Description: input.Description, Team: input.Team, Contact: input.Contact, Interfaces: input.Interfaces, Contracts: input.Contracts, Notes: input.Notes}
		if previous != nil {
			value.ID = previous.ID
			value.CreatedAt = previous.CreatedAt
			*previous = value
		} else {
			st.ExternalSystems = append(st.ExternalSystems, value)
		}
		return value, value.Meta, nil
	case "create_system_relationship":
		var input struct {
			ComponentID      string `json:"component_id"`
			ExternalSystemID string `json:"external_system_id"`
			Type             string `json:"type"`
			Notes            string `json:"notes"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		if !productExists(st, c.ProductID) {
			return nil, m, missing("product", c.ProductID)
		}
		if externalSystem(st, input.ExternalSystemID) == nil {
			return nil, m, missing("external system", input.ExternalSystemID)
		}
		if input.ComponentID != "" {
			component := applicationByID(st, input.ComponentID)
			if component == nil || component.ProductID != c.ProductID {
				return nil, m, invalid("component must belong to relationship product")
			}
		}
		if !slices.Contains([]string{"DEPENDS_ON", "CONSUMES", "PROVIDES_TO", "SHARES_DATA_WITH"}, input.Type) {
			return nil, m, invalid("unsupported external relationship type")
		}
		for _, r := range st.SystemRelationships {
			if r.ProductID == c.ProductID && r.ComponentID == input.ComponentID && r.ExternalSystemID == input.ExternalSystemID && r.Type == input.Type {
				return nil, m, invalid("relationship already exists")
			}
		}
		m.FeatureID = ""
		value := domain.SystemRelationship{Meta: m, ComponentID: input.ComponentID, ExternalSystemID: input.ExternalSystemID, Type: input.Type, Notes: input.Notes}
		st.SystemRelationships = append(st.SystemRelationships, value)
		return value, m, nil
	case "set_external_scope":
		var input struct {
			ExternalSystemIDs []string `json:"external_system_ids"`
			RelationshipIDs   []string `json:"relationship_ids"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		if c.GateID != "" {
			g := gate(st, c.GateID)
			if g == nil {
				return nil, m, missing("gate", c.GateID)
			}
			if c.FeatureID != "" && g.FeatureID != c.FeatureID {
				return nil, m, invalid("gate feature mismatch")
			}
			if c.IntegrationID != "" {
				return nil, m, invalid("scope has one integration or gate target")
			}
			c.FeatureID = g.FeatureID
			m.FeatureID = g.FeatureID
			m.ProductID = g.ProductID
		}
		f := feature(st, c.FeatureID)
		if f == nil {
			return nil, m, missing("feature", c.FeatureID)
		}
		m.FeatureID = f.ID
		m.ProductID = f.ProductID
		if c.IntegrationID != "" {
			in := integration(st, c.IntegrationID)
			if in == nil || in.FeatureID != f.ID {
				return nil, m, invalid("scope integration must belong to feature")
			}
		}
		seen := map[string]bool{}
		for _, id := range input.ExternalSystemIDs {
			if seen[id] || externalSystem(st, id) == nil {
				return nil, m, invalid("external system scope reference missing or duplicated")
			}
			seen[id] = true
		}
		seen = map[string]bool{}
		for _, id := range input.RelationshipIDs {
			r := systemRelationship(st, id)
			if seen[id] || r == nil || r.ProductID != f.ProductID {
				return nil, m, invalid("relationship scope must be unique and belong to feature product")
			}
			seen[id] = true
		}
		if input.ExternalSystemIDs == nil {
			input.ExternalSystemIDs = []string{}
		}
		if input.RelationshipIDs == nil {
			input.RelationshipIDs = []string{}
		}
		value := domain.ExternalScope{Meta: m, IntegrationID: c.IntegrationID, GateID: c.GateID, ExternalSystemIDs: input.ExternalSystemIDs, RelationshipIDs: input.RelationshipIDs}
		st.ExternalScopes = append(st.ExternalScopes, value)
		// A changed verification contract requires fresh evidence and reopens readiness.
		if c.GateID != "" {
			g := gate(st, c.GateID)
			if g.Blocking {
				for i := range st.Integrations {
					in := &st.Integrations[i]
					if slices.Contains(g.IntegrationIDs, in.ID) && in.Status == "ready" {
						in.Status = "working"
						in.UpdatedAt = m.UpdatedAt
					}
				}
			}
		}
		return value, m, nil
	}
	return nil, m, invalid("unknown external systems command")
}
func latestExternalScopes(st *domain.State) []domain.ExternalScope {
	byTarget := map[string]domain.ExternalScope{}
	for _, scope := range st.ExternalScopes {
		key := scope.FeatureID + "/" + scope.IntegrationID + "/" + scope.GateID
		byTarget[key] = scope
	}
	values := make([]domain.ExternalScope, 0, len(byTarget))
	for _, scope := range byTarget {
		values = append(values, scope)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	return values
}
func externalContext(st *domain.State, featureID, integrationID string) map[string]any {
	scopes := []domain.ExternalScope{}
	relations := []domain.SystemRelationship{}
	systems := []domain.ExternalSystem{}
	systemIDs := map[string]bool{}
	relationIDs := map[string]bool{}
	for _, scope := range latestExternalScopes(st) {
		if scope.FeatureID != featureID {
			continue
		}
		if integrationID != "" {
			if scope.IntegrationID != "" && scope.IntegrationID != integrationID {
				continue
			}
			if scope.GateID != "" {
				g := gate(st, scope.GateID)
				if g == nil || !slices.Contains(g.IntegrationIDs, integrationID) {
					continue
				}
			}
		}
		scopes = append(scopes, scope)
		for _, id := range scope.ExternalSystemIDs {
			systemIDs[id] = true
		}
		for _, id := range scope.RelationshipIDs {
			relationIDs[id] = true
		}
	}
	for _, r := range st.SystemRelationships {
		if relationIDs[r.ID] {
			relations = append(relations, r)
			systemIDs[r.ExternalSystemID] = true
		}
	}
	for _, system := range st.ExternalSystems {
		if systemIDs[system.ID] {
			systems = append(systems, system)
		}
	}
	return map[string]any{"external_systems": systems, "system_relationships": relations, "external_scopes": scopes}
}
func addExternalContext(out map[string]any, st domain.State, featureID string) {
	for key, value := range externalContext(&st, featureID, "") {
		out[key] = value
	}
}
func externalGateEvidenceCurrent(st *domain.State, g domain.Gate) bool {
	var scope *domain.ExternalScope
	for i := range st.ExternalScopes {
		if st.ExternalScopes[i].GateID == g.ID {
			scope = &st.ExternalScopes[i]
		}
	}
	if scope == nil {
		return true
	}
	for _, check := range st.Checks {
		if check.GateID == g.ID {
			r := latest(st, check.ID)
			if r == nil || r.CreatedAt.Before(scope.CreatedAt) {
				return false
			}
		}
	}
	return true
}
