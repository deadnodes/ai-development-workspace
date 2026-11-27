package application

import (
	"testing"

	"releasecontrol/internal/domain"
)

// The owner answers decisions in chat; a different agent of the Product owner
// relays the answer. The agent that requested the decision cannot approve it,
// and agents of other users cannot either.
func TestDecisionAnswerRelayedByOwnersOtherAgent(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.idea("idea")
	chat := userCtx("agent/alice-chat", "agent", "alice", "member")
	question := work.run(work.agent, "request_idea_promotion", "p", "idea", "", "", map[string]any{"rationale": "Ready"}).(domain.Memory)
	_, err := work.apply(work.agent, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, "approve"))
	expectWorkError(t, err, "forbidden")
	_, err = work.apply(work.bobAgent, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, "approve"))
	if err == nil {
		t.Fatal("another user's agent answered the owner's decision")
	}
	work.run(chat, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, "approve"))
	if f := feature(&work.store.state, "idea"); f == nil || f.Status != "ready_for_execution" {
		t.Fatalf("relayed approval did not promote the idea: %+v", f)
	}
}

func TestDependencyApprovalRelayedByOwnersOtherAgent(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.idea("idea")
	chat := userCtx("agent/alice-chat", "agent", "alice", "member")
	edge := work.run(work.agent, "propose_dependency", "p", "", "", "", map[string]any{"from_feature_id": "idea", "to_feature_id": "f", "type": "requires", "rationale": "After f"}).(domain.FeatureDependency)
	question := work.run(work.agent, "request_dependency_decision", "p", "", "", "", map[string]any{"dependency_id": edge.ID, "decision": "accept"}).(domain.Memory)
	work.run(chat, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, "approve"))
	work.run(work.agent, "decide_dependency", "p", "", "", "", map[string]any{"dependency_id": edge.ID, "decision": "accept", "approval_question_id": question.ID})
	for _, d := range work.store.state.FeatureDependencies {
		if d.ID == edge.ID && d.Status != "accepted" {
			t.Fatalf("relayed approval did not allow acceptance: %+v", d)
		}
	}
}
