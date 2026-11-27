package application

import (
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

// Coarse clocks (Windows) give consecutive edits the same UpdatedAt; approval
// staleness must follow the revision counter, never the timestamp.
func TestIdeaApprovalStaleOnEqualTimestamps(t *testing.T) {
	at := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	parent := &domain.Feature{Meta: domain.Meta{ID: "idea", UpdatedAt: at}, Status: "idea", Revision: 3}
	question := &domain.Memory{Meta: domain.Meta{ID: "q"}, Question: &domain.QuestionDetails{DecisionFor: "idea_promotion", DecisionAction: "ready_for_execution", DecisionRevision: 3}}
	if !decisionMatches(question, parent, nil) {
		t.Fatal("approval bound to the current revision must match")
	}
	parent.Revision++ // edited within the same clock tick
	if parent.UpdatedAt != at || decisionMatches(question, parent, nil) {
		t.Fatal("approval must be stale after a revision change with an identical timestamp")
	}
}
