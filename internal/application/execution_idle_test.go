package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func idleTestPolicy(idle, wait, owner, stale int) map[string]any {
	return map[string]any{"idle_after_seconds": idle, "wait_timeout_seconds": wait, "owner_blocked_interval_seconds": owner, "member_stale_after_seconds": stale}
}

func TestAcceptedProgressKeepsWaitingAndUnclaimedMembersFresh(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		group := thirdReviewGroup(work)
		work.run(work.owner, "create_integration", "p", "f", "", "unclaimed", map[string]any{"title": "Unclaimed", "objective": "Ship"})
		work.run(work.agent, "add_execution_group_member", "p", "", "", "", map[string]any{"group_id": group.ID, "integration_id": "unclaimed"})
		execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("executor")).(domain.IntegrationExecution)
		if err := store.Update(context.Background(), func(state *domain.State) error {
			state.Builds = append(state.Builds, domain.Build{Meta: domain.Meta{ID: "ci", ProductID: "p"}, Status: "RUNNING"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		work.run(work.agent, "report_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence, "state": "waiting_external", "waiting_ref": "ci"})
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", idleTestPolicy(0, 60, 0, 60))
		old := time.Now().UTC().Add(-2 * time.Minute)
		if err := store.Update(context.Background(), func(state *domain.State) error {
			group := &state.ExecutionGroups[0]
			group.CreatedAt = old
			for index := range group.Revisions {
				group.Revisions[index].CreatedAt = old
			}
			waiting := executionFor(state, "i")
			waiting.LastHeartbeat, waiting.LastProgress = &old, &old
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		evaluate := func() groupIdleEvaluation {
			state := thirdReviewState(t, store)
			result, _ := evaluateGroupIdle(&state, state.ExecutionGroups[0], time.Now().UTC())
			return result
		}
		if len(evaluate().staleMembers) != 2 {
			t.Fatal("expected both members to be stale before progress")
		}
		waitingProgress := work.run(work.agent, "record_progress", "p", "f", "i", "", map[string]any{"title": "CI advanced", "execution_fence": execution.Fence}).(domain.Memory)
		result := evaluate()
		wait := result.Checks[domain.EventGroupWaitTimeout]
		if !result.GroupActivityAt.Equal(waitingProgress.UpdatedAt) || wait.NextDueAt == nil || !wait.NextDueAt.Equal(waitingProgress.UpdatedAt.Add(time.Minute)) || len(result.staleMembers) != 1 || result.staleMembers[0].IntegrationID != "unclaimed" {
			t.Fatalf("waiting progress did not refresh both clocks: %+v", result)
		}
		unclaimedProgress := work.run(work.owner, "record_progress", "p", "f", "unclaimed", "", map[string]any{"title": "Unclaimed progress"}).(domain.Memory)
		state := thirdReviewState(t, store)
		result = evaluate()
		if executionFor(&state, "unclaimed") != nil || !result.GroupActivityAt.Equal(unclaimedProgress.UpdatedAt) || len(result.staleMembers) != 0 {
			t.Fatalf("unclaimed progress did not refresh both clocks: %+v", result)
		}
	})
}

func TestExecutionDeliveryFeedByteBudget(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	now := time.Now().UTC()
	largeReason := strings.Repeat("&", domain.MaxExecutionEventPageBytes)
	events := []domain.ExecutionEvent{
		{ID: "large", ProductID: "p", IntegrationID: "i", CreatedAt: now, Reason: largeReason, Type: "execution_continuation_required"},
		{ID: "first", ProductID: "p", IntegrationID: "i", CreatedAt: now.Add(time.Second), Reason: strings.Repeat("&", 90000)},
		{ID: "later", ProductID: "p", IntegrationID: "i", CreatedAt: now.Add(2 * time.Second), Reason: strings.Repeat("&", 90000)},
	}
	work.store.state.ExecutionEvents = events
	cursor := ""
	for _, want := range [][]string{{"large", "first"}, {"later"}} {
		query, _ := json.Marshal(map[string]any{"product_id": "p", "cursor": cursor, "due_only": true})
		page := work.query(work.agent, "list_execution_events", string(query))
		raw, err := json.Marshal(page)
		if err != nil || len(raw)+1 > domain.MaxExecutionEventPageBytes {
			t.Fatalf("delivery page exceeds limit: %d bytes (%v)", len(raw), err)
		}
		var ids []string
		for _, event := range page["items"].([]domain.ExecutionEvent) {
			if event.ID == "large" && (event.Reason != "" || event.Type != "execution_continuation_required") {
				t.Fatal("oversized event did not retain its delivery metadata")
			}
			ids = append(ids, event.ID)
		}
		if fmt.Sprint(ids) != fmt.Sprint(want) {
			t.Fatalf("delivery page = %v; want %v", ids, want)
		}
		cursor = page["next_cursor"].(string)
	}
	if cursor != "" || work.store.state.ExecutionEvents[0].Reason != largeReason {
		t.Fatal("delivery paging modified history or did not finish")
	}
}

func TestStalledWakeAcknowledgesActiveSkip(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	group := thirdReviewGroup(work)
	work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("executor"))
	state := &work.store.state
	executionCondition(state, executionFor(state, "i"), "stalled", true, "no progress", time.Now().UTC())
	var stalled domain.ExecutionEvent
	for _, event := range state.ExecutionEvents {
		if event.Type == "execution_stalled" && event.WakeTarget.GroupID == group.ID {
			stalled = event
		}
	}
	data := reviewDelivery(stalled.ID)
	data["delivery_id"] = work.run(work.agent, "claim_execution_event_delivery", "p", "", "", "", data)
	data["outcome"] = "skipped_active"
	ack := work.run(work.agent, "ack_execution_event", "p", "", "", "", data).(domain.ExecutionEvent)
	if ack.EscalationRequired || groupIdleEffectiveAttempts(ack) != 0 || !ack.Delivery.NextAttemptAt.After(time.Now().UTC()) {
		t.Fatalf("active pane skip spent a retry or escalated: %+v", ack)
	}
}

func TestIdleEventMemberPreviewAndFullGroup(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		group := thirdReviewGroup(work)
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", idleTestPolicy(0, 0, 0, 60))
		for index := range 11 {
			member := fmt.Sprintf("member-%02d", index)
			work.run(work.owner, "create_integration", "p", "f", "", member, map[string]any{"title": member, "objective": "Ship"})
			work.run(work.agent, "add_execution_group_member", "p", "", "", "", map[string]any{"group_id": group.ID, "integration_id": member})
		}
		now := time.Now().UTC().Add(2 * time.Minute)
		if err := store.Update(context.Background(), func(state *domain.State) error {
			reconcileGroupIdle(state, now)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		state := thirdReviewState(t, store)
		event := idleEvents(state.ExecutionEvents, group.ID, true)[domain.EventGroupMemberStale]
		if event.GroupIdle == nil || event.GroupIdle.MemberTotal != 12 || len(event.GroupIdle.Members) != 10 {
			t.Fatalf("wrong member preview: %+v", event.GroupIdle)
		}
		if members := work.query(work.agent, "get_execution_group", group.ID)["members"].([]map[string]any); len(members) != 12 {
			t.Fatalf("full group details truncated: %d", len(members))
		}
		if err := store.Update(context.Background(), func(state *domain.State) error {
			integration(state, "member-10").LastRecordedProgress = &now
			reconcileGroupIdle(state, now.Add(2*time.Minute))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		state = thirdReviewState(t, store)
		next := idleEvents(state.ExecutionEvents, group.ID, true)[domain.EventGroupMemberStale]
		if next.ID == event.ID || next.GroupIdle == nil || next.GroupIdle.MemberTotal != 12 {
			t.Fatalf("progress outside preview did not retire the stale episode: %+v", next)
		}
	})
}

func TestMemberStalenessStoreMatrix(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		group := thirdReviewGroup(work)
		work.run(work.owner, "create_integration", "p", "f", "", "fresh", map[string]any{"title": "Fresh", "objective": "Ship"})
		work.run(work.agent, "add_execution_group_member", "p", "", "", "", map[string]any{"group_id": group.ID, "integration_id": "fresh"})
		execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("executor-pane")).(domain.IntegrationExecution)
		ctx := context.Background()
		sweep := func(now time.Time) domain.ExecutionEvent {
			t.Helper()
			if err := store.Update(ctx, func(state *domain.State) error { reconcileGroupIdle(state, now); return nil }); err != nil {
				t.Fatal(err)
			}
			return idleEvents(thirdReviewState(t, store).ExecutionEvents, group.ID, true)[domain.EventGroupMemberStale]
		}
		state := thirdReviewState(t, store)
		if idlePolicy(&state, "p").MemberStaleAfterSeconds != 900 {
			t.Fatal("default staleness is not 15 minutes")
		}
		clock := time.Now().UTC()
		if got := sweep(clock); got.ID != "" {
			t.Fatalf("fresh member already stale: %+v", got)
		}
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", idleTestPolicy(0, 0, 0, 60))
		old := clock.Add(-2 * time.Minute)
		if err := store.Update(ctx, func(state *domain.State) error {
			state.ExecutionGroups[0].CreatedAt = old
			state.ExecutionGroups[0].Revisions[0].CreatedAt = old
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		work.run(work.agent, "heartbeat_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence})
		stale := sweep(time.Now().UTC())
		if stale.ID == "" || stale.WakeTarget != groupWakeTarget(group) || len(stale.GroupIdle.Members) != 1 {
			t.Fatalf("expected stale-only wake to orchestrator: %+v", stale)
		}
		member := stale.GroupIdle.Members[0]
		if member.IntegrationID != "i" || member.NextAction == "" || member.SecondsSinceLastProgress == nil || *member.SecondsSinceLastProgress < 120 {
			t.Fatalf("invalid stale member: %+v", member)
		}
		work.run(work.agent, "heartbeat_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence})
		if got := sweep(time.Now().UTC()); got.ID != stale.ID {
			t.Fatalf("heartbeat reset or duplicated the stale episode: %+v", got)
		}
		worker := userCtx("agent/worker", "agent", "alice", "member")
		work.run(worker, "record_progress", "p", "f", "i", "", map[string]any{"title": "Checkpoint", "body": "Worker advanced", "execution_fence": execution.Fence})
		if current := thirdReviewState(t, store).IntegrationExecutions[0]; current.LeasePrincipal != execution.LeasePrincipal || current.Fence != execution.Fence {
			t.Fatalf("worker progress changed execution ownership: %+v", current)
		}
		if got := sweep(time.Now().UTC()); got.ID != "" {
			t.Fatalf("progress did not clear stale episode: %+v", got)
		}
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", idleTestPolicy(0, 0, 0, 0))
		if got := sweep(clock.Add(time.Hour)); got.ID != "" {
			t.Fatalf("disabled stale policy raised an event: %+v", got)
		}
	})
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
		policy := idleTestPolicy(10, 120, 180, 900)
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
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", idleTestPolicy(0, 0, 0, 0))
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
		work.run(work.owner, "set_product_idle_policy", "p", "", "", "", idleTestPolicy(10, 20, 60, 60))
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
