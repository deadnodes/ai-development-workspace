package application

import (
	"context"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

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
	}{{"product_over_global", 5, 100, -1, 30 * time.Second, "running"}, {"integration_over_product", 5, 100, 10, 20 * time.Second, "stalled"}, {"product_zero_disables", 5, 0, -1, time.Hour, "running"}, {"integration_zero_disables", 5, 1, 0, time.Hour, "running"}} {
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
