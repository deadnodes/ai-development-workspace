package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func idleEvents(events []domain.ExecutionEvent, groupID string, openOnly bool) map[string]domain.ExecutionEvent {
	result := map[string]domain.ExecutionEvent{}
	for _, event := range events {
		if domain.IsGroupIdleEvent(event.Type) && event.WakeTarget.GroupID == groupID && (!openOnly || event.Resolution.ResolvedAt == nil) {
			result[event.Type] = event
		}
	}
	return result
}

func TestGroupIdleStoreMatrix(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		group := thirdReviewGroup(work)
		thirdReviewTurnEnd(work)
		ctx := context.Background()
		started := thirdReviewState(t, store).ExecutionGroups[0]
		if started.TurnEndAt == nil || started.TurnEndID == "" {
			t.Fatal("group turn end did not persist")
		}
		change := func(update func(*domain.State)) {
			t.Helper()
			if err := store.Update(ctx, func(state *domain.State) error { update(state); return nil }); err != nil {
				t.Fatal(err)
			}
		}
		sweep := func() map[string]domain.ExecutionEvent {
			t.Helper()
			if err := work.service.ReconcileExecutionsOnce(ctx); err != nil {
				t.Fatal(err)
			}
			return idleEvents(thirdReviewState(t, store).ExecutionEvents, group.ID, true)
		}
		policy := map[string]any{"idle_after_seconds": 10, "wait_timeout_seconds": 120, "owner_blocked_interval_seconds": 180}
		_, err := work.apply(work.bob, "set_product_idle_policy", "p", "", "", "", policy)
		if !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("foreign policy change: %v", err)
		}
		_, err = work.apply(work.owner, "set_product_idle_policy", "p", "", "", "", map[string]any{"idle_after_seconds": -1})
		expectWorkError(t, err, "thresholds")
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", policy)
		work.run(work.bob, "create_integration", "p", "f", "", "foreign", map[string]any{"title": "Foreign", "objective": "Ship"})
		work.run(work.bobAgent, "claim_execution", "p", "", "foreign", "", work.claimData("foreign-pane"))
		old := time.Now().UTC().Add(-2 * time.Minute)
		change(func(state *domain.State) {
			stored := &state.ExecutionGroups[0]
			stored.TurnEndAt, stored.CreatedAt = &old, old
			stored.MemberIntegrationIDs = append(stored.MemberIntegrationIDs, "foreign")
			stored.Revisions[0].CreatedAt = old
			stored.Revisions[0].MemberIntegrationIDs = stored.MemberIntegrationIDs
			execution := executionFor(state, "i")
			execution.LastHeartbeat, execution.LastProgress = &old, &old
		})
		events := sweep()
		if len(events) != 3 {
			t.Fatalf("idle conditions: %#v", events)
		}
		for _, kind := range []string{domain.EventGroupIdle, domain.EventGroupWaitTimeout, domain.EventGroupMemberActionable} {
			event := events[kind]
			if event.ConditionGeneration != 1 || event.WakeTarget != groupWakeTarget(group) || len(event.GroupIdle.Members) != 1 || event.GroupIdle.Members[0].IntegrationID != "i" || event.GroupIdle.Members[0].NextAction != "claim" {
				t.Fatalf("invalid %s event: %#v", kind, event)
			}
			payload, _ := json.Marshal(event.GroupIdle)
			if strings.Contains(string(payload), "Slice") || strings.Contains(string(payload), "Foreign") {
				t.Fatalf("unexpected wake payload: %s", payload)
			}
			if again := sweep()[kind]; again.ID != event.ID {
				t.Fatalf("duplicate %s in one episode", kind)
			}
		}
		view := work.query(work.agent, "get_execution_group", group.ID)
		if evaluation := view["idle_evaluation"].(groupIdleEvaluation); evaluation.Pending != 1 || !evaluation.Checks[domain.EventGroupIdle].Armed || evaluation.NextDueAt == nil || evaluation.Policy.IdleAfterSeconds != 10 {
			t.Fatalf("bad idle evaluation: %#v", evaluation)
		}
		if members := view["members"].([]map[string]any); len(members) != 1 {
			t.Fatalf("foreign group member exposed: %#v", members)
		}
		_, err = work.service.Query(work.bobAgent, "get_execution_group", group.ID)
		expectWorkError(t, err, "NOT_ASSIGNEE")
		otherView := work.query(work.bobAgent, "list_execution_events", "p")
		if got := idleEvents(otherView["items"].([]domain.ExecutionEvent), group.ID, false); len(got) != 0 {
			t.Fatalf("idle events visible to another owner: %#v", got)
		}
		event := events[domain.EventGroupIdle]
		claim := map[string]any{"event_id": event.ID, "supervisor_id": "sup"}
		_, err = work.apply(work.bobAgent, "claim_execution_event_delivery", "p", "", "", "", claim)
		expectWorkError(t, err, "not found")
		for attempt := 0; attempt < 5; attempt++ {
			delivery := work.run(work.agent, "claim_execution_event_delivery", "p", "", "", "", claim)
			ack := work.run(work.agent, "ack_execution_event", "p", "", "", "", map[string]any{"event_id": event.ID, "delivery_id": delivery, "supervisor_id": "sup", "outcome": "skipped_active"}).(domain.ExecutionEvent)
			if ack.EscalationRequired || len(ack.Acknowledgements) != attempt+1 || ack.Delivery.NextAttemptAt.Sub(*ack.Delivery.AcknowledgedAt) != 10*time.Second {
				t.Fatalf("incorrect active deferral: %#v", ack)
			}
			change(func(state *domain.State) {
				for index := range state.ExecutionEvents {
					if state.ExecutionEvents[index].ID == event.ID {
						state.ExecutionEvents[index].Delivery.NextAttemptAt = old
					}
				}
			})
		}
		work.run(work.agent, "report_turn_start", "p", "", "", "", map[string]any{"harness_kind": "codex", "tmux_target": "alice-group", "turn_id": "new-turn"})
		if fresh := sweep(); len(fresh) != 0 {
			t.Fatalf("turn start must clear events: %#v", fresh)
		}
		change(func(state *domain.State) {
			state.ExecutionGroups[0].TurnStartAt = &old
			older := old.Add(-time.Minute)
			state.ExecutionGroups[0].TurnEndAt = &older
		})
		if waiting := sweep(); len(waiting) != 1 || waiting[domain.EventGroupWaitTimeout].ConditionGeneration != 2 {
			t.Fatalf("stuck turn should only raise next wait generation: %#v", waiting)
		}
		now := time.Now().UTC()
		change(func(state *domain.State) {
			execution := executionFor(state, "i")
			execution.State, execution.LastHeartbeat = "running", &now
		})
		if fresh := sweep(); len(fresh) != 0 {
			t.Fatalf("fresh member heartbeat must clear all idle wakes: %#v", fresh)
		}
		change(func(state *domain.State) {
			execution := executionFor(state, "i")
			execution.LastHeartbeat, execution.LastProgress = &old, &now
		})
		if fresh := sweep(); len(fresh) != 0 {
			t.Fatalf("ongoing progress must suppress wakes: %#v", fresh)
		}
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", map[string]any{"idle_after_seconds": 0, "wait_timeout_seconds": 0, "owner_blocked_interval_seconds": 0})
		change(func(state *domain.State) { executionFor(state, "i").LastProgress = &old })
		if disabled := sweep(); len(disabled) != 0 {
			t.Fatalf("disabled idle policy produced wakes: %#v", disabled)
		}
	})
}

