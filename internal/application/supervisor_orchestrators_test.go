package application

import (
	"context"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func TestListSupervisorOrchestratorsUsesCallerScopeAndWildcardRegistrations(t *testing.T) {
	state := domain.EmptyState()
	state.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "product", Owner: "alice"}}
	state.ExecutionGroups = []domain.ExecutionGroup{
		{ID: "owned", ProductID: "p", Owner: "alice", OrchestratorHarness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "group:0.1"}},
		{ID: "other-owner", ProductID: "p", Owner: "bob", OrchestratorHarness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "other:0.1"}},
	}
	state.UserOrchestrators = []domain.UserOrchestrator{
		{Meta: domain.Meta{ProductID: "p"}, UserID: "alice", Products: []string{"p"}, Available: false, Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "user:0.1"}},
		{Meta: domain.Meta{ProductID: "other-product"}, UserID: "alice", Products: []string{"p"}, Available: true, Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "wrong-product:0.1"}},
		{Meta: domain.Meta{}, UserID: "alice", Products: []string{"p"}, Available: true, Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "global:0.1"}},
		{Meta: domain.Meta{ProductID: "p"}, UserID: "bob", Products: []string{"p"}, Available: true, Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "bob:0.1"}},
	}
	service := New(&memoryStore{state: state})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/alice", Owner: "alice", Kind: "agent", Products: []string{"p"}, Permissions: []string{"read"}})
	result, err := service.Query(ctx, "list_supervisor_orchestrators", "p")
	if err != nil {
		t.Fatal(err)
	}
	items := result.(map[string]any)["items"].([]domain.ExecutionHarness)
	if len(items) != 3 || items[0].TmuxTarget != "group:0.1" || items[1].TmuxTarget != "user:0.1" || items[2].TmuxTarget != "global:0.1" {
		t.Fatalf("unexpected authorized orchestrators: %+v", items)
	}
	if _, err := service.Query(ctx, "list_supervisor_orchestrators", "other-product"); err == nil {
		t.Fatal("expected inaccessible product to be rejected")
	}
}
