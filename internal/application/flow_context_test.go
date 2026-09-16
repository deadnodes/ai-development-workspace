package application

import (
	"context"
	"releasecontrol/internal/domain"
	"testing"
)

func TestFlowContextIncludesSharedExecutionAndVersionedEvidence(t *testing.T) {
	s, m, _ := flowFixture(t)
	m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "parent", ProductID: "p"}, Kind: "COMPOSE", IntegrationIDs: []string{"i"}, ChildIDs: []string{"child"}}, domain.ExternalOperation{Meta: domain.Meta{ID: "child", ProductID: "p"}, ParentID: "parent"})
	m.state.ScenarioVersions = append(m.state.ScenarioVersions, domain.ScenarioVersion{Meta: domain.Meta{ID: "sv", ProductID: "p"}, IntegrationIDs: []string{"i"}})
	m.state.ScenarioRuns = append(m.state.ScenarioRuns, domain.ScenarioRun{Meta: domain.Meta{ID: "run", ProductID: "p"}, ScenarioVersionID: "sv"})
	m.state.RuntimeObservations = append(m.state.RuntimeObservations, domain.RuntimeObservation{Meta: domain.Meta{ID: "runtime", ProductID: "p"}, OperationID: "child"})
	value, err := s.Query(context.Background(), "get_integration_context", "i")
	if err != nil {
		t.Fatal(err)
	}
	out := value.(map[string]any)
	if len(out["operations"].([]domain.ExternalOperation)) != 2 || len(out["scenario_versions"].([]domain.ScenarioVersion)) != 1 || len(out["scenario_runs"].([]domain.ScenarioRun)) != 1 || len(out["runtime_observations"].([]domain.RuntimeObservation)) != 1 {
		t.Fatal("shared operation/evidence absent from agent context")
	}
	reject(t, s, domain.Command{Action: "update_environment", ID: "dev", Data: map[string]any{"desired_operation_id": "forged"}})
}
