package application

import (
	"context"
	"reflect"
	"releasecontrol/internal/domain"
	"testing"
)

func TestExternalSystemInstanceSharingScopeAndGateReadiness(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "create_product", ID: "other", Data: map[string]any{"name": "Other"}})
	exec(t, s, domain.Command{Action: "create_feature", ID: "foreign", ProductID: "other", Data: map[string]any{"title": "Other work", "goal": "Other"}})
	exec(t, s, domain.Command{Action: "create_external_system", ID: "billing", ProductID: "p", Data: map[string]any{"name": "Billing partner", "team": "Partner team", "contracts": []string{"Keep API v1"}}})
	exec(t, s, domain.Command{Action: "create_external_system", ID: "unrelated", Data: map[string]any{"name": "Unrelated"}})
	if m.state.ExternalSystems[0].ProductID != "" {
		t.Fatal("external system became product owned")
	}
	exec(t, s, domain.Command{Action: "create_system_relationship", ID: "relation", ProductID: "p", Data: map[string]any{"external_system_id": "billing", "type": "CONSUMES"}})
	exec(t, s, domain.Command{Action: "create_system_relationship", ID: "foreign-relation", ProductID: "other", Data: map[string]any{"external_system_id": "billing", "type": "DEPENDS_ON"}})
	reject(t, s, domain.Command{Action: "set_external_scope", FeatureID: "f", Data: map[string]any{"relationship_ids": []string{"foreign-relation"}}})
	exec(t, s, domain.Command{Action: "set_external_scope", FeatureID: "f", Data: map[string]any{"relationship_ids": []string{"relation"}}})
	exec(t, s, domain.Command{Action: "create_gate", ID: "external-gate", FeatureID: "f", Data: map[string]any{"title": "Partner contract", "reason": "API compatibility", "integration_ids": []string{"i"}}})
	exec(t, s, domain.Command{Action: "add_check", ID: "external-check", GateID: "external-gate", Data: map[string]any{"title": "Provider contract check", "mechanism": "integration"}})
	exec(t, s, domain.Command{Action: "record_check_result", CheckID: "external-check", Data: map[string]any{"result": "passed", "commit": "abc"}})
	exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "set_external_scope", GateID: "external-gate", Data: map[string]any{"external_system_ids": []string{"billing"}}})
	if m.state.Integrations[0].Status != "working" {
		t.Fatal("changed blocking gate kept stale readiness")
	}
	exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
	reject(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "record_check_result", CheckID: "external-check", Data: map[string]any{"result": "passed", "commit": "abc"}})
	exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	value, err := s.Resume(context.Background(), "f")
	if err != nil {
		t.Fatal(err)
	}
	resume := value.(map[string]any)
	systems := resume["external_systems"].([]domain.ExternalSystem)
	if len(systems) != 1 || systems[0].ID != "billing" {
		t.Fatalf("unrelated external context leaked: %+v", systems)
	}
	exec(t, s, domain.Command{Action: "update_external_system", ID: "billing", Data: map[string]any{"contact": "partner@example.test"}})
	if len(m.state.ExternalSystems) != 2 || m.state.ExternalSystems[0].Name != "Billing partner" || m.state.ExternalSystems[0].Contact != "partner@example.test" {
		t.Fatal("sparse system update failed")
	}
	if len(m.state.Results) != 2 {
		t.Fatal("scope change rewrote check evidence")
	}
}
func TestExternalScopeHistoryAndIntegrationFiltering(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "create_integration", ID: "j", FeatureID: "f", Data: map[string]any{"title": "Other slice", "objective": "Other"}})
	for _, id := range []string{"a", "b"} {
		exec(t, s, domain.Command{Action: "create_external_system", ID: id, Data: map[string]any{"name": id}})
	}
	exec(t, s, domain.Command{Action: "set_external_scope", IntegrationID: "i", Data: map[string]any{"external_system_ids": []string{"a"}}})
	exec(t, s, domain.Command{Action: "set_external_scope", IntegrationID: "j", Data: map[string]any{"external_system_ids": []string{"b"}}})
	scoped := externalContext(&m.state, "f", "i")["external_systems"].([]domain.ExternalSystem)
	if len(scoped) != 1 || scoped[0].ID != "a" {
		t.Fatalf("wrong integration scope %+v", scoped)
	}
	original := m.state.ExternalScopes[0]
	exec(t, s, domain.Command{Action: "set_external_scope", IntegrationID: "i", Data: map[string]any{"external_system_ids": []string{}}})
	if len(m.state.ExternalScopes) != 3 || !reflect.DeepEqual(original, m.state.ExternalScopes[0]) {
		t.Fatal("scope history overwritten")
	}
	if len(externalContext(&m.state, "f", "i")["external_systems"].([]domain.ExternalSystem)) != 0 {
		t.Fatal("cleared scope still current")
	}
	before := len(m.state.Events)
	reject(t, s, domain.Command{Action: "set_external_scope", IntegrationID: "i", Data: map[string]any{"external_system_ids": []string{"missing"}}})
	if len(m.state.Events) != before {
		t.Fatal("invalid scope mutated audit")
	}
	reject(t, s, domain.Command{Action: "create_system_relationship", ProductID: "p", Data: map[string]any{"external_system_id": "a", "component_id": "missing", "type": "CONSUMES"}})
	reject(t, s, domain.Command{Action: "create_external_system", Data: map[string]any{"name": "A"}})
}
