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
