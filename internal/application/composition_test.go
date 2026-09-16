package application

import (
	"context"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

var shaBase = strings.Repeat("a", 40)
var shaHead = strings.Repeat("b", 40)

func compositionFixture(t *testing.T) (*Service, *memoryStore) {
	t.Helper()
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "create_repository", ID: "repo", ProductID: "p", Data: map[string]any{"name": "repo", "url": "https://example.test/repo"}})
	exec(t, s, domain.Command{Action: "create_application", ID: "app", ProductID: "p", Data: map[string]any{"name": "API", "repository_id": "repo"}})
	exec(t, s, domain.Command{Action: "create_environment", ID: "env", ProductID: "p", Data: map[string]any{"name": "DEV", "cluster": "local", "namespace": "dev", "runtime": map[string]any{"status": "unknown"}}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "revision", IntegrationID: "i", Data: revisionData(shaHead)})
	return s, m
}
func revisionData(head string) map[string]any {
	return map[string]any{"repository_id": "repo", "branch": "feature/work", "base_commit": shaBase, "head_commit": head, "commits": []string{head}}
}
func component(app string, revisions ...string) domain.CompositionComponent {
	return domain.CompositionComponent{ApplicationID: app, BaseRef: "main", BaseCommit: shaBase, TargetBranch: "generated/dev", RevisionIDs: revisions}
}
func planData(components ...domain.CompositionComponent) map[string]any {
	return map[string]any{"environment_id": "env", "name": "DEV plan", "components": components}
}
func TestCompositionSnapshotsSurviveNewRevisionAndSelectOnlyDesired(t *testing.T) {
	s, m := compositionFixture(t)
	exec(t, s, domain.Command{Action: "plan_composition", ID: "plan", ProductID: "p", Data: planData(component("app", "revision"))})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "new-revision", IntegrationID: "i", Data: revisionData(strings.Repeat("c", 40))})
	exec(t, s, domain.Command{Action: "select_composition", ID: "plan"})
	v := m.state.Compositions[0]
	if v.RevisionSnapshots[0].HeadCommit != shaHead || len(m.state.IntegrationRevisions) != 2 {
		t.Fatal("revision history overwritten")
	}
	env := m.state.Environments[0]
	if env.DesiredCompositionID != "plan" || env.Runtime["status"] != "unknown" || len(env.Reconciled) != 0 || v.Status != "planned" {
		t.Fatal("selection changed observed state")
	}
	reject(t, s, domain.Command{Action: "record_integration_revision", ID: "revision", IntegrationID: "i", Data: revisionData(strings.Repeat("d", 40))})
	ctx, e := s.Resume(context.Background(), "f")
	if e != nil {
		t.Fatal(e)
	}
	if len(ctx.(map[string]any)["compositions"].([]domain.Composition)) != 1 {
		t.Fatal("composition missing from feature context")
	}
}
func TestCompositionProductAndRepositoryIsolation(t *testing.T) {
	s, _ := compositionFixture(t)
	exec(t, s, domain.Command{Action: "create_product", ID: "other", Data: map[string]any{"name": "Other"}})
	reject(t, s, domain.Command{Action: "create_application", ProductID: "other", Data: map[string]any{"name": "wrong", "repository_id": "repo"}})
	reject(t, s, domain.Command{Action: "plan_composition", ProductID: "other", Data: planData(component("app", "revision"))})
	exec(t, s, domain.Command{Action: "create_repository", ID: "second-repo", ProductID: "p", Data: map[string]any{"name": "second", "url": "https://example.test/second"}})
	exec(t, s, domain.Command{Action: "create_application", ID: "second-app", ProductID: "p", Data: map[string]any{"name": "second", "repository_id": "second-repo"}})
	reject(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(component("second-app", "revision"))})
}
func TestMonorepoApplicationsShareOneBranchPlan(t *testing.T) {
	s, _ := compositionFixture(t)
	exec(t, s, domain.Command{Action: "create_application", ID: "web", ProductID: "p", Data: map[string]any{"name": "Web", "repository_id": "repo", "path": "apps/web"}})
	other := component("web", "revision")
	other.TargetBranch = "generated/other"
	reject(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(component("app", "revision"), other)})
	exec(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(component("app", "revision"), component("web", "revision"))})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "new", IntegrationID: "i", Data: revisionData(strings.Repeat("c", 40))})
	reject(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(component("app", "revision", "new"))})
}
func TestEnvironmentNamesAndTargetChangeInvalidateSelection(t *testing.T) {
	s, m := compositionFixture(t)
	reject(t, s, domain.Command{Action: "create_environment", ProductID: "p", Data: map[string]any{"name": " dev "}})
	exec(t, s, domain.Command{Action: "create_product", ID: "other", Data: map[string]any{"name": "Other"}})
	exec(t, s, domain.Command{Action: "create_environment", ProductID: "other", Data: map[string]any{"name": "dev"}})
	exec(t, s, domain.Command{Action: "plan_composition", ID: "plan", ProductID: "p", Data: planData(component("app", "revision"))})
	exec(t, s, domain.Command{Action: "select_composition", ID: "plan"})
	exec(t, s, domain.Command{Action: "update_environment", ID: "env", Data: map[string]any{"namespace": "new-dev"}})
	if m.state.Environments[0].DesiredCompositionID != "" {
		t.Fatal("stale desired composition retained")
	}
	reject(t, s, domain.Command{Action: "select_composition", ID: "plan"})
	reject(t, s, domain.Command{Action: "update_environment", ID: "env", Data: map[string]any{"desired_composition_id": "plan"}})
}
func TestCompositionDependencyAndSHAValidation(t *testing.T) {
	s, _ := compositionFixture(t)
	bad := revisionData("abc123")
	reject(t, s, domain.Command{Action: "record_integration_revision", IntegrationID: "i", Data: bad})
	exec(t, s, domain.Command{Action: "create_integration", ID: "dependent", FeatureID: "f", Data: map[string]any{"title": "Dependent", "objective": "Ship", "dependencies": []string{"i"}}})
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "dependent-revision", IntegrationID: "dependent", Data: revisionData(strings.Repeat("c", 40))})
	reject(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(component("app", "dependent-revision"))})
	exec(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(component("app", "revision", "dependent-revision"))})
	badComponent := component("app", "revision")
	badComponent.TargetBranch = "main"
	reject(t, s, domain.Command{Action: "plan_composition", ProductID: "p", Data: planData(badComponent)})
}

func TestSafeBranchRejectsInvalidGitReferenceNames(t *testing.T) {
	for _, ref := range []string{"", "@", "-option", "/main", "main/", "generated//dev", "generated/.hidden/x", "generated/x.lock/y", "generated/dev.lock", "generated/d..ev", "generated/dev.", "generated/a@{b", "generated/a\\b", "generated/a b", "generated/a\x00b", "generated/a\x01b", "generated/a\x1fb", "generated/a\x7fb"} {
		t.Run(ref, func(t *testing.T) {
			if safeBranch(ref) {
				t.Fatalf("accepted invalid reference %q", ref)
			}
		})
	}
	for _, ref := range []string{"main", "feature/payments-v2", "generated/dev", "generated/2026.09.16", "feature/user@example"} {
		if !safeBranch(ref) {
			t.Errorf("rejected valid reference %q", ref)
		}
	}
}
