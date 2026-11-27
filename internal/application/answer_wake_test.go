package application

import (
	"testing"

	"releasecontrol/internal/domain"
)

// Owner rule: answers recorded in RCP must reach the group's orchestrator
// promptly, for blocking and non-blocking questions alike.
func TestAnswerWakesGroupOrchestrator(t *testing.T) {
	for _, blocking := range []bool{true, false} {
		work := ownerWorkReviewFixture(t)
		group := work.run(work.agent, "create_execution_group", "p", "", "", "", map[string]any{"name": "Group", "orchestrator_id": "orchestrator", "orchestrator_harness": map[string]any{"kind": "codex", "tmux_target": "orch:0.0"}, "integration_ids": []string{"i"}}).(domain.ExecutionGroup)
		work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("orchestrator"))
		question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(blocking)).(domain.Memory)
		work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
		found := false
		for _, ev := range work.store.state.ExecutionEvents {
			if ev.Resolution.ResolvedAt == nil && ev.WakeTarget.GroupID == group.ID {
				found = true
				t.Logf("blocking=%v event type=%s cause=%s questions=%v", blocking, ev.Type, ev.Cause, ev.QuestionIDs)
			}
		}
		if !found {
			t.Fatalf("blocking=%v: answer raised no pending wake for the group orchestrator", blocking)
		}
	}
}
