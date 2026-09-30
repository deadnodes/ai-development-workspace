package application

import (
	"context"
	"encoding/json"
	"errors"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

func agentContext(permissions ...string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/trusted", Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: permissions})
}
func TestAuthorizationProjectionAndAttribution(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "create_product", ID: "secret", Data: map[string]any{"name": "secret-product"}})
	exec(t, s, domain.Command{Action: "create_feature", ID: "secret-f", ProductID: "secret", Data: map[string]any{"title": "secret-feature", "goal": "private"}})
	m.state.GitHubConnections = append(m.state.GitHubConnections, domain.GitHubConnection{Meta: domain.Meta{ID: "global-secret"}})
	ctx := agentContext("read", "write", "deploy")
	st, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("cross-product/global leak: %s", raw)
	}
	for _, read := range []func() error{
		func() error { _, e := s.Resume(ctx, "secret-f"); return e },
		func() error { _, e := s.ProjectContext(ctx, "secret"); return e },
		func() error { _, e := s.ExportWorkspace(ctx, "secret"); return e },
		func() error { _, e := s.Query(ctx, "get_feature_graph", "secret-f"); return e },
	} {
		if read() == nil {
			t.Fatal("cross-product read allowed")
		}
	}
	_, err = s.Execute(ctx, domain.Command{Action: "record_progress", Actor: "human/admin", IntegrationID: "i", Data: map[string]any{"body": "Progress", "remaining": []string{"tests"}}})
	if err != nil {
		t.Fatal(err)
	}
	if m.state.Events[len(m.state.Events)-1].Actor != "agent/trusted" {
		t.Fatal("spoofed command actor")
	}
	_, err = s.UpsertKnowledgeNode(ctx, "p", "human/admin", domain.KnowledgeNode{Meta: domain.Meta{ID: "node"}, Kind: "contract", Title: "Scope", Content: "Scoped"})
	if err != nil {
		t.Fatal(err)
	}
	if m.state.ProjectKnowledge[0].Nodes[0].Actor != "agent/trusted" {
		t.Fatal("spoofed named operation actor")
	}
}
func TestAuthorizationFailsClosedForPrivilegesAndReferences(t *testing.T) {
	s, m := fixture(t)
	m.state.Repositories = append(m.state.Repositories, domain.Repository{Meta: domain.Meta{ID: "other-repo", ProductID: "secret"}})
	m.state.Environments = append(m.state.Environments, domain.Environment{Meta: domain.Meta{ID: "prod", ProductID: "p"}})
	m.state.EnvironmentBindings = append(m.state.EnvironmentBindings, domain.EnvironmentBinding{Meta: domain.Meta{ID: "prod-binding", ProductID: "p"}, EnvironmentID: "prod", Purpose: "PROD"})
	m.state.Compositions = append(m.state.Compositions, domain.Composition{Meta: domain.Meta{ID: "prod-comp", ProductID: "p"}, EnvironmentID: "prod"})
	ctx := agentContext("read", "write", "deploy", "admin")
	for _, c := range []domain.Command{
		{Action: "grant_connection", ProductID: "p"},
		{Action: "create_github_connection", ProductID: "p"},
		{Action: "prepare_release_candidate", ProductID: "p", Data: map[string]any{"approve_main_update": true}},
		{Action: "promote_release_candidate", ProductID: "p"},
		{Action: "record_release", ProductID: "p"},
		{Action: "create_feature", ProductID: "secret"},
		{Action: "create_integration", FeatureID: "f", Data: map[string]any{"branches": []any{map[string]any{"repository_id": "other-repo"}}}},
		{Action: "record_decision", FeatureID: "f", Data: map[string]any{"nested": map[string]any{"repository_id": "other-repo"}}},
		{Action: "deploy_integration", IntegrationID: "i", Data: map[string]any{"environment_id": "prod"}},
		{Action: "reconcile_composition", ID: "prod-comp"},
		{Action: "transition_integration", IntegrationID: "i", Data: map[string]any{"status": "released"}},
	} {
		if _, err := s.Execute(ctx, c); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("%s: want forbidden, got %v", c.Action, err)
		}
	}
	for _, op := range []func() error{
		func() error { _, e := s.CreateBackup(ctx, "admin"); return e },
		func() error { _, e := s.RestoreBackup(ctx, "admin", "", ""); return e },
		func() error { _, e := s.ImportWorkspace(ctx, "admin", WorkspaceConfiguration{}); return e },
		func() error { _, e := s.ScanWorkspace(ctx, "p", "admin"); return e },
		func() error { _, e := s.Query(ctx, "discover_repositories", "global"); return e },
		func() error { _, e := s.Query(ctx, "test_connection", "global"); return e },
		func() error {
			_, e := s.PlanLocalGitSync(agentContext("read"), LocalGitRequest{ProductID: "p", Actor: "admin"})
			return e
		},
		func() error {
			_, e := s.RecordLocalGitSync(agentContext("read"), LocalGitRequest{ProductID: "p", Actor: "admin"})
			return e
		},
		func() error {
			_, e := s.SyncReview(agentContext("read"), "admin", "i", "other-repo", 1, nil, false)
			return e
		},
	} {
		if err := op(); !errors.Is(err, auth.ErrForbidden) {
			t.Errorf("want forbidden, got %v", err)
		}
	}
	// Claiming admin role cannot turn an agent into an instance administrator.
	elevated := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent", Kind: "agent", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin", "read", "write"}})
	if _, err := s.CreateBackup(elevated, "spoof"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("agent role escalation", err)
	}
	human := auth.WithPrincipal(context.Background(), auth.Principal{ID: "human/admin", Kind: "human", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}})
	if _, err := s.CreateBackup(human, "spoof"); err != nil {
		t.Fatal("explicit instance admin denied", err)
	}
}

func TestBootstrapAdministrationDoesNotGrantReleaseAuthority(t *testing.T) {
	s, _ := fixture(t)
	bootstrap := auth.WithPrincipal(context.Background(), auth.Principal{ID: "service/bootstrap", Kind: "service", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}})
	if _, err := s.CreateBackup(bootstrap, "spoof"); err != nil {
		t.Fatal("configured bootstrap admin denied", err)
	}
	for _, action := range []string{"prepare_release_candidate", "promote_release_candidate", "plan_release", "record_release"} {
		if _, err := s.Execute(bootstrap, domain.Command{Action: action, ProductID: "p"}); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("bootstrap release approval %s: %v", action, err)
		}
	}
}
