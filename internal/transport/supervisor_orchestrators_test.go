package transport

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

type supervisorOrchestratorService struct{ state domain.State }

func (s supervisorOrchestratorService) State(context.Context) (domain.State, error) {
	return s.state, nil
}
func (supervisorOrchestratorService) Execute(context.Context, domain.Command) (any, error) {
	return nil, nil
}
func (supervisorOrchestratorService) Resume(context.Context, string) (any, error) { return nil, nil }
func (s supervisorOrchestratorService) Query(_ context.Context, name, productID string) (any, error) {
	if name != "list_supervisor_orchestrators" || productID != "p" {
		return nil, nil
	}
	items := make([]domain.ExecutionHarness, 0)
	for _, group := range s.state.ExecutionGroups {
		if group.ProductID == productID && group.ClosedAt == nil {
			items = append(items, group.OrchestratorHarness)
		}
	}
	for _, user := range s.state.UserOrchestrators {
		if user.UnregisteredAt == nil && (slices.Contains(user.Products, "*") || slices.Contains(user.Products, productID)) {
			items = append(items, user.Harness)
		}
	}
	return map[string]any{"items": items}, nil
}

func TestSupervisorOrchestratorListExcludesClosedGroupsAndWorkers(t *testing.T) {
	now := time.Now()
	state := domain.EmptyState()
	state.ExecutionGroups = []domain.ExecutionGroup{
		{ID: "group", ProductID: "p", OrchestratorHarness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "group:0.1"}},
		{ID: "closed", ProductID: "p", ClosedAt: &now, OrchestratorHarness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "closed:0.1"}},
		{ID: "other", ProductID: "other", OrchestratorHarness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "other:0.1"}},
	}
	state.UserOrchestrators = []domain.UserOrchestrator{
		{Products: []string{"p"}, Available: true, Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "user:0.1"}},
		{Products: []string{"*"}, Available: true, Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "wildcard:0.1"}},
		{Products: []string{"p"}, Available: false, UnregisteredAt: &now, Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "unregistered:0.1"}},
		{Products: []string{"p"}, Available: false, Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "idle-registered:0.1"}},
	}
	state.IntegrationExecutions = []domain.IntegrationExecution{{Harness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "worker:0.1"}}}
	handler := newServer(supervisorOrchestratorService{state: state}, Options{}, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "http://localhost/api/products/p/supervisor-orchestrators", nil))
	if recorder.Code != 200 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Items []domain.ExecutionHarness `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 4 || result.Items[0].TmuxTarget != "group:0.1" || result.Items[1].TmuxTarget != "user:0.1" || result.Items[2].TmuxTarget != "wildcard:0.1" || result.Items[3].TmuxTarget != "idle-registered:0.1" {
		t.Fatalf("unexpected orchestrators: %+v", result.Items)
	}
}
