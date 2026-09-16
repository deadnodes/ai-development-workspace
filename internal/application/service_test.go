package application

import (
	"context"
	"encoding/json"
	"releasecontrol/internal/domain"
	"testing"
)

type memoryStore struct{ state domain.State }

func (m *memoryStore) Read(context.Context) (domain.State, error) { return m.state, nil }
func (m *memoryStore) Update(_ context.Context, fn func(*domain.State) error) error {
	b, _ := json.Marshal(m.state)
	var copy domain.State
	_ = json.Unmarshal(b, &copy)
	if e := fn(&copy); e != nil {
		return e
	}
	m.state = copy
	return nil
}
func fixture(t *testing.T) (*Service, *memoryStore) {
	t.Helper()
	m := &memoryStore{domain.EmptyState()}
	s := New(m)
	exec(t, s, domain.Command{Action: "create_product", ID: "p", Data: map[string]any{"name": "Product"}})
	exec(t, s, domain.Command{Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "Feature", "goal": "Goal"}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "Slice", "objective": "Ship"}})
	return s, m
}
func exec(t *testing.T, s *Service, c domain.Command) any {
	t.Helper()
	c.Actor = "agent"
	v, e := s.Execute(context.Background(), c)
	if e != nil {
		t.Fatalf("%s: %v", c.Action, e)
	}
	return v
}
func reject(t *testing.T, s *Service, c domain.Command) {
	t.Helper()
	c.Actor = "agent"
	if _, e := s.Execute(context.Background(), c); e == nil {
		t.Fatalf("expected rejection: %+v", c)
	}
}
func TestFailedGateRequiresResolutionAndRerun(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "create_gate", ID: "g", FeatureID: "f", Data: map[string]any{"title": "gate", "reason": "risk", "integration_ids": []string{"i"}}})
	exec(t, s, domain.Command{Action: "add_check", ID: "c", GateID: "g", Data: map[string]any{"title": "test", "mechanism": "unit"}})
	reject(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "record_check_result", ID: "r", CheckID: "c", Data: map[string]any{"result": "failed", "commit": "abc"}})
	exec(t, s, domain.Command{Action: "record_finding", ID: "finding", ResultID: "r", Data: map[string]any{"title": "broken", "severity": "major", "integration_ids": []string{"i"}}})
	exec(t, s, domain.Command{Action: "resolve_finding", ID: "finding", Data: map[string]any{"body": "fixed", "commit": "def"}})
	reject(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "record_check_result", CheckID: "c", Data: map[string]any{"result": "passed", "commit": "def"}})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	if len(m.state.Results) != 2 || m.state.Results[0].Result != "failed" {
		t.Fatal("history overwritten")
	}
}
func TestDependencyCycleAndAtomicFailure(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "create_integration", ID: "j", FeatureID: "f", Data: map[string]any{"title": "next", "objective": "next", "dependencies": []string{"i"}}})
	n := len(m.state.Events)
	reject(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"dependencies": []string{"j"}}})
	if len(m.state.Events) != n || len(m.state.Integrations[0].Dependencies) != 0 {
		t.Fatal("failure persisted")
	}
	reject(t, s, domain.Command{Action: "complete_integration", IntegrationID: "j"})
	reject(t, s, domain.Command{Action: "update_feature", FeatureID: "f", Data: map[string]any{"id": "pwn"}})
}
func TestBlockerAndHandoffHistory(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "add_blocker", ID: "b", FeatureID: "f", Data: map[string]any{"body": "Need specification"}})
	reject(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "resolve_blocker", ID: "b", Data: map[string]any{"body": "specified"}})
	exec(t, s, domain.Command{Action: "handoff", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"current": "API", "next": []string{"test"}, "warnings": []string{"keep v1"}}})
	exec(t, s, domain.Command{Action: "handoff", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"current": "tests", "next": []string{"finish"}}})
	if len(m.state.Memories) != 3 {
		t.Fatal("handoff overwritten")
	}
	v, e := s.Resume(context.Background(), "f")
	if e != nil || v.(map[string]any)["next_actions"].([]string)[0] != "finish" {
		t.Fatal("incorrect resume", e)
	}
}
func TestCrossFeatureAndEvidenceValidation(t *testing.T) {
	s, _ := fixture(t)
	exec(t, s, domain.Command{Action: "create_feature", ID: "f2", ProductID: "p", Data: map[string]any{"title": "Other", "goal": "Other"}})
	reject(t, s, domain.Command{Action: "create_gate", FeatureID: "f2", Data: map[string]any{"title": "bad", "reason": "bad", "integration_ids": []string{"i"}}})
	exec(t, s, domain.Command{Action: "create_gate", ID: "g", FeatureID: "f", Data: map[string]any{"title": "gate", "reason": "risk", "integration_ids": []string{"i"}, "commit": "current"}})
	exec(t, s, domain.Command{Action: "add_check", ID: "c", GateID: "g", Data: map[string]any{"title": "test", "mechanism": "unit"}})
	reject(t, s, domain.Command{Action: "record_check_result", CheckID: "c", Data: map[string]any{"result": "passed"}})
	exec(t, s, domain.Command{Action: "record_check_result", CheckID: "c", Data: map[string]any{"result": "passed", "commit": "old"}})
	reject(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
}
func TestRejectCaseBypassAndDuplicateHistoryID(t *testing.T) {
	s, m := fixture(t)
	reject(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"Status": "released"}})
	exec(t, s, domain.Command{Action: "record_discovery", ID: "d", FeatureID: "f", Data: map[string]any{"body": "original"}})
	reject(t, s, domain.Command{Action: "record_discovery", ID: "d", FeatureID: "f", Data: map[string]any{"body": "overwrite"}})
	if m.state.Memories[0].Body != "original" {
		t.Fatal("history changed")
	}
}
func TestReleaseSnapshotAndDependencySelection(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "create_integration", ID: "j", FeatureID: "f", Data: map[string]any{"title": "second", "objective": "ship", "dependencies": []string{"i"}}})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "j"})
	reject(t, s, domain.Command{Action: "plan_release", ProductID: "p", Data: map[string]any{"name": "incomplete", "integration_ids": []string{"j"}}})
	exec(t, s, domain.Command{Action: "plan_release", ProductID: "p", Data: map[string]any{"name": "intent", "integration_ids": []string{"i", "j"}}})
	exec(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"title": "changed"}})
	r := m.state.Releases[0]
	if r.Status != "planned" || r.Snapshots[0].Title != "Slice" || m.state.Integrations[0].Status != "working" {
		t.Fatal("release intent snapshot changed")
	}
}
func TestFeatureAuditIsInResume(t *testing.T) {
	s, _ := fixture(t)
	exec(t, s, domain.Command{Action: "update_feature", FeatureID: "f", Data: map[string]any{"goal": "Revised"}})
	v, e := s.Resume(context.Background(), "f")
	if e != nil {
		t.Fatal(e)
	}
	events := v.(map[string]any)["events"].([]EventSummary)
	if len(events) != 3 || events[0].Action != "create_feature" || events[2].Action != "update_feature" {
		t.Fatalf("incomplete feature audit: %+v", events)
	}
}
