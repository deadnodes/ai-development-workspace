package application

import (
	"context"
	"errors"
	"testing"

	"releasecontrol/internal/domain"
)

// This inventory is deliberately explicit: adding a product-scoped reader
// requires adding its projection here and exercising it against this fixture.
var privateProductReadMatrix = []string{
	"State", "StatePage", "ProjectContext", "Resume", "SearchContext",
	"ContextExcerpt", "GraphNeighbors", "get_integration_context",
	"get_execution_group", "list_executions", "list_execution_events",
	"get_attention_required", "get_feature_graph", "get_operation",
}

func TestPrivateProductReadMatrix(t *testing.T) {
	store := &memoryStore{state: domain.EmptyState()}
	service := New(store)
	owner := userCtx("A", "human", "", "member")
	for _, command := range []domain.Command{
		{Action: "create_product", ID: "S", Data: map[string]any{"name": "Shared"}},
		{Action: "create_product", ID: "P", Data: map[string]any{"name": "Private Secret", "visibility": "private"}},
		{Action: "create_feature", ID: "PF", ProductID: "P", Data: map[string]any{"title": "Private Title", "goal": "private goal"}},
		{Action: "create_integration", ID: "PI", FeatureID: "PF", Data: map[string]any{"title": "Private Integration", "objective": "private objective"}},
		{Action: "add_product_member", ProductID: "P", Data: map[string]any{"user_id": "B"}},
	} {
		if _, err := service.Execute(owner, command); err != nil {
			t.Fatal(err)
		}
	}
	callers := []struct {
		name    string
		ctx     context.Context
		visible bool
	}{
		{"owner", owner, true}, {"agent owner", userCtx("a1", "agent", "A", "member"), true},
		{"member", userCtx("B", "human", "", "member"), true},
		{"outsider", userCtx("C", "human", "", "member"), false},
		{"agent outsider", userCtx("c1", "agent", "C", "member"), false},
		{"admin outsider", userCtx("D", "human", "", "admin"), false},
		{"bootstrap", userCtx("service/bootstrap", "service", "", "admin"), true},
		{"local", context.Background(), true},
	}
	for _, tc := range callers {
		t.Run(tc.name, func(t *testing.T) {
			st, err := service.State(tc.ctx)
			if err != nil {
				t.Fatal(err)
			}
			seen := false
			for _, p := range st.Products {
				if p.ID == "P" {
					seen = true
				}
			}
			if seen != tc.visible {
				t.Fatalf("private product visible=%v want %v", seen, tc.visible)
			}
			shared := false
			for _, p := range st.Products {
				if p.ID == "S" {
					shared = true
				}
			}
			if !shared {
				t.Fatal("shared Product disappeared")
			}
			for _, f := range st.Features {
				if f.ID == "PF" && !tc.visible {
					t.Fatal("private feature leaked")
				}
			}
			for _, i := range st.Integrations {
				if i.ID == "PI" && !tc.visible {
					t.Fatal("private integration leaked")
				}
			}
			if tc.visible {
				return
			}
			if _, err := service.Resume(tc.ctx, "PF"); !errors.Is(err, ErrNotFound) {
				t.Errorf("resume error=%v want NOT_FOUND", err)
			}
			if _, err := service.ProjectContext(tc.ctx, "P"); !errors.Is(err, ErrNotFound) {
				t.Errorf("project context error=%v want NOT_FOUND", err)
			}
			if _, err := service.Query(tc.ctx, "get_integration_context", "PI"); !errors.Is(err, ErrNotFound) {
				t.Errorf("integration context error=%v want NOT_FOUND", err)
			}
		})
	}
}

func TestPrivateProductWriteMatrix(t *testing.T) {
	store := &memoryStore{state: domain.EmptyState()}
	service := New(store)
	owner := userCtx("A", "human", "", "member")
	for _, c := range []domain.Command{{Action: "create_product", ID: "P", Data: map[string]any{"name": "Private", "visibility": "private"}}, {Action: "create_feature", ID: "PF", ProductID: "P", Data: map[string]any{"title": "Secret", "goal": "secret"}}, {Action: "create_integration", ID: "PI", FeatureID: "PF", Data: map[string]any{"title": "Secret", "objective": "secret"}}} {
		if _, err := service.Execute(owner, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		caller context.Context
	}{{"C", userCtx("C", "human", "", "member")}, {"c1", userCtx("c1", "agent", "C", "member")}, {"D", userCtx("D", "human", "", "admin")}} {
		for _, c := range []domain.Command{{Action: "create_feature", ProductID: "P", Data: map[string]any{"title": "probe", "goal": "probe"}}, {Action: "create_integration", FeatureID: "PF", Data: map[string]any{"title": "probe", "objective": "probe"}}, {Action: "record_progress", IntegrationID: "PI", Data: map[string]any{"completed": []string{"probe"}}}, {Action: "claim_execution", IntegrationID: "PI", Data: map[string]any{}}} {
			if _, err := service.Execute(tc.caller, c); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s: %s returned %v, want NOT_FOUND", tc.name, c.Action, err)
			}
		}
	}
}
