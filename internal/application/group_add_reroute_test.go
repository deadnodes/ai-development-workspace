package application

import (
	"context"
	"testing"

	"releasecontrol/internal/domain"
)

// An event raised while the integration was outside any group must be rebound
// to the group's orchestrator once the integration is added to the group
// (dev incident: execution_stalled kept failing "missing valid execution group").
func TestGroupAddReroutesPendingWakeToOrchestrator(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("executor"))
		question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
		work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
		pending := func() domain.ExecutionEvent {
			st, err := store.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			return reviewPendingWake(t, st, domain.ExecutionCauseBlockerResolved)
		}
		before := pending()
		if before.WakeTarget.GroupID != "" {
			t.Fatalf("event outside a group has a group target: %+v", before.WakeTarget)
		}
		work.run(work.owner, "create_integration", "p", "f", "", "other", map[string]any{"title": "Other", "objective": "Ship"})
		group := work.run(work.agent, "create_execution_group", "p", "", "", "", map[string]any{"name": "Group", "orchestrator_id": "orchestrator", "orchestrator_harness": map[string]any{"kind": "codex", "tmux_target": "orch"}, "integration_ids": []string{"other"}}).(domain.ExecutionGroup)
		work.run(work.agent, "add_execution_group_member", "p", "", "", "", map[string]any{"group_id": group.ID, "integration_id": "i"})
		after := pending()
		if after.ID == before.ID || after.WakeTarget.GroupID != group.ID || after.WakeTarget.TmuxTarget != "orch" || after.WakeTarget.Source != "group_orchestrator" {
			t.Fatalf("pending wake not rebound to the group orchestrator: %+v -> %+v", before.WakeTarget, after.WakeTarget)
		}
		work.run(work.agent, "claim_execution_event_delivery", "p", "", "", "", reviewDelivery(after.ID))
	})
}
