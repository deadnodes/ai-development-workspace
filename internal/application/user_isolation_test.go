package application

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestExecutionIsolationCoversLifecycleGroupsAndSupervisorEvents(t *testing.T) {
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
		{Action: "create_integration", ID: "i1", FeatureID: "f", Data: map[string]any{"title": "I1", "objective": "O"}},
		{Action: "create_integration", ID: "i2", FeatureID: "f", Data: map[string]any{"title": "I2", "objective": "O"}},
	} {
		c.Actor = "local"
		if _, err = s.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	principal := func(id, owner string) context.Context {
		return auth.WithPrincipal(ctx, auth.Principal{ID: id, Owner: owner, Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	}
	a1, b1 := principal("agent/a1", "human/A"), principal("agent/b1", "human/B")
	cmd := func(c context.Context, action, integrationID string, data map[string]any) error {
		_, e := s.Execute(c, domain.Command{Action: action, ProductID: "p", IntegrationID: integrationID, Data: data})
		return e
	}
	if err = cmd(a1, "claim_execution", "i1", map[string]any{"orchestrator_id": "oa", "agent_id": "a1"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		action string
		data   map[string]any
	}{{"claim_execution", map[string]any{"orchestrator_id": "ob", "agent_id": "b1"}}, {"takeover_execution", map[string]any{"orchestrator_id": "ob", "agent_id": "b1", "fence": float64(1), "state_check_reference": "ev"}}, {"heartbeat_execution", map[string]any{"fence": float64(1)}}, {"report_execution", map[string]any{"fence": float64(1), "state": "paused"}}, {"release_execution", map[string]any{"fence": float64(1), "outcome": "paused"}}, {"set_desired_state", map[string]any{"fence": float64(1), "desired_state": "released"}}} {
		if err = cmd(b1, tc.action, "i1", tc.data); err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
			t.Fatalf("%s should be forbidden for B, got %v", tc.action, err)
		}
	}
	for _, tc := range []struct {
		action string
		data   map[string]any
	}{{"record_progress", map[string]any{"title": "foreign", "completed": []string{"work"}}}, {"transition_integration", map[string]any{"status": "working"}}} {
		if err = cmd(b1, tc.action, "i1", tc.data); err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
			t.Fatalf("%s should be forbidden for B, got %v", tc.action, err)
		}
	}
	if err = store.Update(ctx, func(st *domain.State) error {
		st.Integrations[1].Assignee = "human/B"
		now := time.Now().UTC()
		st.IntegrationExecutions[0].State = "lost"
		st.ExecutionEvents = append(st.ExecutionEvents, domain.ExecutionEvent{ID: "ev", ProductID: "p", IntegrationID: "i1", Type: "execution_lost", CreatedAt: now, EscalationRequired: true, Delivery: domain.ExecutionEventDelivery{Attempts: 1, DeliveryID: "d", NextAttemptAt: now.Add(time.Hour)}, Acknowledgements: []domain.ExecutionEventAck{{DeliveryID: "d", Outcome: "delivered", At: now}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = cmd(principal("agent/a2", "human/A"), "takeover_execution", "i1", map[string]any{"orchestrator_id": "oa2", "agent_id": "a2", "fence": float64(1), "state_check_reference": "ev"}); err != nil {
		t.Fatalf("same human's second agent should take over lost work: %v", err)
	}
	if err = cmd(a1, "create_execution_group", "", map[string]any{"name": "mixed", "orchestrator_id": "oa", "integration_ids": []string{"i1", "i2"}}); err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
		t.Fatalf("mixed-owner group should fail, got %v", err)
	}
	if err = cmd(b1, "ack_execution_event", "", map[string]any{"event_id": "ev", "delivery_id": "d", "supervisor_id": "sup", "outcome": "delivered"}); err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
		t.Fatalf("event ACK should be forbidden for B, got %v", err)
	}
	listed, err := s.Query(b1, "list_execution_events", `{"product_id":"p"}`)
	if err != nil || len(listed.(map[string]any)["items"].([]domain.ExecutionEvent)) != 0 {
		t.Fatalf("B should not see A's supervisor event: %#v, %v", listed, err)
	}
	attention, err := s.Query(b1, "list_attention", "p")
	if err != nil {
		t.Fatal(err)
	}
	masked := false
	for _, item := range attention.([]map[string]any) {
		if item["execution_event_id"] == "ev" || item["integration_id"] == "i1" && item["reason"] == "execution" {
			t.Fatalf("B should not see A's execution attention: %#v", item)
		}
		if item["integration_id"] == "i1" {
			masked = item["code"] == "none" && item["text"] == "assigned to human/A"
		}
	}
	if !masked {
		t.Fatal("attention must mark A's Integration as unavailable work")
	}
	if err = cmd(a1, "create_execution_group", "", map[string]any{"name": "owned", "orchestrator_id": "oa", "integration_ids": []string{"i1"}}); err != nil {
		t.Fatal(err)
	}
	state, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	groupID := state.ExecutionGroups[len(state.ExecutionGroups)-1].ID
	group, err := s.Query(b1, "get_execution_group", groupID)
	if err != nil {
		t.Fatal(err)
	}
	member := group.(map[string]any)["members"].([]map[string]any)[0]
	if got := member["next_action"].(map[string]string); got["code"] != "none" || got["text"] != "assigned to human/A" {
		t.Fatalf("foreign next_action = %#v", got)
	}
}
