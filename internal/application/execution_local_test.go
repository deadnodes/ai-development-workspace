package application

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func TestCreateExecutionGroupWithLocalStore(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := New(store)
	run := func(c domain.Command) any {
		t.Helper()
		c.Actor = "agent"
		v, err := s.Execute(ctx, c)
		if err != nil {
			t.Fatalf("%s: %v", c.Action, err)
		}
		return v
	}
	run(domain.Command{Action: "create_product", ID: "p", Data: map[string]any{"name": "P"}})
	run(domain.Command{Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "F", "goal": "G"}})
	for i := 1; i <= 4; i++ {
		run(domain.Command{Action: "create_integration", ID: string(rune('a' + i)), FeatureID: "f", Data: map[string]any{"title": "I", "objective": "O"}})
	}
	run(domain.Command{Action: "transition_integration", IntegrationID: "b", Data: map[string]any{"status": "working"}})
	run(domain.Command{Action: "transition_integration", IntegrationID: "b", Data: map[string]any{"status": "ready"}})
	run(domain.Command{Action: "create_execution_group", ProductID: "p", Data: map[string]any{"name": "G", "orchestrator_id": "orch", "integration_ids": []string{"b", "c", "d", "e"}}})
}

func runExecutionLifecycle(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	s := New(store)
	run := func(c domain.Command) any {
		t.Helper()
		c.Actor = "agent/lifecycle"
		v, err := s.Execute(ctx, c)
		if err != nil {
			t.Fatalf("%s: %v", c.Action, err)
		}
		return v
	}
	run(domain.Command{Action: "create_product", ID: "life-p", Data: map[string]any{"name": "Lifecycle"}})
	run(domain.Command{Action: "create_feature", ID: "life-f", ProductID: "life-p", Data: map[string]any{"title": "Lifecycle", "goal": "complete"}})
	run(domain.Command{Action: "create_integration", ID: "life-i", FeatureID: "life-f", Data: map[string]any{"title": "Member", "objective": "complete"}})
	g := run(domain.Command{Action: "create_execution_group", ProductID: "life-p", Data: map[string]any{"name": "group", "orchestrator_id": "orch", "orchestrator_harness": map[string]any{"kind": "codex", "session_ref": "session-orch-1234", "tmux_target": "orch"}, "integration_ids": []string{"life-i"}}}).(domain.ExecutionGroup)
	if g.OrchestratorHarness.TmuxTarget != "orch" {
		t.Fatalf("group harness: %+v", g)
	}
	g = run(domain.Command{Action: "set_execution_group_harness", ProductID: "life-p", Data: map[string]any{"group_id": g.ID, "harness": map[string]any{"kind": "codex", "session_ref": "session-orch-1234", "tmux_target": "orch-restarted"}}}).(domain.ExecutionGroup)
	if g.OrchestratorHarness.TmuxTarget != "orch-restarted" {
		t.Fatalf("re-registered harness: %+v", g.OrchestratorHarness)
	}
	run(domain.Command{Action: "claim_execution", ProductID: "life-p", IntegrationID: "life-i", Data: map[string]any{"orchestrator_id": "orch", "agent_id": "agent", "harness": map[string]any{"kind": "codex", "session_ref": "session-orch-1234", "tmux_target": "orch"}}})
	run(domain.Command{Action: "heartbeat_execution", ProductID: "life-p", IntegrationID: "life-i", Data: map[string]any{"fence": float64(1)}})
	run(domain.Command{Action: "report_turn_end", ProductID: "life-p", Data: map[string]any{"harness_kind": "codex", "session_ref": "session-orch-1234", "turn_id": "turn-1"}})
	if err := store.Update(ctx, func(st *domain.State) error {
		e := executionFor(st, "life-i")
		past := time.Now().UTC().Add(-time.Minute)
		e.TurnEndAt = &past
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileExecutionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	findEvent := func(typ string) domain.ExecutionEvent {
		t.Helper()
		for _, ev := range st.ExecutionEvents {
			if ev.Type == typ && ev.Resolution.ResolvedAt == nil {
				return ev
			}
		}
		t.Fatalf("missing unresolved %s event: %+v", typ, st.ExecutionEvents)
		return domain.ExecutionEvent{}
	}
	continuation := findEvent("execution_continuation_required")
	if continuation.WakeTarget.Source != "group_orchestrator" || continuation.WakeTarget.TmuxTarget != "orch-restarted" {
		t.Fatalf("continuation wake: %+v", continuation.WakeTarget)
	}
	ackEvent := func(ev domain.ExecutionEvent, outcome string) {
		t.Helper()
		delivery := run(domain.Command{Action: "claim_execution_event_delivery", ProductID: "life-p", Data: map[string]any{"event_id": ev.ID, "supervisor_id": "sup"}}).(string)
		run(domain.Command{Action: "ack_execution_event", ProductID: "life-p", Data: map[string]any{"event_id": ev.ID, "delivery_id": delivery, "supervisor_id": "sup", "outcome": outcome}})
	}
	ackEvent(continuation, "delivered")
	// The orchestrator turn ended, so explicitly claim the unfinished member
	// again before exercising lease expiry on a live executor.
	run(domain.Command{Action: "claim_execution", ProductID: "life-p", IntegrationID: "life-i", Data: map[string]any{"orchestrator_id": "orch", "agent_id": "agent2", "harness": map[string]any{"kind": "codex", "session_ref": "session-orch-1234", "tmux_target": "orch"}}})
	if err = store.Update(ctx, func(st *domain.State) error {
		past := time.Now().UTC().Add(-time.Second)
		executionFor(st, "life-i").LeaseUntil = &past
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileExecutionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lost := findEventFrom(st, "execution_lost")
	ackEvent(lost, "delivered")
	run(domain.Command{Action: "takeover_execution", ProductID: "life-p", IntegrationID: "life-i", Data: map[string]any{"orchestrator_id": "orch", "agent_id": "agent2", "fence": float64(2), "state_check_reference": lost.ID, "harness": map[string]any{"kind": "codex", "session_ref": "session-orch-1234", "tmux_target": "orch"}}})
	if _, err = s.Execute(ctx, domain.Command{Action: "record_progress", Actor: "agent/old", ProductID: "life-p", FeatureID: "life-f", IntegrationID: "life-i", Data: map[string]any{"title": "stale", "execution_fence": float64(2)}}); err == nil || !strings.Contains(err.Error(), "STALE_FENCE") {
		t.Fatalf("old fence progress error=%v", err)
	}
	run(domain.Command{Action: "transition_integration", ProductID: "life-p", IntegrationID: "life-i", Data: map[string]any{"status": "working", "execution_fence": float64(3)}})
	run(domain.Command{Action: "transition_integration", ProductID: "life-p", IntegrationID: "life-i", Data: map[string]any{"status": "ready", "execution_fence": float64(3)}})
	if err = s.ReconcileExecutionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ex := executionFor(&st, "life-i"); ex.State != "completed" {
		t.Fatalf("execution=%+v", ex)
	}
	for _, ev := range st.ExecutionEvents {
		if ev.IntegrationID == "life-i" && ev.Resolution.ResolvedAt == nil {
			t.Fatalf("unresolved completion event: %+v", ev)
		}
	}
	count := len(st.ExecutionEvents)
	if err = s.ReconcileExecutionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ExecutionEvents) != count {
		t.Fatalf("completed member received new event: %d -> %d", count, len(st.ExecutionEvents))
	}
}

func findEventFrom(st domain.State, typ string) domain.ExecutionEvent {
	for _, ev := range st.ExecutionEvents {
		if ev.Type == typ && ev.Resolution.ResolvedAt == nil {
			return ev
		}
	}
	return domain.ExecutionEvent{}
}

func TestExecutionLifecycleLocalStore(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runExecutionLifecycle(t, store)
}
