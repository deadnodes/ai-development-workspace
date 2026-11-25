package application

import (
	"context"
	"fmt"
	"releasecontrol/internal/domain"
	"slices"
	"time"
)

func resolveExecutionEvents(st *domain.State, e *domain.IntegrationExecution, now time.Time, newState string) {
	for i := range st.ExecutionEvents {
		ev := &st.ExecutionEvents[i]
		if ev.IntegrationID == e.IntegrationID && ev.Resolution.ResolvedAt == nil && (ev.Fence != e.Fence || ev.ConditionGeneration != e.Reconciliation.Generation+1 || !e.Reconciliation.ActionRequired) {
			t := now
			ev.Resolution = domain.ExecutionEventResolution{ResolvedAt: &t, ResolvedBy: "system", Evidence: newState}
			appendExecutionEventAudit(st, *ev, "execution_event_resolved", "system/execution-reconciler", now, map[string]any{"evidence": newState})
		}
	}
}
func createExecutionEvent(st *domain.State, e *domain.IntegrationExecution, typ, reason string, now time.Time) {
	key := fmt.Sprintf("%s|%d|%s|%d", e.IntegrationID, e.Fence, typ, e.Reconciliation.Generation)
	for _, ev := range st.ExecutionEvents {
		if ev.DedupeKey == key {
			return
		}
	}
	groups := []string{}
	for _, g := range st.ExecutionGroups {
		if g.ProductID == e.ProductID && slices.Contains(g.MemberIntegrationIDs, e.IntegrationID) {
			groups = append(groups, g.ID)
		}
	}
	sessionRef, harnessKind := e.Harness.SessionRef, e.Harness.Kind
	if typ == "execution_continuation_required" && reason == "turn ended with unfinished group work" && (sessionRef == "" || harnessKind == "") {
		for _, candidate := range st.IntegrationExecutions {
			if candidate.ProductID == e.ProductID && candidate.OrchestratorID == e.OrchestratorID && candidate.TurnEndID != "" && candidate.Harness.SessionRef != "" && candidate.Harness.Kind != "" {
				sessionRef, harnessKind = candidate.Harness.SessionRef, candidate.Harness.Kind
				break
			}
		}
	}
	event := domain.ExecutionEvent{ID: id(), ProductID: e.ProductID, IntegrationID: e.IntegrationID, GroupIDs: groups, Type: typ, Fence: e.Fence, ConditionGeneration: e.Reconciliation.Generation, Reason: reason, DedupeKey: key, SessionRef: sessionRef, HarnessKind: harnessKind, CreatedAt: now, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: now}}
	st.ExecutionEvents = append(st.ExecutionEvents, event)
	appendExecutionEventAudit(st, event, "execution_event_created", "system/execution-reconciler", now, map[string]any{"type": typ, "generation": event.ConditionGeneration})
}

func appendExecutionEventAudit(st *domain.State, ev domain.ExecutionEvent, action, actor string, now time.Time, data map[string]any) {
	st.Events = append(st.Events, domain.Event{ID: id(), Action: action, Actor: actor, At: now, EntityID: ev.ID, ProductID: ev.ProductID, Data: domain.Command{Action: action, Actor: actor, ProductID: ev.ProductID, IntegrationID: ev.IntegrationID, ID: ev.ID, Data: data}})
}

func executionNotifyConfig(s *Service) (retry, cap, escalate int) {
	retry, cap, escalate = 30, 900, 10
	if s.executionDefaults != nil {
		if s.executionDefaults.NotifyRetrySeconds > 0 {
			retry = s.executionDefaults.NotifyRetrySeconds
		}
		if s.executionDefaults.MaxBackoffSeconds > 0 {
			cap = s.executionDefaults.MaxBackoffSeconds
		}
		if s.executionDefaults.EscalateAfterAttempts > 0 {
			escalate = s.executionDefaults.EscalateAfterAttempts
		}
	}
	return
}

func statusReachedIntegration(actual, desired string) bool {
	order := map[string]int{"planned": 0, "working": 1, "implemented": 2, "verifying": 3, "ready": 4, "released": 5}
	a, aok := order[actual]
	d, dok := order[desired]
	return aok && dok && a >= d
}

