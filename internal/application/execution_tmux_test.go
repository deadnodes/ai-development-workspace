package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func TestExecutionHarnessTmuxTargetValidationAndEventFields(t *testing.T) {
	for _, bad := range []string{"bad target", "bad/target", "a" + strings.Repeat("b", 64)} {
		s, _ := fixture(t)
		_, err := s.Execute(context.Background(), domain.Command{Action: "claim_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "orch", "agent_id": "agent", "harness": map[string]any{"kind": "codex", "session_ref": "session-12345678", "tmux_target": bad}}})
		if err == nil {
			t.Fatalf("invalid target accepted: %q", bad)
		}
	}
	s, m := fixture(t)
	got, err := s.Execute(context.Background(), domain.Command{Action: "claim_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "orch", "agent_id": "agent", "harness": map[string]any{"kind": "claude_code", "session_ref": "session-12345678", "tmux_target": "orch:0.1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.(domain.IntegrationExecution).Harness.TmuxTarget != "orch:0.1" {
		t.Fatalf("claim lost target: %+v", got)
	}
	e := executionFor(&m.state, "i")
	executionCondition(&m.state, e, "exited_incomplete", true, "test", time.Now().UTC())
	if len(m.state.ExecutionEvents) != 1 || m.state.ExecutionEvents[0].HarnessKind != "claude_code" || m.state.ExecutionEvents[0].SessionRef != "session-12345678" || m.state.ExecutionEvents[0].TmuxTarget != "orch:0.1" {
		t.Fatalf("event harness fields: %+v", m.state.ExecutionEvents)
	}
}

func TestExecutionHarnessRejectsUnknownKind(t *testing.T) {
	s, _ := fixture(t)
	_, err := s.Execute(context.Background(), domain.Command{Action: "claim_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "orch", "agent_id": "agent", "harness": map[string]any{"kind": "made_up"}}})
	if err == nil {
		t.Fatal("unknown harness kind accepted")
	}
}
