package application

import (
	"context"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

type executionCountingStore struct {
	*memoryStore
	writes int
}

func (m *executionCountingStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	m.writes++
	return m.memoryStore.Update(ctx, fn)
}

func TestIdleExecutionSweepDoesNotWrite(t *testing.T) {
	_, base := fixture(t)
	store := &executionCountingStore{memoryStore: base}
	s := New(store)
	if err := s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.writes != 0 {
		t.Fatalf("idle sweep wrote %d times", store.writes)
	}
}

func TestExecutionDefaultsWithoutBuildConfiguration(t *testing.T) {
	s, m := fixture(t)
	if got := s.ExecutionReconcileInterval(); got != 30*time.Second {
		t.Fatalf("reconcile interval=%v", got)
	}
	if got := s.ExecutionHeartbeatInterval(); got != 60*time.Second {
		t.Fatalf("heartbeat interval=%v", got)
	}
	exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
	ex := executionFor(&m.state, "i")
	if ex.LeaseTTLSeconds != 300 {
		t.Fatalf("lease ttl=%d", ex.LeaseTTLSeconds)
	}
	if got, source := effectiveStalledTimeout(&m.state, ex); got != 900 || source != "global" {
		t.Fatalf("stalled timeout=%d source=%s", got, source)
	}
}

func TestCreateExecutionGroupRecognizesReleasedAsReady(t *testing.T) {
	s, m := fixture(t)
	m.state.Integrations[0].Status = "released"
	exec(t, s, domain.Command{Action: "create_execution_group", ProductID: "p", Data: map[string]any{"name": "g", "orchestrator_id": "o", "integration_ids": []string{"i"}}})
	if got := executionFor(&m.state, "i"); got == nil || got.State != "completed" {
		t.Fatalf("released integration initial execution: %+v", got)
	}
}

func TestExecutionGroupReportsEffectiveStalledTimeoutAndSource(t *testing.T) {
	s, _ := fixture(t)
	exec(t, s, domain.Command{Action: "set_product_stalled_timeout", ProductID: "p", Data: map[string]any{"seconds": float64(40)}})
	exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a", "stalled_timeout_seconds": float64(20)}})
	g := exec(t, s, domain.Command{Action: "create_execution_group", ProductID: "p", Data: map[string]any{"name": "g", "orchestrator_id": "o", "integration_ids": []string{"i"}}}).(domain.ExecutionGroup)
	result, err := s.Query(context.Background(), "get_execution_group", g.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := result.(map[string]any)["members"].([]map[string]any)[0]
	if member["effective_stalled_timeout_seconds"] != 20 || member["stalled_timeout_source"] != "integration" {
		t.Fatalf("timeout output: %#v", member)
	}
}

func TestExecutionStatusVocabularyTracksCatalog(t *testing.T) {
	checks := map[string]func(string) bool{"build": buildStatusNonTerminal, "deployment": deploymentStatusNonTerminal, "operation": operationStatusNonTerminal}
	want := map[string]map[string]bool{
		"build":      {"QUEUED": true, "DISPATCHED": true, "RUNNING": true},
		"deployment": {"VERIFYING_ROLLOUT": true, "PENDING_CHECKS": true, "PROPOSING": true, "MERGING": true},
		"operation":  {"PENDING": true, "RUNNING": true},
	}
	known := map[string]map[string]bool{
		"build":      {"QUEUED": true, "DISPATCHED": true, "RUNNING": true, "SUCCEEDED": true, "FAILED": true, "CANCELLED": true, "RECONCILIATION_REQUIRED": true},
		"deployment": {"VERIFYING_ROLLOUT": true, "PENDING_CHECKS": true, "PROPOSING": true, "MERGING": true, "SUCCEEDED": true, "FAILED": true, "RECONCILIATION_REQUIRED": true},
		"operation":  {"PENDING": true, "RUNNING": true, "SUCCEEDED": true, "FAILED": true, "CANCELLED": true},
	}
	for kind, check := range checks {
		for _, status := range domain.StatusCatalog()[kind] {
			if !known[kind][status] {
				t.Errorf("catalog added unclassified %s status %q", kind, status)
			}
			if got := check(status); got != want[kind][status] {
				t.Errorf("%s/%s nonterminal=%v want %v", kind, status, got, want[kind][status])
			}
		}
	}
}

func TestDesiredStateUsesIntegrationStatusOrder(t *testing.T) {
	for _, tc := range []struct {
		actual, desired string
		reached         bool
	}{{"ready", "ready", true}, {"released", "ready", true}, {"ready", "released", false}, {"implemented", "ready", false}} {
		if got := statusReachedIntegration(tc.actual, tc.desired); got != tc.reached {
			t.Errorf("%s reached %s = %v", tc.actual, tc.desired, got)
		}
	}
}

func TestClaimAndTakeoverStateRules(t *testing.T) {
	for _, state := range []string{"unassigned", "exited_incomplete", "paused"} {
		t.Run("claim_"+state, func(t *testing.T) {
			s, m := fixture(t)
			m.state.IntegrationExecutions = append(m.state.IntegrationExecutions, domain.IntegrationExecution{ID: "e", IntegrationID: "i", ProductID: "p", State: state, Fence: 3, Reconciliation: domain.ExecutionReconciliation{Enabled: true}})
			if _, err := s.Execute(context.Background(), domain.Command{Action: "claim_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}}); err != nil {
				t.Fatal(err)
			}
			if got := executionFor(&m.state, "i"); got.State != "running" || got.Fence != 4 {
				t.Fatalf("claim result: %+v", got)
			}
		})
	}
	for _, state := range []string{"completed", "cancelled"} {
		t.Run("reject_"+state, func(t *testing.T) {
			s, m := fixture(t)
			m.state.IntegrationExecutions = append(m.state.IntegrationExecutions, domain.IntegrationExecution{ID: "e", IntegrationID: "i", ProductID: "p", State: state, Fence: 3})
			_, err := s.Execute(context.Background(), domain.Command{Action: "claim_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
			if err == nil {
				t.Fatal("claim accepted")
			}
		})
	}
	for _, state := range []string{"lost", "stalled"} {
		t.Run("takeover_"+state, func(t *testing.T) {
			s, m := fixture(t)
			m.state.IntegrationExecutions = append(m.state.IntegrationExecutions, domain.IntegrationExecution{ID: "e", IntegrationID: "i", ProductID: "p", State: state, Fence: 3})
			data := map[string]any{"orchestrator_id": "o", "agent_id": "a", "fence": float64(3), "state_check_reference": "checked"}
			if _, err := s.Execute(context.Background(), domain.Command{Action: "takeover_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: data}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTerminalExecutionReportsReleaseLease(t *testing.T) {
	for _, state := range []string{"paused", "cancelled", "exited_incomplete", "completed"} {
		t.Run(state, func(t *testing.T) {
			s, m := fixture(t)
			if state == "completed" {
				m.state.Integrations[0].Status = "released"
			}
			exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
			_, err := s.Execute(context.Background(), domain.Command{Action: "release_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"fence": float64(1), "outcome": state}})
			if err != nil {
				t.Fatal(err)
			}
			if executionFor(&m.state, "i").LeaseUntil != nil {
				t.Fatal("terminal report retained lease")
			}
			_, err = s.Execute(context.Background(), domain.Command{Action: "heartbeat_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"fence": float64(1)}})
			if err == nil {
				t.Fatal("heartbeat after release accepted")
			}
			if !strings.Contains(err.Error(), "INVALID_STATE") {
				t.Fatalf("got %v, want INVALID_STATE", err)
			}
		})
	}
}

func TestInvalidExecutionReferenceDoesNotCreateConditionAudit(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
	before := len(m.state.Events)
	_, err := s.Execute(context.Background(), domain.Command{Action: "report_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"fence": float64(1), "state": "waiting_external", "waiting_ref": "missing"}})
	if err == nil {
		t.Fatal("invalid waiting reference accepted")
	}
	if len(m.state.Events) != before {
		t.Fatalf("rejected report added audit events: %d -> %d", before, len(m.state.Events))
	}
}

func TestExecutionFenceHeartbeatProgressAndReconcile(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "orch", "agent_id": "agent", "desired_state": "ready"}})
	ex := executionFor(&m.state, "i")
	if ex == nil || ex.Fence != 1 {
		t.Fatalf("claim: %+v", ex)
	}
	if ex.Reconciliation.ActionRequired {
		t.Fatal("claimed execution still requires action")
	}
	priorProgress := *ex.LastProgress
	exec(t, s, domain.Command{Action: "record_progress", ProductID: "p", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"title": "legacy progress"}})
	ex = executionFor(&m.state, "i")
	if !ex.LastProgress.Equal(priorProgress) {
		t.Fatal("unfenced progress receipt advanced progress clock")
	}
	last := m.state.Events[len(m.state.Events)-1]
	if last.Data.Data["execution_unfenced"] != true {
		t.Fatalf("unfenced write not audited: %+v", last)
	}
	if _, err := s.Execute(context.Background(), domain.Command{Action: "heartbeat_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"fence": float64(0)}}); err == nil {
		t.Fatal("stale heartbeat accepted")
	}
	if _, err := s.Execute(context.Background(), domain.Command{Action: "record_progress", Actor: "agent", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"title": "progress", "execution_fence": float64(0)}}); err == nil {
		t.Fatal("stale progress accepted")
	}
	progress := ex.LastProgress
	exec(t, s, domain.Command{Action: "heartbeat_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"fence": float64(1), "progress_receipt": "step-1"}})
	if executionFor(&m.state, "i").LastProgress == progress {
		t.Fatal("progress receipt did not update progress")
	}
	exec(t, s, domain.Command{Action: "create_integration", ID: "i2", ProductID: "p", FeatureID: "f", Data: map[string]any{"title": "Second", "objective": "Keep in restore"}})
	exec(t, s, domain.Command{Action: "create_execution_group", ProductID: "p", Data: map[string]any{"name": "g", "orchestrator_id": "orch", "integration_ids": []string{"i", "i2"}}})
	g := m.state.ExecutionGroups[0]
	m.state.Integrations[0].Status = "ready"
	if err := s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ex = executionFor(&m.state, "i")
	if executionFor(&m.state, "i").State != "completed" || executionFor(&m.state, "i").Reconciliation.ActionRequired {
		t.Fatalf("desired state not completed: %+v", executionFor(&m.state, "i"))
	}
	if executionFor(&m.state, "i").LeaseUntil != nil {
		t.Fatal("reconciled completion retained lease")
	}
	v, err := s.Query(context.Background(), "get_execution_group", g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.(map[string]any)["members"].([]map[string]any)) != 2 {
		t.Fatalf("restore omitted member: %#v", v)
	}
	generation := executionFor(&m.state, "i").Reconciliation.Generation
	if err = s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executionFor(&m.state, "i").Reconciliation.Generation != generation {
		t.Fatal("non-idempotent sweep")
	}
}

func TestStalledTimeoutPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		global, product, integration int
		elapsed                      time.Duration
		want                         string
	}{{"product_over_global", 5, 100, -1, 30 * time.Second, "running"}, {"integration_over_product", 5, 100, 10, 20 * time.Second, "stalled"}, {"global_zero_disables", 0, -1, -1, time.Hour, "running"}, {"product_zero_disables", 5, 0, -1, time.Hour, "running"}, {"integration_zero_disables", 5, 1, 0, time.Hour, "running"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := fixture(t)
			s.executionDefaults = &buildconfig.Config{StalledTimeout: tc.global}
			exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
			ex := executionFor(&m.state, "i")
			ex.LastProgress = ptrTime(time.Now().Add(-tc.elapsed))
			future := time.Now().Add(10 * time.Minute)
			ex.LeaseUntil = &future
			if tc.product >= 0 {
				n := tc.product
				m.state.Products[0].StalledTimeoutSeconds = &n
			}
			if tc.integration >= 0 {
				n := tc.integration
				ex.StalledTimeoutSeconds = &n
			}
			if err := s.ReconcileExecutionsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := executionFor(&m.state, "i").State; got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
func ptrTime(t time.Time) *time.Time { return &t }

func TestStalledKeepsLeaseAndLostDoesNotReassign(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
	ex := executionFor(&m.state, "i")
	oldLease := *ex.LeaseUntil
	old := time.Now().Add(-time.Hour)
	ex.LastProgress = &old
	ex.LeaseUntil = &old
	if err := s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ex = executionFor(&m.state, "i")
	if ex.State != "lost" {
		t.Fatalf("expired lease did not become lost: %s", ex.State)
	}
	if ex.AgentID != "a" {
		t.Fatal("lost execution reassigned")
	}
	// A live lease with old progress stalls without changing the lease.
	ex.State = "running"
	ex.LeaseUntil = &oldLease
	before := *ex.LeaseUntil
	if err := s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ex = executionFor(&m.state, "i")
	if ex.State != "stalled" || !ex.LeaseUntil.Equal(before) {
		t.Fatalf("stalled changed lease: %+v", ex)
	}
}

func TestTakeoverFencesFormerExecutorAndTimeoutPrecedence(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "old"}})
	ex := executionFor(&m.state, "i")
	expired := time.Now().Add(-time.Hour)
	ex.LeaseUntil = &expired
	ex.LastProgress = &expired
	if err := s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	exec(t, s, domain.Command{Action: "takeover_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "new", "fence": float64(1), "state_check_reference": "checked"}})
	for _, action := range []string{"heartbeat_execution", "record_progress", "transition_integration"} {
		data := map[string]any{"execution_fence": float64(1), "fence": float64(1), "progress_receipt": "old"}
		c := domain.Command{Action: action, ProductID: "p", FeatureID: "f", IntegrationID: "i", Data: data}
		if action == "transition_integration" {
			c.Data = map[string]any{"status": "working", "execution_fence": float64(1)}
		}
		if _, err := s.Execute(context.Background(), c); err == nil {
			t.Fatalf("old fence accepted by %s", action)
		}
	}
	ex = executionFor(&m.state, "i")
	if ex.Fence != 2 || ex.AgentID != "new" {
		t.Fatalf("takeover did not advance fence: %+v", ex)
	}
	zero := 0
	m.state.Products[0].StalledTimeoutSeconds = &zero
	ex.State = "running"
	ex.LastProgress = &expired
	until := time.Now().Add(time.Hour)
	ex.LeaseUntil = &until
	if err := s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executionFor(&m.state, "i").State != "running" {
		t.Fatal("explicit zero did not disable product stalled detection")
	}
}
