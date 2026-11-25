package application

import (
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func TestExecutionNextActionCodes(t *testing.T) {
	in := &domain.Integration{Meta: domain.Meta{ID: "i"}, Status: "planned"}
	group := &domain.ExecutionGroup{OrchestratorID: "orch"}
	cases := []struct {
		name, want string
		ex         *domain.IntegrationExecution
	}{
		{"claim", "claim", nil},
		{"paused claim", "claim", &domain.IntegrationExecution{State: "paused", DesiredState: "ready"}},
		{"resume", "resume", &domain.IntegrationExecution{State: "lost", DesiredState: "ready", Fence: 1, OrchestratorID: "orch"}},
		{"takeover", "takeover", &domain.IntegrationExecution{State: "lost", DesiredState: "ready", Fence: 1, OrchestratorID: "old"}},
		{"wait", "wait", &domain.IntegrationExecution{State: "waiting_external", DesiredState: "ready", WaitingRef: "operation-1"}},
		{"advance", "advance", &domain.IntegrationExecution{State: "running", DesiredState: "ready", Fence: 2}},
		{"none", "none", &domain.IntegrationExecution{State: "completed", DesiredState: "ready"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := executionNextAction(in, tc.ex, []domain.ExecutionEvent{{ID: "wake", IntegrationID: "i", Type: "execution_lost", Acknowledgements: []domain.ExecutionEventAck{{Outcome: "delivered"}}}}, group)
			if got["code"] != tc.want || got["text"] == "" {
				t.Fatalf("next_action = %#v", got)
			}
		})
	}
}

func TestExecutionInputsAcceptReasonNoteAndReportUnknownFields(t *testing.T) {
	if err := validateExecutionInput(map[string]any{"reason": "idle", "note": strings.Repeat("n", 500)}, "claim_execution", "reason", "note"); err != nil {
		t.Fatalf("valid optional inputs rejected: %v", err)
	}
	if err := validateExecutionInput(map[string]any{"note": strings.Repeat("n", 501)}, "release_execution", "note"); err == nil {
		t.Fatal("oversized note accepted")
	}
	err := validateExecutionInput(map[string]any{"unexpected": true}, "release_execution", "fence", "outcome", "reason", "note")
	if err == nil || !strings.Contains(err.Error(), "unknown field unexpected; accepted: fence, outcome, reason, note") {
		t.Fatalf("unknown field error = %v", err)
	}
}
