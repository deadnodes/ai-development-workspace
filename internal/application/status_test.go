package application

import (
	"context"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func TestStatusFeatureCatalogAndCompletionGuard(t *testing.T) {
	s, m := fixture(t)
	for _, status := range []string{"custom", "ready", "completed", "archived", "blocked"} {
		reject(t, s, domain.Command{Action: "create_feature", ProductID: "p", Data: map[string]any{"title": "Invalid initial state", "goal": "Goal", "status": status}})
	}
	for _, status := range []string{"custom", "ready", "released", ""} {
		reject(t, s, domain.Command{Action: "update_feature", FeatureID: "f", Data: map[string]any{"status": status}})
	}
	reject(t, s, domain.Command{Action: "update_feature", FeatureID: "f", Data: map[string]any{"status": "completed"}})
	exec(t, s, domain.Command{Action: "create_feature", ID: "empty", ProductID: "p", Data: map[string]any{"title": "Empty", "goal": "Goal"}})
	reject(t, s, domain.Command{Action: "update_feature", FeatureID: "empty", Data: map[string]any{"status": "completed"}})
	exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "update_feature", FeatureID: "f", Data: map[string]any{"status": "completed"}})
	if feature(&m.state, "f").Status != "completed" {
		t.Fatal("valid completion not retained")
	}
	exec(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"remaining": []string{"new requirement"}}})
	if feature(&m.state, "f").Status != "active" || integration(&m.state, "i").Status != "working" {
		t.Fatal("editing ready work must reopen feature and integration")
	}
}

func TestStatusIntegrationTransitionsAreExplicit(t *testing.T) {
	s, m := fixture(t)
	for _, status := range []string{"ready", "released", "planned", "working", "custom"} {
		reject(t, s, domain.Command{Action: "create_integration", FeatureID: "f", Data: map[string]any{"title": "Injected state", "objective": "Goal", "status": status}})
	}
	for _, status := range []string{"ready", "released", "working"} {
		reject(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"status": status}})
	}
	reject(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	reject(t, s, domain.Command{Action: "transition_integration", IntegrationID: "i", Data: map[string]any{"status": "ready"}})
	reject(t, s, domain.Command{Action: "transition_integration", IntegrationID: "i", Data: map[string]any{"status": "custom"}})
	for _, status := range []string{"working", "implemented", "verifying", "ready"} {
		exec(t, s, domain.Command{Action: "transition_integration", IntegrationID: "i", Data: map[string]any{"status": status}})
		if integration(&m.state, "i").Status != status {
			t.Fatal("legal transition not applied")
		}
	}
	reject(t, s, domain.Command{Action: "transition_integration", IntegrationID: "i", Data: map[string]any{"status": "planned"}})
	reject(t, s, domain.Command{Action: "transition_integration", IntegrationID: "i", Data: map[string]any{"status": "released"}})
	// Only release execution sets this terminal value; a persisted released record
	// must reject subsequent generic command attempts to reopen or edit it.
	integration(&m.state, "i").Status = "released"
	for _, status := range []string{"planned", "working", "ready", "released"} {
		reject(t, s, domain.Command{Action: "transition_integration", IntegrationID: "i", Data: map[string]any{"status": status}})
	}
	reject(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
	reject(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"title": "Rewrite history"}})
}

func TestStatusServerOwnedFieldsCannotBeInjected(t *testing.T) {
	commands := []domain.Command{
		{Action: "record_finding", FeatureID: "f", Data: map[string]any{"title": "Issue", "severity": "major", "status": "open"}},
		{Action: "add_blocker", FeatureID: "f", Data: map[string]any{"body": "Wait", "status": "open"}},
		{Action: "create_gate", FeatureID: "f", Data: map[string]any{"title": "Verify", "reason": "Risk", "status": "pending"}},
		{Action: "plan_release", ProductID: "p", Data: map[string]any{"name": "Release", "integration_ids": []string{"i"}, "status": "planned"}},
		{Action: "start_integration", IntegrationID: "i", Data: map[string]any{"status": "working"}},
		{Action: "complete_integration", IntegrationID: "i", Data: map[string]any{"status": "ready"}},
	}
	for _, c := range commands {
		t.Run(c.Action, func(t *testing.T) {
			s, m := fixture(t)
			before := len(m.state.Events)
			c.Actor = "agent"
			_, err := s.Execute(context.Background(), c)
			if err == nil || !strings.Contains(err.Error(), "status is server-managed") {
				t.Fatalf("expected explicit server-owned status rejection, got %v", err)
			}
			if len(m.state.Events) != before {
				t.Fatal("invalid payload persisted an audit event")
			}
		})
	}
}

func TestStatusPullRequestsUseCatalog(t *testing.T) {
	s, _ := fixture(t)
	exec(t, s, domain.Command{Action: "create_repository", ID: "repo", ProductID: "p", Data: map[string]any{"name": "Source", "url": "https://example.test/source"}})
	for _, status := range []string{"", "custom", "ready", "APPROVED"} {
		reject(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"pull_requests": []domain.PullRequest{{RepositoryID: "repo", ID: "42", Status: status}}}})
	}
	exec(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"pull_requests": []domain.PullRequest{{RepositoryID: "repo", ID: "42", Status: "open"}}}})
}
