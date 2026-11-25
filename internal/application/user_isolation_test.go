package application

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func TestClaimExecutionIsAssignedToPrincipalOwner(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := New(store)
	for _, c := range []domain.Command{
		{Action: "create_product", ID: "p", Data: map[string]any{"name": "P"}},
		{Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "F", "goal": "G"}},
		{Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "I", "objective": "O"}},
	} {
		c.Actor = "local"
		if _, err = s.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	principal := func(id, owner string) context.Context {
		return auth.WithPrincipal(ctx, auth.Principal{ID: id, Owner: owner, Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	}
	claim := func(c context.Context) error {
		_, e := s.Execute(c, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
		return e
	}
	if err = claim(principal("agent/a1", "human/A")); err != nil {
		t.Fatal(err)
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Integrations[0].Assignee != "human/A" {
		t.Fatalf("assignee=%q", st.Integrations[0].Assignee)
	}
	page, err := s.StatePage(ctx, domain.StatePageRequest{FeatureID: "f"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Assignee != "human/A" {
		t.Fatalf("state card assignee: page=%+v err=%v", page, err)
	}
	resumed, err := s.ResumeSummary(ctx, "f")
	if err != nil || resumed.(map[string]any)["integrations"].([]map[string]any)[0]["assignee"] != "human/A" {
		t.Fatalf("resume assignee: %v, %v", resumed, err)
	}
	if err = claim(principal("agent/b1", "human/B")); err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") || !strings.Contains(err.Error(), "human/A") {
		t.Fatalf("expected NOT_ASSIGNEE naming human/A, got %v", err)
	}
	if err = claim(principal("agent/orphan", "")); err == nil || !strings.Contains(err.Error(), "AGENT_OWNER_UNKNOWN") {
		t.Fatalf("expected AGENT_OWNER_UNKNOWN, got %v", err)
	}
	// A second agent for the same human can resume an expired execution.
	if err = claim(principal("agent/a2", "human/A")); err == nil || !strings.Contains(err.Error(), "LEASE_HELD") {
		t.Fatalf("same-owner claim should retain normal lease rules, got %v", err)
	}
}

func TestAssignIntegrationAuthorizationAndAdminForce(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := New(store)
	for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "P"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "F", "goal": "G"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "I", "objective": "O"}}} {
		c.Actor = "local"
		if _, err = s.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	a := auth.WithPrincipal(ctx, auth.Principal{ID: "human/A", Kind: "human", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	if _, err = s.Execute(a, domain.Command{Action: "assign_integration", IntegrationID: "i", Data: map[string]any{"assignee": "human/A"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(auth.WithPrincipal(ctx, auth.Principal{ID: "human/B", Kind: "human", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}}), domain.Command{Action: "assign_integration", IntegrationID: "i", Data: map[string]any{"assignee": "human/B"}}); err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
		t.Fatalf("expected assignment denial, got %v", err)
	}
	if _, err = s.Execute(a, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}}); err != nil {
		t.Fatal(err)
	}
	admin := auth.WithPrincipal(ctx, auth.Principal{ID: "human/admin", Kind: "human", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}})
	if _, err = s.Execute(admin, domain.Command{Action: "assign_integration", IntegrationID: "i", Data: map[string]any{"assignee": "human/B"}}); err == nil || !strings.Contains(err.Error(), "ACTIVE_EXECUTION") {
		t.Fatalf("expected active execution conflict, got %v", err)
	}
	if _, err = s.Execute(admin, domain.Command{Action: "assign_integration", IntegrationID: "i", Data: map[string]any{"assignee": "human/B", "force": true}}); err != nil {
		t.Fatal(err)
	}
}