func TestGroupIdleOwnerReminderAndAddedMemberStoreMatrix(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		group := thirdReviewGroup(work)
		thirdReviewTurnEnd(work)
		question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", map[string]any{"idle_after_seconds": 10, "wait_timeout_seconds": 20, "owner_blocked_interval_seconds": 60})
		ctx := context.Background()
		sweepAt := func(now time.Time) map[string]domain.ExecutionEvent {
			t.Helper()
			if err := store.Update(ctx, func(state *domain.State) error { reconcileGroupIdle(state, now); return nil }); err != nil {
				t.Fatal(err)
			}
			return idleEvents(thirdReviewState(t, store).ExecutionEvents, group.ID, true)
		}
		if before := sweepAt(time.Now().UTC()); len(before) != 0 {
			t.Fatalf("reminder too early: %#v", before)
		}
		old := time.Now().UTC().Add(-2 * time.Minute)
		if err := store.Update(ctx, func(state *domain.State) error {
			group := &state.ExecutionGroups[0]
			group.CreatedAt, group.TurnEndAt, group.HarnessChangedAt = old, &old, &old
			for index := range group.Revisions {
				group.Revisions[index].CreatedAt = old
			}
			if execution := executionFor(state, "i"); execution != nil {
				execution.LastProgress, execution.LastHeartbeat = &old, &old
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		events := sweepAt(time.Now().UTC())
		reminder := events[domain.EventGroupOwnerBlocked]
		if len(events) != 1 || reminder.ID == "" {
			t.Fatalf("human-blocked work must only remind the owner: %#v", events)
		}
		delivery := work.run(work.agent, "claim_execution_event_delivery", "p", "", "", "", map[string]any{"event_id": reminder.ID, "supervisor_id": "sup"})
		ack := work.run(work.agent, "ack_execution_event", "p", "", "", "", map[string]any{"event_id": reminder.ID, "delivery_id": delivery, "supervisor_id": "sup", "outcome": "delivered"}).(domain.ExecutionEvent)
		if ack.Delivery.NextAttemptAt.Sub(*ack.Delivery.AcknowledgedAt) != time.Minute {
			t.Fatalf("incorrect reminder cooldown: %#v", ack.Delivery)
		}
		if err := store.Update(ctx, func(state *domain.State) error {
			activity := ack.Delivery.AcknowledgedAt.Add(-30 * time.Second)
			state.ExecutionGroups[0].TurnEndAt = &activity
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if events := sweepAt(ack.Delivery.NextAttemptAt.Add(-time.Second)); len(events) != 0 {
			t.Fatalf("an intervening turn must not reset the reminder cooldown: %#v", events)
		}
		if events := sweepAt(ack.Delivery.NextAttemptAt); len(events) != 1 || events[domain.EventGroupOwnerBlocked].ConditionGeneration != 2 {
			t.Fatalf("new reminder missing after cooldown: %#v", events)
		}
		work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
		work.run(work.owner, "create_integration", "p", "f", "", "added", map[string]any{"title": "Added", "objective": "Ship"})
		work.run(work.agent, "add_execution_group_member", "p", "", "", "", map[string]any{"group_id": group.ID, "integration_id": "added"})
		if events := sweepAt(time.Now().UTC().Add(2 * time.Minute)); events[domain.EventGroupOwnerBlocked].ID != "" || events[domain.EventGroupMemberActionable].GroupIdle == nil || len(events[domain.EventGroupMemberActionable].GroupIdle.Members) != 2 {
			t.Fatalf("added/unassigned work should clear reminder and become actionable: %#v", events)
		}
	})
}