func statusInCatalog(kind, status string, active []string) bool {
	if !domain.ValidStatus(kind, status) {
		return false
	}
	for _, candidate := range active {
		if candidate == status {
			return true
		}
	}
	return false
}
func buildStatusNonTerminal(status string) bool {
	return statusInCatalog("build", status, []string{"QUEUED", "DISPATCHED", "RUNNING"})
}
func deploymentStatusNonTerminal(status string) bool {
	return statusInCatalog("deployment", status, []string{"VERIFYING_ROLLOUT", "PENDING_CHECKS", "PROPOSING", "MERGING"})
}
func operationStatusNonTerminal(status string) bool {
	return statusInCatalog("operation", status, []string{"PENDING", "RUNNING"})
}

func effectiveStalledTimeout(st *domain.State, e *domain.IntegrationExecution, global ...int) (int, string) {
	timeout, source := 900, "global"
	if len(global) > 0 {
		timeout = global[0]
	}
	for i := range st.Products {
		if st.Products[i].ID == e.ProductID && st.Products[i].StalledTimeoutSeconds != nil {
			timeout, source = *st.Products[i].StalledTimeoutSeconds, "product"
			break
		}
	}
	if e.StalledTimeoutSeconds != nil {
		timeout, source = *e.StalledTimeoutSeconds, "integration"
	}
	return timeout, source
}

func executionAction(a string) bool {
	switch a {
	case "claim_execution", "heartbeat_execution", "report_execution", "release_execution", "takeover_execution", "set_desired_state", "create_execution_group", "add_execution_group_member", "remove_execution_group_member", "set_product_stalled_timeout", "report_turn_end", "claim_execution_event_delivery", "ack_execution_event", "resolve_execution_event":
		return true
	}
	return false
}

