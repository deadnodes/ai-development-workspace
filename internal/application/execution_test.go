package application

import (
	"context"
	"encoding/json"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"strings"
	"sync"
	"testing"
	"time"
)

type executionCountingStore struct {
	*memoryStore
	writes int
}

type executionSerialStore struct {
	*memoryStore
	mu sync.Mutex
}

func (m *executionSerialStore) Read(ctx context.Context) (domain.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.memoryStore.Read(ctx)
}
func (m *executionSerialStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.memoryStore.Update(ctx, fn)
}
func (m *executionSerialStore) UpdateScoped(ctx context.Context, scope domain.StateMutationScope, fn func(*domain.State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.memoryStore.UpdateScoped(ctx, scope, fn)
}

func (m *executionCountingStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	m.writes++
	return m.memoryStore.Update(ctx, fn)
}
func (m *executionCountingStore) UpdateScoped(ctx context.Context, scope domain.StateMutationScope, fn func(*domain.State) error) error {
	m.writes++
	return m.memoryStore.UpdateScoped(ctx, scope, fn)
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

func TestExecutionNotificationConfigDefaultsAndOverrides(t *testing.T) {
	if retry, cap, escalate := executionNotifyConfig(&Service{}); retry != 30 || cap != 900 || escalate != 10 {
		t.Fatalf("defaults=%d/%d/%d", retry, cap, escalate)
	}
	s := &Service{executionDefaults: &buildconfig.Config{NotifyRetrySeconds: 12, MaxBackoffSeconds: 75, EscalateAfterAttempts: 4}}
	if retry, cap, escalate := executionNotifyConfig(s); retry != 12 || cap != 75 || escalate != 4 {
		t.Fatalf("overrides=%d/%d/%d", retry, cap, escalate)
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
			typ := "execution_lost"
			if state == "stalled" {
				typ = "execution_stalled"
			}
			m.state.IntegrationExecutions = append(m.state.IntegrationExecutions, domain.IntegrationExecution{ID: "e", IntegrationID: "i", ProductID: "p", State: state, Fence: 3})
			m.state.ExecutionEvents = append(m.state.ExecutionEvents, domain.ExecutionEvent{ID: "check", ProductID: "p", IntegrationID: "i", Type: typ, Acknowledgements: []domain.ExecutionEventAck{{Outcome: "delivered"}}})
			data := map[string]any{"orchestrator_id": "o", "agent_id": "a", "fence": float64(3), "state_check_reference": "check"}
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
			exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a", "harness": map[string]any{"kind": "other"}}})
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

func TestExecutionEventDedupeAuditAndDeliveryLifecycle(t *testing.T) {
	s, m := fixture(t)
	exec(t, s, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
	e := executionFor(&m.state, "i")
	now := time.Now().UTC()
	executionCondition(&m.state, e, "lost", true, "lease expired", now)
	if len(m.state.ExecutionEvents) != 1 {
		t.Fatalf("events=%d", len(m.state.ExecutionEvents))
	}
	executionCondition(&m.state, e, "lost", true, "lease expired", now.Add(time.Second))
	if len(m.state.ExecutionEvents) != 1 {
		t.Fatalf("duplicate event: %d", len(m.state.ExecutionEvents))
	}
	ev := m.state.ExecutionEvents[0]
	if !hasAuditAction(m.state.Events, "execution_event_created") {
		t.Fatal("creation audit absent")
	}
	delays := []int{30, 60, 120, 240, 480, 900, 900, 900}
	for i, want := range delays {
		if i > 0 {
			ev.Delivery.NextAttemptAt = time.Now().Add(-time.Second)
			ev.Delivery.AcknowledgedAt = ptrTime(time.Now().Add(-time.Second))
		}
		m.state.ExecutionEvents[0] = ev
		delivery := exec(t, s, domain.Command{Action: "claim_execution_event_delivery", ProductID: "p", Data: map[string]any{"event_id": ev.ID, "supervisor_id": "sup"}}).(string)
		ev = m.state.ExecutionEvents[0]
		got := int(ev.Delivery.NextAttemptAt.Sub(*ev.Delivery.LastAttemptAt).Seconds())
		if got != want {
			t.Fatalf("attempt %d delay=%d want %d", i+1, got, want)
		}
		if i == 0 {
			exec(t, s, domain.Command{Action: "ack_execution_event", ProductID: "p", Data: map[string]any{"event_id": ev.ID, "delivery_id": delivery, "supervisor_id": "sup", "outcome": "delivered"}})
		}
	}
	if !hasAuditAction(m.state.Events, "execution_event_delivery_claimed") || !hasAuditAction(m.state.Events, "execution_event_acknowledged") {
		t.Fatal("delivery lifecycle audit absent")
	}
}

func TestExecutionEventAckDoesNotResolveAndUnsupportedEscalates(t *testing.T) {
	s, m := fixture(t)
	m.state.IntegrationExecutions = append(m.state.IntegrationExecutions, domain.IntegrationExecution{ID: "ex", ProductID: "p", IntegrationID: "i", Fence: 1, State: "unassigned", Reconciliation: domain.ExecutionReconciliation{ActionRequired: true, Generation: 1}})
	ev := domain.ExecutionEvent{ID: "ev", ProductID: "p", IntegrationID: "i", Type: "execution_unassigned", Fence: 1, ConditionGeneration: 1, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: time.Now().Add(-time.Second)}}
	m.state.ExecutionEvents = append(m.state.ExecutionEvents, ev)
	delivery := exec(t, s, domain.Command{Action: "claim_execution_event_delivery", ProductID: "p", Data: map[string]any{"event_id": "ev", "supervisor_id": "sup"}}).(string)
	exec(t, s, domain.Command{Action: "ack_execution_event", ProductID: "p", Data: map[string]any{"event_id": "ev", "delivery_id": delivery, "supervisor_id": "sup", "outcome": "unsupported"}})
	if m.state.ExecutionEvents[0].Resolution.ResolvedAt != nil || !m.state.ExecutionEvents[0].EscalationRequired {
		t.Fatalf("ack state: %+v", m.state.ExecutionEvents[0])
	}
	m.state.ExecutionEvents[0].Delivery.NextAttemptAt = time.Now().Add(-time.Second)
	if _, err := s.Execute(context.Background(), domain.Command{Action: "claim_execution_event_delivery", Actor: "agent", ProductID: "p", Data: map[string]any{"event_id": "ev", "supervisor_id": "sup"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(context.Background(), domain.Command{Action: "resolve_execution_event", Actor: "agent", ProductID: "p", Data: map[string]any{"event_id": "ev", "supervisor_id": "sup", "fence": float64(0), "evidence": "cleared"}}); err == nil || !strings.Contains(err.Error(), "INVALID_STATE") {
		t.Fatalf("resolved active condition: %v", err)
	}
	items, err := s.Query(context.Background(), "list_attention", "p")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items.([]map[string]any) {
		if item["execution_event_id"] == "ev" {
			found = true
		}
	}
	if !found {
		t.Fatal("escalation missing from attention")
	}
	m.state.IntegrationExecutions[0].Reconciliation.ActionRequired = false
	if _, err := s.Execute(context.Background(), domain.Command{Action: "resolve_execution_event", Actor: "agent", ProductID: "p", Data: map[string]any{"event_id": "ev", "supervisor_id": "sup", "fence": float64(1), "evidence": "condition cleared"}}); err != nil {
		t.Fatalf("resolve after condition cleared: %v", err)
	}
	if !hasAuditAction(m.state.Events, "execution_event_acknowledged") || !hasAuditAction(m.state.Events, "execution_event_escalated") {
		t.Fatal("ack/escalation audit absent")
	}
	if !hasAuditAction(m.state.Events, "execution_event_resolved") {
		t.Fatal("resolution audit absent")
	}
}

func TestUnacknowledgedExecutionEventCanBeReclaimedAfterBackoff(t *testing.T) {
	s, m := fixture(t)
	event := domain.ExecutionEvent{ID: "ev", ProductID: "p", IntegrationID: "i", Delivery: domain.ExecutionEventDelivery{NextAttemptAt: time.Now().UTC().Add(-time.Second)}}
	m.state.ExecutionEvents = append(m.state.ExecutionEvents, event)
	old := exec(t, s, domain.Command{Action: "claim_execution_event_delivery", ProductID: "p", Data: map[string]any{"event_id": "ev", "supervisor_id": "sup"}}).(string)
	m.state.ExecutionEvents[0].Delivery.NextAttemptAt = time.Now().UTC().Add(-time.Second)
	newID := exec(t, s, domain.Command{Action: "claim_execution_event_delivery", ProductID: "p", Data: map[string]any{"event_id": "ev", "supervisor_id": "sup"}}).(string)
	if old == newID {
		t.Fatal("retry reused delivery id")
	}
	_, err := s.Execute(context.Background(), domain.Command{Action: "ack_execution_event", Actor: "agent", ProductID: "p", Data: map[string]any{"event_id": "ev", "delivery_id": old, "supervisor_id": "sup", "outcome": "delivered"}})
	if err == nil || !strings.Contains(err.Error(), "INVALID_STATE") {
		t.Fatalf("late ack error=%v", err)
	}
}

func TestTurnEndDebounceIdempotencyAndMemberFiltering(t *testing.T) {
	s, m := fixture(t)
	ids := []string{"i"}
	for _, state := range []string{"running", "completed", "paused", "waiting_external", "blocked"} {
		iid := "member-" + state
		m.state.Integrations = append(m.state.Integrations, domain.Integration{Meta: domain.Meta{ID: iid, ProductID: "p"}, Status: "planned"})
		m.state.IntegrationExecutions = append(m.state.IntegrationExecutions, domain.IntegrationExecution{ID: "ex-" + iid, IntegrationID: iid, ProductID: "p", DesiredState: "ready", State: state, OrchestratorID: "orch", Harness: domain.ExecutionHarness{Kind: "claude_code", SessionRef: "s-" + iid}, Reconciliation: domain.ExecutionReconciliation{Enabled: true}})
		ids = append(ids, iid)
	}
	m.state.IntegrationExecutions = append(m.state.IntegrationExecutions, domain.IntegrationExecution{ID: "ex-i", IntegrationID: "i", ProductID: "p", DesiredState: "ready", State: "running", OrchestratorID: "orch", Harness: domain.ExecutionHarness{Kind: "codex", SessionRef: "session"}, Reconciliation: domain.ExecutionReconciliation{Enabled: true}})
	// Seed intended terminal and pending flags explicitly.
	for _, e := range m.state.IntegrationExecutions {
		if e.IntegrationID == "member-completed" || e.IntegrationID == "member-paused" || e.IntegrationID == "member-waiting_external" || e.IntegrationID == "member-blocked" {
			e.Reconciliation.ActionRequired = false
		}
	}
	_, err := s.Execute(context.Background(), domain.Command{Action: "create_execution_group", Actor: "agent", ProductID: "p", Data: map[string]any{"name": "g", "orchestrator_id": "orch", "orchestrator_harness": map[string]any{"kind": "codex", "session_ref": "session", "tmux_target": "orch"}, "integration_ids": ids}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Execute(context.Background(), domain.Command{Action: "report_turn_end", Actor: "agent", ProductID: "p", Data: map[string]any{"harness_kind": "codex", "session_ref": "unknown"}})
	if err != nil || len(res.(map[string]any)["matched"].([]string)) != 0 {
		t.Fatalf("unknown session result=%v err=%v", res, err)
	}
	res, err = s.Execute(context.Background(), domain.Command{Action: "report_turn_end", Actor: "agent", ProductID: "p", Data: map[string]any{"harness_kind": "codex", "session_ref": "session", "turn_id": "turn-1"}})
	if err != nil || len(res.(map[string]any)["matched"].([]string)) != len(ids) {
		t.Fatalf("turn report=%v err=%v", res, err)
	}
	if err = s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.state.ExecutionEvents) != 0 {
		t.Fatalf("event before debounce: %+v", m.state.ExecutionEvents)
	}
	base := executionFor(&m.state, "i")
	base.TurnEndAt = ptrTime(time.Now().UTC().Add(-21 * time.Second))
	if err = s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.state.ExecutionEvents) != 2 {
		t.Fatalf("expected actionable unfinished members events, got %+v", m.state.ExecutionEvents)
	}
	for _, ev := range m.state.ExecutionEvents {
		if ev.WakeTarget.Source != "group_orchestrator" || ev.WakeTarget.SessionRef != "session" || ev.WakeTarget.TmuxTarget != "orch" {
			t.Fatalf("missing group wake target: %+v", ev)
		}
	}
	if err = s.ReconcileExecutionsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.state.ExecutionEvents) != 2 {
		t.Fatalf("turn end not idempotent: %d", len(m.state.ExecutionEvents))
	}
}

func TestExecutionEventCursorDueFiltersAndClaimCAS(t *testing.T) {
	s, m := fixture(t)
	s.store = &executionSerialStore{memoryStore: m}
	base := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 3; i++ {
		m.state.ExecutionEvents = append(m.state.ExecutionEvents, domain.ExecutionEvent{ID: []string{"a", "b", "c"}[i], ProductID: "p", CreatedAt: base, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: base}})
	}
	query := func(cursor string, limit int, due bool) map[string]any {
		b, _ := json.Marshal(map[string]any{"product_id": "p", "cursor": cursor, "limit": limit, "due_only": due})
		v, e := s.Query(context.Background(), "list_execution_events", string(b))
		if e != nil {
			t.Fatal(e)
		}
		return v.(map[string]any)
	}
	page := query("", 1, false)
	first := page["items"].([]domain.ExecutionEvent)
	if len(first) != 1 || first[0].ID != "a" {
		t.Fatalf("first page: %+v", first)
	}
	cur := page["next_cursor"].(string)
	m.state.ExecutionEvents = append(m.state.ExecutionEvents, domain.ExecutionEvent{ID: "d", ProductID: "p", CreatedAt: base.Add(time.Second), Delivery: domain.ExecutionEventDelivery{NextAttemptAt: base}})
	page2 := query(cur, 10, false)
	got := page2["items"].([]domain.ExecutionEvent)
	if len(got) != 3 || got[0].ID != "b" || got[1].ID != "c" || got[2].ID != "d" {
		t.Fatalf("stable second page: %+v", got)
	}
	pageDue := query("", 100, true)
	if len(pageDue["items"].([]domain.ExecutionEvent)) != 4 {
		t.Fatal("due events omitted")
	}
	var wg sync.WaitGroup
	successes := 0
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Execute(context.Background(), domain.Command{Action: "claim_execution_event_delivery", Actor: "agent", ProductID: "p", Data: map[string]any{"event_id": "a", "supervisor_id": "sup"}})
			if e == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("concurrent winners=%d", successes)
	}
	pageDue = query("", 100, true)
	for _, item := range pageDue["items"].([]domain.ExecutionEvent) {
		if item.ID == "a" {
			t.Fatal("unacknowledged current attempt shown due")
		}
	}
}

func TestTakeoverRequiresDeliveredLostOrStalledEvidenceForSameIntegration(t *testing.T) {
	for _, tc := range []struct{ name, typ, integration, outcome string }{
		{"not_delivered", "execution_lost", "i", "failed"},
		{"wrong_type", "execution_unassigned", "i", "delivered"},
		{"wrong_integration", "execution_lost", "other", "delivered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := fixture(t)
			m.state.IntegrationExecutions = append(m.state.IntegrationExecutions, domain.IntegrationExecution{ID: "ex", ProductID: "p", IntegrationID: "i", State: "lost", Fence: 3})
			m.state.ExecutionEvents = append(m.state.ExecutionEvents, domain.ExecutionEvent{ID: "ev", ProductID: "p", IntegrationID: tc.integration, Type: tc.typ, Acknowledgements: []domain.ExecutionEventAck{{Outcome: tc.outcome}}})
			_, err := s.Execute(context.Background(), domain.Command{Action: "takeover_execution", Actor: "agent", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "new", "fence": float64(3), "state_check_reference": "ev"}})
			if err == nil || !strings.Contains(err.Error(), "TAKEOVER_CHECK_REQUIRED") {
				t.Fatalf("takeover error=%v", err)
			}
		})
	}
}

func hasAuditAction(events []domain.Event, action string) bool {
	for _, e := range events {
		if e.Action == action {
			return true
		}
	}
	return false
}

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
	event := m.state.ExecutionEvents[0]
	delivery := exec(t, s, domain.Command{Action: "claim_execution_event_delivery", ProductID: "p", Data: map[string]any{"event_id": event.ID, "supervisor_id": "sup"}}).(string)
	exec(t, s, domain.Command{Action: "ack_execution_event", ProductID: "p", Data: map[string]any{"event_id": event.ID, "delivery_id": delivery, "supervisor_id": "sup", "outcome": "delivered"}})
	exec(t, s, domain.Command{Action: "takeover_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "new", "fence": float64(1), "state_check_reference": event.ID}})
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

func TestExecutionEventSelectsOrchestratorGroupAndOrdersWakeContext(t *testing.T) {
	_, m := fixture(t)
	now := time.Now().UTC()
	m.state.ExecutionGroups = []domain.ExecutionGroup{
		{ID: "group-no-harness", ProductID: "p", MemberIntegrationIDs: []string{"i"}},
		{ID: "group-orch", ProductID: "p", MemberIntegrationIDs: []string{"i"}, OrchestratorHarness: domain.ExecutionHarness{Kind: "codex", TmuxTarget: "orch-target"}},
		{ID: "group-closed", ProductID: "p", MemberIntegrationIDs: []string{"i"}, ClosedAt: &now, OrchestratorHarness: domain.ExecutionHarness{Kind: "claude_code", TmuxTarget: "closed-target"}},
	}
	e := &domain.IntegrationExecution{ID: "execution", IntegrationID: "i", ProductID: "p", State: "lost", Fence: 3, Reconciliation: domain.ExecutionReconciliation{Generation: 1}}
	createExecutionEvent(&m.state, e, "execution_lost", "lease expired", now)
	if len(m.state.ExecutionEvents) != 1 {
		t.Fatalf("events=%+v", m.state.ExecutionEvents)
	}
	ev := m.state.ExecutionEvents[0]
	if ev.WakeTarget.Source != "group_orchestrator" || ev.WakeTarget.TmuxTarget != "orch-target" || len(ev.GroupIDs) != 2 || ev.GroupIDs[0] != "group-orch" {
		t.Fatalf("event routing: %+v", ev)
	}
}
