package application

import (
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

func scenarioFixture() domain.State {
	meta := domain.Meta{ID: "p", ProductID: "p", Actor: "agent", CreatedAt: time.Now().UTC()}
	st := domain.State{Products: []domain.Product{{Meta: meta}}}
	meta.ID = "i"
	st.Integrations = []domain.Integration{{Meta: meta}}
	child := domain.ExternalOperation{Meta: domain.Meta{ID: "child", ProductID: "p"}, Status: "SUCCEEDED", Snapshot: &domain.DeliverySnapshot{Application: domain.Application{Meta: domain.Meta{ID: "app"}}, Revision: domain.IntegrationRevision{HeadCommit: strings.Repeat("a", 40)}}, Artifact: &delivery.ArtifactResult{Available: true, Digest: "sha256:" + strings.Repeat("b", 64)}}
	parent := domain.ExternalOperation{Meta: domain.Meta{ID: "candidate", ProductID: "p"}, Kind: "RELEASE_CANDIDATE", Status: "SUCCEEDED", DeploymentState: "READY_FOR_VERIFICATION", ChildIDs: []string{"child"}, IntegrationIDs: []string{"i"}}
	st.Operations = []domain.ExternalOperation{child, parent}
	return st
}
func scenarioDefinitionData() map[string]any {
	return map[string]any{"title": "Payments", "objective": "No duplicates", "integration_ids": []string{"i"}, "steps": []string{"retry"}, "expected_outcomes": []string{"one payment"}, "mechanism": "integration", "blocking": true}
}
func scenarioRunData() map[string]any {
	return map[string]any{"scenario_version_id": "scenario", "candidate_operation_id": "candidate", "components": []domain.ScenarioComponentEvidence{{ApplicationID: "app", OperationID: "child", SourceSHA: strings.Repeat("a", 40), ArtifactDigest: "sha256:" + strings.Repeat("b", 64)}}, "result": "passed", "observations": []string{"one payment observed"}}
}
func scenarioApply(st *domain.State, action, id string, data map[string]any) error {
	_, _, e := applyScenario(st, domain.Command{Action: action, ID: id, ProductID: "p", Actor: "agent", Data: data}, domain.Meta{ID: id, ProductID: "p", Actor: "agent", CreatedAt: time.Now().UTC()})
	return e
}
func TestScenarioVersionsAndCandidateEvidence(t *testing.T) {
	st := scenarioFixture()
	if err := scenarioApply(&st, "create_test_scenario", "scenario", scenarioDefinitionData()); err != nil {
		t.Fatal(err)
	}
	if err := scenarioCandidateReady(&st, "candidate"); err == nil {
		t.Fatal("untested candidate ready")
	}
	if err := scenarioApply(&st, "record_scenario_run", "run", scenarioRunData()); err != nil {
		t.Fatal(err)
	}
	if err := scenarioCandidateReady(&st, "candidate"); err != nil {
		t.Fatal(err)
	}
	revised := scenarioDefinitionData()
	revised["scenario_id"] = "scenario"
	revised["steps"] = []string{"retry twice"}
	if err := scenarioApply(&st, "revise_test_scenario", "v2", revised); err != nil {
		t.Fatal(err)
	}
	if len(st.ScenarioVersions) != 2 || st.ScenarioVersions[0].Steps[0] != "retry" || len(st.ScenarioRuns) != 1 {
		t.Fatal("history overwritten")
	}
	if err := scenarioCandidateReady(&st, "candidate"); err == nil {
		t.Fatal("old version approved new obligation")
	}
}
func TestScenarioRejectsForgedOrCrossProductEvidence(t *testing.T) {
	for _, field := range []string{"digest", "sha", "child", "product", "empty", "missing-integration"} {
		t.Run(field, func(t *testing.T) {
			st := scenarioFixture()
			if err := scenarioApply(&st, "create_test_scenario", "scenario", scenarioDefinitionData()); err != nil {
				t.Fatal(err)
			}
			data := scenarioRunData()
			components := data["components"].([]domain.ScenarioComponentEvidence)
			switch field {
			case "digest":
				components[0].ArtifactDigest = "sha256:" + strings.Repeat("c", 64)
			case "sha":
				components[0].SourceSHA = strings.Repeat("c", 40)
			case "child":
				components[0].OperationID = "unrelated"
			case "product":
				st.Operations[1].ProductID = "other"
			case "empty":
				data["components"] = []domain.ScenarioComponentEvidence{}
			case "missing-integration":
				st.Operations[1].IntegrationIDs = nil
			}
			if err := scenarioApply(&st, "record_scenario_run", "run", data); err == nil {
				t.Fatal("accepted invalid evidence")
			}
			if len(st.ScenarioRuns) != 0 {
				t.Fatal("invalid run persisted")
			}
		})
	}
}
func TestFailedRerunAndDifferentCandidateInvalidate(t *testing.T) {
	st := scenarioFixture()
	_ = scenarioApply(&st, "create_test_scenario", "scenario", scenarioDefinitionData())
	_ = scenarioApply(&st, "record_scenario_run", "run", scenarioRunData())
	failed := scenarioRunData()
	failed["result"] = "failed"
	if err := scenarioApply(&st, "record_scenario_run", "failed", failed); err != nil {
		t.Fatal(err)
	}
	if err := scenarioCandidateReady(&st, "candidate"); err == nil {
		t.Fatal("ignored failed rerun")
	}
	st.Operations[1].ID = "new-candidate"
	if err := scenarioCandidateReady(&st, "new-candidate"); err == nil {
		t.Fatal("reused old candidate evidence")
	}
}
func TestCompositionRunRequiresDeployedSnapshotAndKeepsHistoricalScope(t *testing.T) {
	st := scenarioFixture()
	_ = scenarioApply(&st, "create_test_scenario", "scenario", scenarioDefinitionData())
	composition := domain.Composition{Meta: domain.Meta{ID: "composition", ProductID: "p"}, EnvironmentID: "env", Components: []domain.CompositionComponent{{ApplicationID: "app"}}, RevisionSnapshots: []domain.IntegrationRevision{{IntegrationID: "i"}}}
	st.Compositions = []domain.Composition{composition}
	parent := &st.Operations[1]
	parent.Kind = "COMPOSE"
	parent.CompositionSnapshot = &composition
	parent.DeploymentState = "GITOPS_APPLIED"
	data := scenarioRunData()
	delete(data, "candidate_operation_id")
	data["composition_id"] = "composition"
	components := data["components"].([]domain.ScenarioComponentEvidence)
	components[0].DeploymentEvidence = "executor log: pod ran exact digest"
	if err := scenarioApply(&st, "record_scenario_run", "pending", data); err == nil {
		t.Fatal("gitops-only result accepted")
	}
	parent.DeploymentState = "DEPLOYED"
	if err := scenarioApply(&st, "record_scenario_run", "run", data); err != nil {
		t.Fatal(err)
	}
	if st.ScenarioRuns[0].EnvironmentID != "env" {
		t.Fatal("environment not captured")
	}
	parent.Kind = "RELEASE_CANDIDATE"
	parent.DeploymentState = "READY_FOR_VERIFICATION"
	if err := scenarioCandidateReady(&st, "candidate"); err == nil {
		t.Fatal("composition evidence used for main")
	}
}

func TestScenarioDefinitionScopeAndBlankEvidence(t *testing.T) {
	st := scenarioFixture()
	foreign := scenarioDefinitionData()
	foreign["integration_ids"] = []string{"unknown"}
	if err := scenarioApply(&st, "create_test_scenario", "wrong", foreign); err == nil {
		t.Fatal("unknown integration accepted")
	}
	if err := scenarioApply(&st, "create_test_scenario", "scenario", scenarioDefinitionData()); err != nil {
		t.Fatal(err)
	}
	revision := scenarioDefinitionData()
	revision["scenario_id"] = "unknown"
	if err := scenarioApply(&st, "revise_test_scenario", "wrong", revision); err == nil {
		t.Fatal("unknown scenario revised")
	}
	data := scenarioRunData()
	data["observations"] = []string{" "}
	if err := scenarioApply(&st, "record_scenario_run", "blank", data); err == nil {
		t.Fatal("blank evidence accepted")
	}
	data = scenarioRunData()
	data["finding_ids"] = []string{"foreign"}
	st.Findings = []domain.Finding{{Meta: domain.Meta{ID: "foreign", ProductID: "other"}}}
	if err := scenarioApply(&st, "record_scenario_run", "foreign", data); err == nil {
		t.Fatal("foreign finding accepted")
	}
}