func (s *Service) ExecutionReconcileInterval() time.Duration {
	seconds := 30
	if s.executionDefaults != nil {
		seconds = s.executionDefaults.ReconcileInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (s *Service) ExecutionHeartbeatInterval() time.Duration {
	seconds := 60
	if s.executionDefaults != nil && s.executionDefaults.HeartbeatInterval != 0 {
		seconds = s.executionDefaults.HeartbeatInterval
	}
	return time.Duration(seconds) * time.Second
}
func executionFor(st *domain.State, id string) *domain.IntegrationExecution {
	for i := range st.IntegrationExecutions {
		if st.IntegrationExecutions[i].IntegrationID == id {
			return &st.IntegrationExecutions[i]
		}
	}
	return nil
}
func executionFence(data map[string]any, key string) uint64 {
	switch v := data[key].(type) {
	case float64:
		if v >= 0 {
			return uint64(v)
		}
	case int:
		if v >= 0 {
			return uint64(v)
		}
	case int64:
		if v >= 0 {
			return uint64(v)
		}
	case uint64:
		return v
	}
	return 0
}
func executionCondition(st *domain.State, e *domain.IntegrationExecution, state string, required bool, reason string, now time.Time) {
	if e.State == state && e.Reconciliation.ActionRequired == required && e.Reconciliation.Reason == reason {
		return
	}
	e.State = state
	e.Reconciliation.ActionRequired = required
	e.Reconciliation.Reason = reason
	e.Reconciliation.Since = &now
	e.Reconciliation.Generation++
	resolveExecutionEvents(st, e, now, state)
	if required {
		typ := ""
		switch state {
		case "lost":
			typ = "execution_lost"
		case "stalled":
			typ = "execution_stalled"
		case "unassigned":
			typ = "execution_unassigned"
		case "exited_incomplete", "waiting_external", "blocked":
			typ = "execution_continuation_required"
		}
		if typ != "" {
			createExecutionEvent(st, e, typ, reason, now)
		}
	}
	st.Events = append(st.Events, domain.Event{ID: id(), Action: "execution_condition_changed", Actor: "system/execution-reconciler", At: now, EntityID: e.IntegrationID, ProductID: e.ProductID, Data: domain.Command{Action: "execution_condition_changed", Actor: "system/execution-reconciler", IntegrationID: e.IntegrationID, ProductID: e.ProductID, Data: map[string]any{"state": state, "action_required": required, "reason": reason, "generation": e.Reconciliation.Generation}}})
}
func (s *Service) executeExecutionCommand(ctx context.Context, c domain.Command) (any, error) {
	var out any
	err := s.store.Update(ctx, func(st *domain.State) error {
		if c.Actor == "" {
			return invalid("actor required")
		}
		now := time.Now().UTC()
		if c.IntegrationID != "" {
			in := integration(st, c.IntegrationID)
			if in == nil {
				return missing("integration", c.IntegrationID)
			}
			if in.ProductID != c.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
		}
		switch c.Action {
		case "report_turn_end":
			var x struct {
				HarnessKind string `json:"harness_kind"`
				SessionRef  string `json:"session_ref"`
				TurnID      string `json:"turn_id"`
				CWD         string `json:"cwd"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			if x.SessionRef == "" || x.HarnessKind == "" {
				return invalid("harness_kind and session_ref required")
			}
			bucket := now.Truncate(time.Minute).Format(time.RFC3339)
			key := x.TurnID
			if key == "" {
				key = bucket
			}
			matched := []string{}
			for i := range st.IntegrationExecutions {
				e := &st.IntegrationExecutions[i]
				if e.ProductID == c.ProductID && e.Harness.SessionRef == x.SessionRef && (x.HarnessKind == "" || e.Harness.Kind == x.HarnessKind) {
					if e.TurnEndID == key {
						matched = append(matched, e.IntegrationID)
						continue
					}
					e.TurnEndAt = &now
					e.TurnEndID = key
					matched = append(matched, e.IntegrationID)
				}
			}
			slices.Sort(matched)
			out = map[string]any{"matched": matched, "turn_id": key}
		case "claim_execution_event_delivery":
			var x struct {
				EventID      string `json:"event_id"`
				SupervisorID string `json:"supervisor_id"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			for i := range st.ExecutionEvents {
				ev := &st.ExecutionEvents[i]
				if ev.ID != x.EventID {
					continue
				}
				if ev.ProductID != c.ProductID {
					return invalid("SCOPE_MISMATCH")
				}
				if x.SupervisorID == "" || ev.Resolution.ResolvedAt != nil || now.Before(ev.Delivery.NextAttemptAt) {
					return invalid("INVALID_STATE")
				}
				beforeEscalation := ev.EscalationRequired
				ev.Delivery.Attempts++
				ev.Delivery.LastAttemptAt = &now
				ev.Delivery.DeliveryID = id()
				retry, cap, escalate := executionNotifyConfig(s)
				delay := retry
				for j := 1; j < ev.Delivery.Attempts && delay < cap; j++ {
					if delay > cap/2 {
						delay = cap
					} else {
						delay *= 2
					}
				}
				if delay > cap {
					delay = cap
				}
				ev.Delivery.NextAttemptAt = now.Add(time.Duration(delay) * time.Second)
				if ev.Delivery.Attempts >= escalate {
					ev.EscalationRequired = true
				}
				ev.Delivery.AcknowledgedAt = nil
				ev.Delivery.AcknowledgedBy = ""
				out = ev.Delivery.DeliveryID
				appendExecutionEventAudit(st, *ev, "execution_event_delivery_claimed", x.SupervisorID, now, map[string]any{"attempt": ev.Delivery.Attempts, "delivery_id": ev.Delivery.DeliveryID, "next_attempt_at": ev.Delivery.NextAttemptAt})
				if !beforeEscalation && ev.EscalationRequired {
					appendExecutionEventAudit(st, *ev, "execution_event_escalated", x.SupervisorID, now, map[string]any{"attempts": ev.Delivery.Attempts})
				}
				return nil
			}
			return missing("execution_event", x.EventID)
		case "ack_execution_event":
			var x struct {
				EventID      string `json:"event_id"`
				DeliveryID   string `json:"delivery_id"`
				SupervisorID string `json:"supervisor_id"`
				Outcome      string `json:"outcome"`
				Detail       string `json:"detail"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			for i := range st.ExecutionEvents {
				ev := &st.ExecutionEvents[i]
				if ev.ID != x.EventID {
					continue
				}
				if ev.ProductID != c.ProductID {
					return invalid("SCOPE_MISMATCH")
				}
				if ev.Delivery.Attempts == 0 || ev.Delivery.DeliveryID != x.DeliveryID || x.SupervisorID == "" || (x.Outcome != "delivered" && x.Outcome != "failed" && x.Outcome != "unsupported") || ev.Delivery.AcknowledgedAt != nil {
					return invalid("INVALID_STATE")
				}
				wasEscalated := ev.EscalationRequired
				t := now
				ev.Delivery.AcknowledgedAt = &t
				ev.Delivery.AcknowledgedBy = x.SupervisorID
				ev.Acknowledgements = append(ev.Acknowledgements, domain.ExecutionEventAck{DeliveryID: x.DeliveryID, SupervisorID: x.SupervisorID, Outcome: x.Outcome, Detail: x.Detail, At: now})
				if x.Outcome == "unsupported" {
					ev.EscalationRequired = true
				}
				out = *ev
				appendExecutionEventAudit(st, *ev, "execution_event_acknowledged", x.SupervisorID, now, map[string]any{"delivery_id": x.DeliveryID, "outcome": x.Outcome, "detail": x.Detail})
				if !wasEscalated && ev.EscalationRequired {
					appendExecutionEventAudit(st, *ev, "execution_event_escalated", x.SupervisorID, now, map[string]any{"reason": "unsupported delivery"})
				}
				return nil
			}
			return missing("execution_event", x.EventID)
		case "resolve_execution_event":
			var x struct {
				EventID      string `json:"event_id"`
				SupervisorID string `json:"supervisor_id"`
				Evidence     string `json:"evidence"`
				Fence        uint64 `json:"fence"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			for i := range st.ExecutionEvents {
				ev := &st.ExecutionEvents[i]
				if ev.ID != x.EventID {
					continue
				}
				if ev.ProductID != c.ProductID {
					return invalid("SCOPE_MISMATCH")
				}
				if ev.Resolution.ResolvedAt != nil || x.SupervisorID == "" || x.Evidence == "" {
					return invalid("INVALID_STATE")
				}
				ex := executionFor(st, ev.IntegrationID)
				if ex == nil || ex.Fence != x.Fence || ex.Reconciliation.ActionRequired && ex.Reconciliation.Generation == ev.ConditionGeneration {
					return invalid("INVALID_STATE")
				}
				t := now
				ev.Resolution = domain.ExecutionEventResolution{ResolvedAt: &t, ResolvedBy: x.SupervisorID, Evidence: x.Evidence}
				out = *ev
				appendExecutionEventAudit(st, *ev, "execution_event_resolved", x.SupervisorID, now, map[string]any{"fence": x.Fence, "evidence": x.Evidence})
				return nil
			}
			return missing("execution_event", x.EventID)
		case "claim_execution", "takeover_execution":
			in := integration(st, c.IntegrationID)
			if in == nil {
				return missing("integration", c.IntegrationID)
			}
			e := executionFor(st, c.IntegrationID)
			var x struct {
				OrchestratorID        string                  `json:"orchestrator_id"`
				AgentID               string                  `json:"agent_id"`
				Harness               domain.ExecutionHarness `json:"harness"`
				DesiredState          string                  `json:"desired_state"`
				StateCheckReference   string                  `json:"state_check_reference"`
				Fence                 uint64                  `json:"fence"`
				LeaseTTLSeconds       int                     `json:"lease_ttl_seconds"`
				StalledTimeoutSeconds *int                    `json:"stalled_timeout_seconds"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			if x.DesiredState == "" {
				x.DesiredState = "ready"
			}
			if x.DesiredState != "ready" && x.DesiredState != "released" {
				return invalid("DESIRED_STATE_INVALID")
			}
			if x.LeaseTTLSeconds != 0 && (x.LeaseTTLSeconds < 30 || x.LeaseTTLSeconds > 86400) {
				return invalid("invalid lease ttl")
			}
			if x.StalledTimeoutSeconds != nil && (*x.StalledTimeoutSeconds < 0 || *x.StalledTimeoutSeconds > 604800) {
				return invalid("invalid stalled timeout")
			}
			if x.OrchestratorID == "" || x.AgentID == "" {
				return invalid("executor identity required")
			}
			if e != nil {
				if e.ProductID != in.ProductID || (c.ProductID != "" && c.ProductID != e.ProductID) {
					return invalid("SCOPE_MISMATCH")
				}
				if c.Action == "claim_execution" && e.State == "running" && e.LeaseUntil != nil && now.Before(*e.LeaseUntil) {
					return invalid("LEASE_HELD")
				}
				if c.Action == "claim_execution" && e.State != "unassigned" && e.State != "exited_incomplete" && e.State != "paused" {
					return invalid("INVALID_STATE")
				}
				if c.Action == "takeover_execution" {
					validEvidence := false
					for _, ev := range st.ExecutionEvents {
						if ev.ID == x.StateCheckReference && ev.ProductID == in.ProductID && ev.IntegrationID == in.ID && (ev.Type == "execution_lost" || ev.Type == "execution_stalled") {
							for _, ack := range ev.Acknowledgements {
								if ack.Outcome == "delivered" {
									validEvidence = true
								}
							}
						}
					}
					if !validEvidence {
						return invalid("TAKEOVER_CHECK_REQUIRED")
					}
					if x.Fence != e.Fence {
						return invalid("STALE_FENCE")
					}
					if e.State != "lost" && e.State != "stalled" {
						return invalid("INVALID_STATE")
					}
				}
				e.Fence++
				e.LeaseUntil = nil
				e.State = "running"
				e.DesiredState = x.DesiredState
				e.OrchestratorID = x.OrchestratorID
				e.AgentID = x.AgentID
				e.Harness = x.Harness
				e.StalledTimeoutSeconds = x.StalledTimeoutSeconds
				executionCondition(st, e, "running", false, "", now)
				e.LeaseTTLSeconds = x.LeaseTTLSeconds
				if e.LeaseTTLSeconds <= 0 {
					e.LeaseTTLSeconds = 300
					if s.executionDefaults != nil {
						e.LeaseTTLSeconds = s.executionDefaults.LeaseTTL
					}
				}
				until := now.Add(time.Duration(e.LeaseTTLSeconds) * time.Second)
				e.LeaseUntil = &until
				e.ClaimedAt = &now
				e.LastHeartbeat = &now
				e.LastProgress = &now
			} else {
				if c.Action == "takeover_execution" {
					return missing("execution", c.IntegrationID)
				}
				if in.ProductID != c.ProductID {
					return invalid("SCOPE_MISMATCH")
				}
				ttl := x.LeaseTTLSeconds
				if ttl <= 0 {
					ttl = 300
					if s.executionDefaults != nil {
						ttl = s.executionDefaults.LeaseTTL
					}
				}
				until := now.Add(time.Duration(ttl) * time.Second)
				e = &domain.IntegrationExecution{ID: id(), IntegrationID: in.ID, ProductID: in.ProductID, DesiredState: x.DesiredState, State: "running", OrchestratorID: x.OrchestratorID, AgentID: x.AgentID, Harness: x.Harness, Fence: 1, LeaseTTLSeconds: ttl, LeaseUntil: &until, ClaimedAt: &now, LastHeartbeat: &now, LastProgress: &now, Reconciliation: domain.ExecutionReconciliation{Enabled: true}}
				e.StalledTimeoutSeconds = x.StalledTimeoutSeconds
				e.Reconciliation.ActionRequired = false
				st.IntegrationExecutions = append(st.IntegrationExecutions, *e)
			}
			out = *e
		case "heartbeat_execution":
			e := executionFor(st, c.IntegrationID)
			if e == nil {
				return missing("execution", c.IntegrationID)
			}
			if e.ProductID != c.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			if executionFence(c.Data, "fence") != e.Fence {
				return invalid("STALE_FENCE")
			}
			if e.State != "running" && e.State != "stalled" {
				return invalid("INVALID_STATE")
			}
			if e.LeaseUntil == nil || !now.Before(*e.LeaseUntil) {
				return invalid("LEASE_EXPIRED")
			}
			nowp := now
			e.LastHeartbeat = &nowp
			until := now.Add(time.Duration(e.LeaseTTLSeconds) * time.Second)
			e.LeaseUntil = &until
			receipt, _ := c.Data["progress_receipt"].(string)
			if len(receipt) > 256 {
				return invalid("progress receipt exceeds limit")
			}
			if receipt != "" {
				e.LastProgress = &nowp
			}
			out = *e
		case "report_execution", "release_execution":
			e := executionFor(st, c.IntegrationID)
			if e == nil {
				return missing("execution", c.IntegrationID)
			}
			if e.ProductID != c.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			if executionFence(c.Data, "fence") != e.Fence {
				return invalid("STALE_FENCE")
			}
			state, _ := c.Data["state"].(string)
			if c.Action == "release_execution" {
				state, _ = c.Data["outcome"].(string)
				if state == "" {
					state = "exited_incomplete"
				}
			}
			if state != "waiting_external" && state != "blocked" && state != "paused" && state != "cancelled" && state != "exited_incomplete" && state != "completed" {
				return invalid("INVALID_STATE")
			}
			if state == "completed" {
				in := integration(st, c.IntegrationID)
				if in == nil || !statusReachedIntegration(in.Status, e.DesiredState) {
					return invalid("INVALID_STATE")
				}
			}
			waitingRef, _ := c.Data["waiting_ref"].(string)
			blockerRef, _ := c.Data["blocker_ref"].(string)
			if state == "waiting_external" && waitingRef == "" {
				return invalid("WAITING_REFERENCE_REQUIRED")
			}
			if state == "waiting_external" {
				found := false
				for _, op := range st.Operations {
					if op.ID == waitingRef && op.ProductID == e.ProductID {
						found = true
					}
				}
				for _, b := range st.Builds {
					if b.ID == waitingRef && b.ProductID == e.ProductID {
						found = true
					}
				}
				for _, d := range st.Deployments {
					if d.ID == waitingRef && d.ProductID == e.ProductID {
						found = true
					}
				}
				if !found {
					return invalid("SCOPE_MISMATCH")
				}
			}
			if state == "blocked" && blockerRef == "" {
				return invalid("BLOCKER_REFERENCE_REQUIRED")
			}
			if state == "blocked" {
				found := false
				for _, m := range st.Memories {
					if m.ID == blockerRef && m.Kind == "blocker" {
						productID := m.ProductID
						if productID == "" {
							if f := feature(st, m.FeatureID); f != nil {
								productID = f.ProductID
							}
						}
						if productID == e.ProductID {
							found = true
						}
					}
				}
				if !found {
					return invalid("SCOPE_MISMATCH")
				}
			}
			required := state == "exited_incomplete"
			reason, _ := c.Data["reason"].(string)
			executionCondition(st, e, state, required, reason, now)
			e.ExitReason, e.WaitingRef, e.BlockerRef = reason, waitingRef, blockerRef
			if state == "paused" || state == "cancelled" || state == "exited_incomplete" || state == "completed" {
				e.LeaseUntil = nil
			}
			out = *e
		case "set_desired_state":
			e := executionFor(st, c.IntegrationID)
			if e == nil {
				return missing("execution", c.IntegrationID)
			}
			if e.ProductID != c.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			if executionFence(c.Data, "fence") != e.Fence {
				return invalid("STALE_FENCE")
			}
			d, _ := c.Data["desired_state"].(string)
			if d != "ready" && d != "released" {
				return invalid("DESIRED_STATE_INVALID")
			}
			e.DesiredState = d
			out = *e
		case "create_execution_group":
			var x struct {
				Name           string   `json:"name"`
				OrchestratorID string   `json:"orchestrator_id"`
				IntegrationIDs []string `json:"integration_ids"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			if x.Name == "" || x.OrchestratorID == "" || len(x.IntegrationIDs) == 0 {
				return invalid("group fields required")
			}
			for _, iid := range x.IntegrationIDs {
				in := integration(st, iid)
				if in == nil {
					return missing("integration", iid)
				}
				if in.ProductID != c.ProductID {
					return invalid("SCOPE_MISMATCH")
				}
				if executionFor(st, iid) == nil {
					state := "unassigned"
					required := true
					reason := "no executor assigned"
					if statusReachedIntegration(in.Status, "ready") {
						state = "completed"
						required = false
						reason = ""
					}
					st.IntegrationExecutions = append(st.IntegrationExecutions, domain.IntegrationExecution{ID: id(), IntegrationID: iid, ProductID: in.ProductID, DesiredState: "ready", State: state, Reconciliation: domain.ExecutionReconciliation{Enabled: true, ActionRequired: required, Reason: reason, Since: &now}})
				}
			}
			g := domain.ExecutionGroup{ID: id(), ProductID: c.ProductID, Name: x.Name, OrchestratorID: x.OrchestratorID, MemberIntegrationIDs: x.IntegrationIDs, CreatedAt: now, Revisions: []domain.ExecutionGroupRevision{{Revision: 1, MemberIntegrationIDs: x.IntegrationIDs, CreatedAt: now}}}
			st.ExecutionGroups = append(st.ExecutionGroups, g)
			for _, iid := range x.IntegrationIDs {
				if e := executionFor(st, iid); e != nil && e.Reconciliation.ActionRequired {
					typ := "execution_unassigned"
					createExecutionEvent(st, e, typ, e.Reconciliation.Reason, now)
				}
			}
			out = g
		case "add_execution_group_member", "remove_execution_group_member":
			gid, _ := c.Data["group_id"].(string)
			iid, _ := c.Data["integration_id"].(string)
			var g *domain.ExecutionGroup
			for i := range st.ExecutionGroups {
				if st.ExecutionGroups[i].ID == gid {
					g = &st.ExecutionGroups[i]
				}
			}
			if g == nil {
				return missing("execution_group", gid)
			}
			if c.ProductID != g.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			in := integration(st, iid)
			if in == nil {
				return missing("integration", iid)
			}
			if in.ProductID != g.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			next := append([]string{}, g.MemberIntegrationIDs...)
			idx := -1
			for i, v := range next {
				if v == iid {
					idx = i
				}
			}
			if c.Action == "add_execution_group_member" && idx < 0 {
				next = append(next, iid)
			}
			if c.Action == "remove_execution_group_member" && idx >= 0 {
				next = append(next[:idx], next[idx+1:]...)
			}
			if (c.Action == "add_execution_group_member" && idx >= 0) || (c.Action == "remove_execution_group_member" && idx < 0) {
				out = *g
				return nil
			}
			g.MemberIntegrationIDs = next
			rev := uint64(len(g.Revisions) + 1)
			g.Revisions = append(g.Revisions, domain.ExecutionGroupRevision{Revision: rev, MemberIntegrationIDs: next, CreatedAt: now})
			out = *g
		case "set_product_stalled_timeout":
			var p *domain.Product
			for i := range st.Products {
				if st.Products[i].ID == c.ProductID {
					p = &st.Products[i]
					break
				}
			}
			if p == nil {
				return missing("product", c.ProductID)
			}
			if c.Data["clear"] == true {
				p.StalledTimeoutSeconds = nil
			} else {
				v, ok := c.Data["seconds"].(float64)
				if !ok || v < 0 || v > 604800 {
					return invalid("invalid stalled timeout")
				}
				n := int(v)
				p.StalledTimeoutSeconds = &n
			}
			out = *p
		}
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: now, EntityID: func() string {
			if c.IntegrationID != "" {
				return c.IntegrationID
			}
			if c.Action == "create_execution_group" {
				if g, ok := out.(domain.ExecutionGroup); ok {
					return g.ID
				}
			}
			return c.ProductID
		}(), ProductID: c.ProductID, Data: c})
		return nil
	})
	return out, err
}

func reconcileExecutions(st *domain.State, now time.Time, globalTimeout int, debounceSeconds ...int) bool {
	changed := false
	debounce := 20
	if len(debounceSeconds) > 0 && debounceSeconds[0] > 0 {
		debounce = debounceSeconds[0]
	}
	for i := range st.IntegrationExecutions {
		e := &st.IntegrationExecutions[i]
		if !e.Reconciliation.Enabled {
			continue
		}
		in := integration(st, e.IntegrationID)
		if in == nil {
			continue
		}
		oldState, oldRequired, oldReason := e.State, e.Reconciliation.ActionRequired, e.Reconciliation.Reason
		next, required, reason := oldState, oldRequired, oldReason
		if statusReachedIntegration(in.Status, e.DesiredState) {
			next, required, reason = "completed", false, ""
		} else {
			switch e.State {
			case "running":
				required, reason = false, ""
				if e.LeaseUntil != nil && now.After(*e.LeaseUntil) {
					next, required, reason = "lost", true, "lease expired"
				} else if e.LastProgress != nil {
					timeout, _ := effectiveStalledTimeout(st, e, globalTimeout)
					if timeout > 0 && now.Sub(*e.LastProgress) > time.Duration(timeout)*time.Second {
						next, required, reason = "stalled", true, "progress timeout"
					}
				}
			case "stalled":
				timeout, _ := effectiveStalledTimeout(st, e, globalTimeout)
				if timeout == 0 || (e.LastProgress != nil && now.Sub(*e.LastProgress) <= time.Duration(timeout)*time.Second) {
					next, required, reason = "running", false, ""
				}
			case "unassigned", "lost", "exited_incomplete":
				required = true
				if reason == "" {
					reason = next
				}
			case "paused", "cancelled", "completed":
				required = false
			case "waiting_external":
				required = false
				for _, op := range st.Operations {
					if op.ID == e.WaitingRef && !operationStatusNonTerminal(op.Status) {
						required, reason = true, "external operation completed"
					}
				}
				for _, b := range st.Builds {
					if b.ID == e.WaitingRef && !buildStatusNonTerminal(b.Status) {
						required, reason = true, "build operation completed"
					}
				}
				for _, d := range st.Deployments {
					if d.ID == e.WaitingRef && !deploymentStatusNonTerminal(d.Status) {
						required, reason = true, "deployment operation completed"
					}
				}
			case "blocked":
				required = false
				for _, m := range st.Memories {
					if m.ID == e.BlockerRef && m.Kind == "blocker" && m.Status == "resolved" {
						required, reason = true, "blocker resolved"
					}
				}
			}
		}
		if next != oldState || required != oldRequired || reason != oldReason {
			executionCondition(st, e, next, required, reason, now)
			if next == "completed" {
				e.LeaseUntil = nil
			}
			changed = true
		}
	}
	// Turn-end reports are persisted on the matching execution. The sweep uses
	// them as debounce inputs and raises continuation events for unfinished
	// members of each still-open group owned by the same orchestrator.
	for i := range st.IntegrationExecutions {
		source := &st.IntegrationExecutions[i]
		if source.TurnEndAt == nil || source.TurnEndID == "" || now.Before(source.TurnEndAt.Add(time.Duration(debounce)*time.Second)) {
			continue
		}
		for _, g := range st.ExecutionGroups {
			if g.ProductID != source.ProductID || g.OrchestratorID != source.OrchestratorID || g.ClosedAt != nil {
				continue
			}
			for _, iid := range g.MemberIntegrationIDs {
				e := executionFor(st, iid)
				in := integration(st, iid)
				if e == nil || in == nil || e.ProductID != source.ProductID || e.State == "completed" || e.State == "paused" || e.State == "cancelled" || ((e.State == "waiting_external" || e.State == "blocked") && !e.Reconciliation.ActionRequired) || statusReachedIntegration(in.Status, e.DesiredState) {
					continue
				}
				if e.State == "exited_incomplete" && e.Reconciliation.ActionRequired && e.Reconciliation.Reason == "turn ended with unfinished group work" {
					continue
				}
				if e.OrchestratorID == "" {
					e.OrchestratorID = source.OrchestratorID
				}
				e.TurnEndAt = source.TurnEndAt
				e.TurnEndID = source.TurnEndID
				executionCondition(st, e, "exited_incomplete", true, "turn ended with unfinished group work", now)
				changed = true
			}
		}
	}
	return changed
}

func (s *Service) ReconcileExecutionsOnce(ctx context.Context) error {
	globalTimeout := 900
	if s.executionDefaults != nil {
		globalTimeout = s.executionDefaults.StalledTimeout
	}
	debounce := 20
	if s.executionDefaults != nil && s.executionDefaults.TurnEndDebounceSeconds > 0 {
		debounce = s.executionDefaults.TurnEndDebounceSeconds
	}
	now := time.Now().UTC()
	changed := false
	if err := s.view(ctx, func(st *domain.State) error {
		// Borrow read-only state and copy only the collection the plan mutates.
		snapshot := domain.State{IntegrationExecutions: slices.Clone(st.IntegrationExecutions), Integrations: st.Integrations, Products: st.Products, Operations: st.Operations, Builds: st.Builds, Deployments: st.Deployments, Memories: st.Memories, ExecutionGroups: st.ExecutionGroups}
		changed = reconcileExecutions(&snapshot, now, globalTimeout, debounce)
		return nil
	}); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	scoped, ok := s.store.(interface {
		UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
	})
	if !ok {
		return fmt.Errorf("execution reconciler requires UpdateScoped")
	}
	return scoped.UpdateScoped(ctx, domain.StateMutationScope{MutableCollections: []string{"integration_executions", "execution_events"}, AppendCollections: []string{"events"}}, func(st *domain.State) error {
		reconcileExecutions(st, now, globalTimeout, debounce)
		return nil
	})
}
