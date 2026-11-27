package application

import (
	"context"
	"fmt"
	"regexp"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"slices"
	"strings"
	"time"
)

var tmuxTargetPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

func validateExecutionHarness(h domain.ExecutionHarness) error {
	if h.Kind != "" && h.Kind != "codex" && h.Kind != "claude_code" && h.Kind != "claude_desktop" && h.Kind != "ccr" && h.Kind != "other" {
		return invalid("invalid harness kind")
	}
	if h.TmuxTarget != "" && !tmuxTargetPattern.MatchString(h.TmuxTarget) {
		return invalid("invalid tmux target")
	}
	if (h.SessionRef != "" || h.TmuxTarget != "") && h.Kind == "" {
		return invalid("harness kind required")
	}
	return nil
}

func resolveExecutionEvents(st *domain.State, integrationID string, now time.Time, newState string) {
	for i := range st.ExecutionEvents {
		ev := &st.ExecutionEvents[i]
		if ev.IntegrationID == integrationID && ev.Resolution.ResolvedAt == nil {
			if ev.Cause == domain.ExecutionCauseQuestionContinuation && executionEventAssignmentCurrent(st, *ev) && registrationEventEligible(st, *ev) {
				continue
			}
			retireExecutionEvent(st, ev, now, newState)
		}
	}
}

func retireExecutionEvent(st *domain.State, event *domain.ExecutionEvent, now time.Time, reason string) {
	event.Resolution = domain.ExecutionEventResolution{ResolvedAt: &now, ResolvedBy: "system", Evidence: reason}
	appendExecutionEventAudit(st, *event, "execution_event_resolved", "system/execution-reconciler", now, map[string]any{"evidence": reason})
}

func createExecutionEvent(st *domain.State, e *domain.IntegrationExecution, typ, reason string, now time.Time) {
	key := fmt.Sprintf("%s|%d|%s|%d", e.IntegrationID, e.Fence, typ, e.Reconciliation.Generation)
	routeExecutionEvent(st, e, typ, reason, key, now)
}

func routeExecutionEvent(st *domain.State, e *domain.IntegrationExecution, typ, reason, key string, now time.Time) {
	for _, ev := range st.ExecutionEvents {
		if ev.DedupeKey == key {
			return
		}
	}
	child := integration(st, e.IntegrationID)
	groups := []string{}
	for _, g := range st.ExecutionGroups {
		if groupOwnsIntegration(g, child) {
			groups = append(groups, g.ID)
		}
	}
	wake, registrationID := memberWakeTarget(st, child)
	wakeGroupID := wake.GroupID
	if wakeGroupID != "" {
		for i, groupID := range groups {
			if groupID == wakeGroupID && i > 0 {
				groups = append([]string{wakeGroupID}, append(groups[:i], groups[i+1:]...)...)
				break
			}
		}
	}
	owner, featureID := "", ""
	if child != nil {
		owner, featureID = child.Assignee, child.FeatureID
	}
	event := domain.ExecutionEvent{ID: id(), ProductID: e.ProductID, FeatureID: featureID, IntegrationID: e.IntegrationID, GroupIDs: groups, Type: typ, Cause: e.Reconciliation.Cause, Owner: owner, RegistrationID: registrationID, Fence: e.Fence, ConditionGeneration: e.Reconciliation.Generation, Reason: reason, DedupeKey: key, WakeTarget: wake, SessionRef: wake.SessionRef, HarnessKind: wake.Kind, TmuxTarget: wake.TmuxTarget, CreatedAt: now, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: now}, EscalationRequired: wake.Kind == ""}
	if event.Cause == domain.ExecutionCauseBlockerResolved && questionByID(st, e.BlockerRef) != nil {
		event.QuestionIDs = []string{e.BlockerRef}
	}
	st.ExecutionEvents = append(st.ExecutionEvents, event)
	appendExecutionEventAudit(st, event, "execution_event_created", "system/execution-reconciler", now, map[string]any{"type": typ, "generation": event.ConditionGeneration})
	if event.EscalationRequired {
		appendExecutionEventAudit(st, event, "execution_event_escalated", "system/execution-reconciler", now, map[string]any{"reason": "no registered orchestrator harness"})
	}
}

func firstGroupWake(st *domain.State, child *domain.Integration) domain.ExecutionWakeTarget {
	for _, group := range st.ExecutionGroups {
		if groupOwnsIntegration(group, child) && group.OrchestratorHarness.Kind != "" && (group.OrchestratorHarness.SessionRef != "" || group.OrchestratorHarness.TmuxTarget != "") {
			return groupWakeTarget(group)
		}
	}
	return domain.ExecutionWakeTarget{}
}

func memberWakeTarget(st *domain.State, child *domain.Integration) (domain.ExecutionWakeTarget, string) {
	if wake := firstGroupWake(st, child); wake.Kind != "" {
		return wake, ""
	}
	if child != nil && child.Assignee != "" {
		for _, scope := range []string{child.ProductID, ""} {
			for _, registration := range st.UserOrchestrators {
				if registration.UnregisteredAt == nil && registration.UserID == child.Assignee && registration.ProductID == scope && registrationVisible(st, registration, child.ProductID) {
					return userWakeTarget(registration), registration.ID
				}
			}
		}
	}
	return domain.ExecutionWakeTarget{}, ""
}

func groupWakeTarget(group domain.ExecutionGroup) domain.ExecutionWakeTarget {
	harness := group.OrchestratorHarness
	return domain.ExecutionWakeTarget{Kind: harness.Kind, SessionRef: harness.SessionRef, TmuxTarget: harness.TmuxTarget, Source: "group_orchestrator", GroupID: group.ID, HarnessRevision: group.HarnessRevision}
}

func groupContainsIntegration(group domain.ExecutionGroup, child *domain.Integration) bool {
	return child != nil && child.ProductID == group.ProductID && group.ClosedAt == nil && slices.Contains(group.MemberIntegrationIDs, child.ID)
}

func groupOwnsIntegration(group domain.ExecutionGroup, child *domain.Integration) bool {
	return groupContainsIntegration(group, child) && group.Owner == child.Assignee
}

func executionAssignmentUser(ctx context.Context) string {
	if _, authenticated := auth.FromContext(ctx); authenticated {
		return workUser(ctx)
	}
	return ""
}

// adoptLegacyGroupOwner records the human owner of an execution group created
// before group ownership was stored. The owner is the caller's human (or, for a
// trusted service caller, the assignee of the integration being added) and is
// adopted only when every current member is unassigned or assigned to that
// human, so a legacy group never captures another user's work.
func adoptLegacyGroupOwner(ctx context.Context, st *domain.State, g *domain.ExecutionGroup, candidate string, now time.Time) {
	if g.Owner != "" {
		return
	}
	if user, enforced := principalUser(ctx); enforced {
		candidate = user
	}
	if candidate == "" {
		return
	}
	for _, id := range g.MemberIntegrationIDs {
		if in := integration(st, id); in != nil && in.Assignee != "" && in.Assignee != candidate {
			return
		}
	}
	g.Owner = candidate
	st.Events = append(st.Events, domain.Event{ID: id(), Action: "adopt_execution_group_owner", Actor: candidate, At: now, EntityID: g.ID, ProductID: g.ProductID, Data: domain.Command{Action: "adopt_execution_group_owner", Actor: candidate, ProductID: g.ProductID, ID: g.ID, Data: map[string]any{"owner": candidate}}})
}

func requireExecutionGroupOwner(ctx context.Context, group domain.ExecutionGroup) error {
	if user, enforced := principalUser(ctx); enforced && (user == "" || user != group.Owner) {
		return notAssignee(group.Owner)
	}
	return nil
}

func groupWakeCurrent(st *domain.State, event domain.ExecutionEvent) bool {
	if !domain.IsGroupIdleEvent(event.Type) {
		if event.IntegrationID == "" {
			return event.WakeTarget.Source != "executor"
		}
		wake, registrationID := memberWakeTarget(st, integration(st, event.IntegrationID))
		return event.WakeTarget == wake && event.RegistrationID == registrationID
	}
	return slices.ContainsFunc(st.ExecutionGroups, func(group domain.ExecutionGroup) bool {
		return group.ClosedAt == nil && group.Owner == event.Owner && group.ProductID == event.ProductID && event.WakeTarget == groupWakeTarget(group)
	})
}

func executionEventAssignmentCurrent(st *domain.State, event domain.ExecutionEvent) bool {
	if event.IntegrationID == "" {
		return true
	}
	child := integration(st, event.IntegrationID)
	var fence uint64
	if execution := executionFor(st, event.IntegrationID); execution != nil {
		fence = execution.Fence
	}
	return child != nil && child.Assignee == event.Owner && fence == event.Fence
}

func rerouteExecutionWakes(st *domain.State, now time.Time) bool {
	changed := false
	for index := range st.ExecutionEvents {
		event := &st.ExecutionEvents[index]
		if event.IntegrationID == "" || event.Resolution.ResolvedAt != nil || groupWakeCurrent(st, *event) {
			continue
		}
		retireExecutionEvent(st, event, now, "orchestrator wake target changed")
		changed = true
		execution := executionFor(st, event.IntegrationID)
		child := integration(st, event.IntegrationID)
		if event.Cause == domain.ExecutionCauseQuestionContinuation && executionEventAssignmentCurrent(st, *event) {
			wakeUserContinuation(st, event.Owner, *event, now)
			continue
		}
		if execution == nil || child == nil || child.Assignee != event.Owner || !execution.Reconciliation.ActionRequired || execution.Fence != event.Fence || execution.Reconciliation.Generation != event.ConditionGeneration || event.Cause == domain.ExecutionCauseBlockerResolved && !blockedExecutionPending(st, child, execution) {
			continue
		}
		routeExecutionEvent(st, execution, event.Type, event.Reason, "reroute|"+event.ID, now)
	}
	return changed
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
	case "set_product_idle_policy", "report_turn_start":
		return true
	case "claim_execution", "heartbeat_execution", "report_execution", "release_execution", "takeover_execution", "set_desired_state", "create_execution_group", "add_execution_group_member", "remove_execution_group_member", "set_execution_group_harness", "set_product_stalled_timeout", "report_turn_end", "claim_execution_event_delivery", "ack_execution_event", "resolve_execution_event":
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

func validateExecutionInput(data map[string]any, action string, accepted ...string) error {
	allowed := map[string]bool{}
	for _, key := range accepted {
		allowed[key] = true
	}
	for key := range data {
		if !allowed[key] {
			return invalid("unknown field %s; accepted: %s", key, strings.Join(accepted, ", "))
		}
	}
	for _, key := range []string{"reason", "note"} {
		if value, ok := data[key]; ok {
			text, valid := value.(string)
			if !valid || len(text) > 500 {
				return invalid("%s must be a string of at most 500 characters", key)
			}
		}
	}
	return nil
}
func executionCondition(st *domain.State, e *domain.IntegrationExecution, state string, required bool, reason string, now time.Time) {
	executionConditionWithCause(st, e, state, required, reason, "", now)
}

func checkExecutionRenewal(ctx context.Context, st *domain.State, execution *domain.IntegrationExecution, now time.Time) error {
	if execution.State != "running" {
		child := integration(st, execution.IntegrationID)
		if err := guardFeatureStart(ctx, st, child); err != nil {
			return err
		}
		if execution.BlockerFence != nil && *execution.BlockerFence == execution.Fence && slices.ContainsFunc(st.Memories, func(memory domain.Memory) bool {
			return memory.ID == execution.BlockerRef && memory.Kind == "blocker" && memory.Status == "open" && executionBlockerInScope(memory, child)
		}) {
			return invalid("OPEN_BLOCKER: %s", execution.BlockerRef)
		}
		return nil
	}
	if execution.LeaseUntil == nil || !now.Before(*execution.LeaseUntil) {
		return invalid("LEASE_EXPIRED")
	}
	return nil
}

func executionConditionWithCause(st *domain.State, e *domain.IntegrationExecution, state string, required bool, reason string, cause domain.ExecutionEventCause, now time.Time) {
	if e.State == state && e.Reconciliation.ActionRequired == required && e.Reconciliation.Reason == reason && e.Reconciliation.Cause == cause {
		return
	}
	e.State = state
	if state != "blocked" {
		e.BlockerFence = nil
	}
	e.Reconciliation.ActionRequired = required
	e.Reconciliation.Reason = reason
	e.Reconciliation.Cause = cause
	e.Reconciliation.Since = &now
	e.Reconciliation.Generation++
	resolveExecutionEvents(st, e.IntegrationID, now, state)
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
			allowUnassigned := c.Action == "claim_execution"
			if err := requireIntegrationUser(ctx, in, allowUnassigned); err != nil {
				return err
			}
		}
		switch c.Action {
		case "report_turn_end", "report_turn_start":
			var x struct {
				HarnessKind string `json:"harness_kind"`
				SessionRef  string `json:"session_ref"`
				TmuxTarget  string `json:"tmux_target"`
				TurnID      string `json:"turn_id"`
				CWD         string `json:"cwd"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			if x.SessionRef == "" && x.TmuxTarget == "" || x.HarnessKind == "" {
				return invalid("harness_kind and session_ref or tmux_target required")
			}
			if err := validateExecutionHarness(domain.ExecutionHarness{Kind: x.HarnessKind, SessionRef: x.SessionRef, TmuxTarget: x.TmuxTarget}); err != nil {
				return err
			}
			bucket := now.Truncate(time.Minute).Format(time.RFC3339)
			identity := x.SessionRef
			if identity == "" {
				identity = x.TmuxTarget
			}
			if x.SessionRef != "" && x.TmuxTarget != "" {
				identity += "|" + x.TmuxTarget
			}
			turn := x.TurnID
			if turn == "" {
				turn = bucket
			}
			key := x.HarnessKind + "|" + identity + "|" + turn
			matched := []string{}
			isEnd := c.Action == "report_turn_end"
			for i := 0; isEnd && i < len(st.IntegrationExecutions); i++ {
				e := &st.IntegrationExecutions[i]
				if e.ProductID == c.ProductID && executionOwnedByCaller(ctx, integration(st, e.IntegrationID)) && e.Harness.Kind == x.HarnessKind && ((x.SessionRef != "" && e.Harness.SessionRef == x.SessionRef) || (x.TmuxTarget != "" && e.Harness.TmuxTarget == x.TmuxTarget)) {
					if e.TurnEndID == key {
						matched = append(matched, e.IntegrationID)
						continue
					}
					e.TurnEndAt = &now
					e.TurnEndID = key
					matched = append(matched, e.IntegrationID)
				}
			}
			// The orchestrator session is registered on its group and can differ
			// from every member executor. Fan its turn-end signal into the open
			// group's member execution records so reconciliation can debounce it.
			for index := range st.ExecutionGroups {
				g := &st.ExecutionGroups[index]
				if g.ProductID != c.ProductID || g.ClosedAt != nil || g.OrchestratorHarness.Kind != x.HarnessKind || !((x.SessionRef != "" && g.OrchestratorHarness.SessionRef == x.SessionRef) || (x.TmuxTarget != "" && g.OrchestratorHarness.TmuxTarget == x.TmuxTarget)) {
					continue
				}
				if requireExecutionGroupOwner(ctx, *g) != nil {
					continue
				}
				if !isEnd {
					if g.TurnStartID != key && (x.TurnID == "" || g.TurnEndID != key) {
						g.TurnStartAt, g.TurnStartID = &now, key
					}
					continue
				}
				if g.TurnEndID != key {
					g.TurnEndAt, g.TurnEndID = &now, key
				}
				for _, iid := range g.MemberIntegrationIDs {
					e := executionFor(st, iid)
					child := integration(st, iid)
					if e == nil || e.ProductID != c.ProductID || !groupOwnsIntegration(*g, child) || !executionOwnedByCaller(ctx, child) {
						continue
					}
					if e.TurnEndID != key {
						e.TurnEndAt = &now
						e.TurnEndID = key
					}
					if e.OrchestratorID == "" {
						e.OrchestratorID = g.OrchestratorID
					}
					matched = append(matched, e.IntegrationID)
				}
			}
			slices.Sort(matched)
			matched = slices.Compact(matched)
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
				if err := requireExecutionEventUser(ctx, st, ev); err != nil {
					return err
				}
				if x.SupervisorID == "" || ev.Resolution.ResolvedAt != nil || now.Before(ev.Delivery.NextAttemptAt) || !groupWakeCurrent(st, *ev) || !registrationEventEligible(st, *ev) {
					return invalid("INVALID_STATE")
				}
				idleEvent := domain.IsGroupIdleEvent(ev.Type)
				if idleEvent && !groupIdleEventCurrent(st, *ev, now) {
					return invalid("INVALID_STATE")
				}
				if ev.Cause == domain.ExecutionCauseBlockerResolved {
					child, execution := integration(st, ev.IntegrationID), executionFor(st, ev.IntegrationID)
					if !blockedExecutionPending(st, child, execution) || execution.Reconciliation.Generation != ev.ConditionGeneration || guardFeatureStart(ctx, st, child) != nil {
						return invalid("INVALID_STATE")
					}
				}
				if ev.Owner != "" && !executionEventAssignmentCurrent(st, *ev) {
					return invalid("INVALID_STATE")
				}
				beforeEscalation := ev.EscalationRequired
				ev.Delivery.Attempts++
				ev.Delivery.LastAttemptAt = &now
				ev.Delivery.DeliveryID = id()
				retry, cap, escalate := executionNotifyConfig(s)
				delay := retry
				attempts := groupIdleEffectiveAttempts(*ev)
				for j := 1; j < attempts && delay < cap; j++ {
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
				if (!idleEvent && attempts >= escalate) || (idleEvent && attempts-1 >= escalate) {
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
				if err := requireExecutionEventUser(ctx, st, ev); err != nil {
					return err
				}
				idleEvent := domain.IsGroupIdleEvent(ev.Type)
				if ev.Delivery.Attempts == 0 || x.DeliveryID == "" || ev.Delivery.DeliveryID != x.DeliveryID || x.SupervisorID == "" || (x.Outcome != "delivered" && x.Outcome != "failed" && x.Outcome != "unsupported" && !(idleEvent && x.Outcome == "skipped_active")) || ev.Delivery.AcknowledgedAt != nil {
					return invalid("INVALID_STATE")
				}
				wasEscalated := ev.EscalationRequired
				t := now
				ev.Delivery.AcknowledgedAt = &t
				ev.Delivery.AcknowledgedBy = x.SupervisorID
				ev.Acknowledgements = append(ev.Acknowledgements, domain.ExecutionEventAck{DeliveryID: x.DeliveryID, SupervisorID: x.SupervisorID, Outcome: x.Outcome, Detail: x.Detail, At: now})
				if idleEvent {
					policy := idlePolicy(st, ev.ProductID)
					switch x.Outcome {
					case "skipped_active":
						delay := policy.IdleAfterSeconds
						if delay == 0 {
							delay = 600
						}
						ev.Delivery.NextAttemptAt = now.Add(time.Duration(delay) * time.Second)
					case "delivered":
						if ev.Type == domain.EventGroupOwnerBlocked {
							ev.Delivery.NextAttemptAt = now.Add(time.Duration(policy.OwnerBlockedIntervalSeconds) * time.Second)
						}
					case "failed":
						_, _, threshold := executionNotifyConfig(s)
						ev.EscalationRequired = ev.EscalationRequired || groupIdleEffectiveAttempts(*ev) >= threshold
					}
				}
				if ev.Type == "work_available" && x.Outcome == "delivered" && ev.Resolution.ResolvedAt == nil {
					ev.Resolution = domain.ExecutionEventResolution{ResolvedAt: &now, ResolvedBy: x.SupervisorID, Evidence: "wake delivered"}
					appendExecutionEventAudit(st, *ev, "execution_event_resolved", x.SupervisorID, now, nil)
				}
				if x.Outcome == "delivered" && ev.Resolution.ResolvedAt == nil {
					ex := executionFor(st, ev.IntegrationID)
					if ex != nil && ex.Reconciliation.ActionRequired && ex.Reconciliation.Generation == ev.ConditionGeneration {
						threshold := 3
						if s.executionDefaults != nil && s.executionDefaults.EscalateAfterDelivered > 0 {
							threshold = s.executionDefaults.EscalateAfterDelivered
						}
						delivered := 0
						for _, ack := range ev.Acknowledgements {
							if ack.Outcome == "delivered" {
								delivered++
							}
						}
						if delivered >= threshold {
							ev.EscalationRequired = true
						}
					}
				}
				if x.Outcome == "unsupported" {
					ev.EscalationRequired = true
				}
				out = *ev
				appendExecutionEventAudit(st, *ev, "execution_event_acknowledged", x.SupervisorID, now, map[string]any{"delivery_id": x.DeliveryID, "outcome": x.Outcome, "detail": x.Detail})
				if !wasEscalated && ev.EscalationRequired {
					reason := "unsupported delivery"
					if idleEvent && x.Outcome == "failed" {
						reason = "idle wake delivery attempts exhausted"
					}
					appendExecutionEventAudit(st, *ev, "execution_event_escalated", x.SupervisorID, now, map[string]any{"reason": reason})
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
				if err := requireExecutionEventUser(ctx, st, ev); err != nil {
					return err
				}
				if domain.IsGroupIdleEvent(ev.Type) || ev.Resolution.ResolvedAt != nil || x.SupervisorID == "" || x.Evidence == "" {
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
			if c.Action == "claim_execution" {
				if err := validateExecutionInput(c.Data, c.Action, "orchestrator_id", "agent_id", "harness", "desired_state", "lease_ttl_seconds", "stalled_timeout_seconds", "reason", "note"); err != nil {
					return err
				}
			} else if err := validateExecutionInput(c.Data, c.Action, "orchestrator_id", "agent_id", "harness", "desired_state", "state_check_reference", "fence", "lease_ttl_seconds", "stalled_timeout_seconds", "reason", "note"); err != nil {
				return err
			}
			in := integration(st, c.IntegrationID)
			if in == nil {
				return missing("integration", c.IntegrationID)
			}
			if c.Action == "claim_execution" {
				if err := requireIntegrationUser(ctx, in, true); err != nil {
					return err
				}
			}
			if err := guardFeatureStart(ctx, st, in); err != nil {
				return err
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
			if x.Harness.Kind != "" && x.Harness.Kind != "codex" && x.Harness.Kind != "claude_code" && x.Harness.Kind != "claude_desktop" && x.Harness.Kind != "ccr" && x.Harness.Kind != "other" {
				return invalid("invalid harness kind")
			}
			if x.Harness.TmuxTarget != "" && !tmuxTargetPattern.MatchString(x.Harness.TmuxTarget) {
				return invalid("invalid tmux target")
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
					if (x.OrchestratorID == "" || x.OrchestratorID == e.OrchestratorID) && (x.AgentID == "" || x.AgentID == e.AgentID) {
						return invalid("TAKEOVER_NOT_REQUIRED")
					}
				}
				e.Fence++
				e.LeaseUntil = nil
				e.TurnEndAt = nil
				e.State = "running"
				e.DesiredState = x.DesiredState
				e.OrchestratorID = x.OrchestratorID
				e.AgentID = x.AgentID
				e.LeasePrincipal = c.Actor
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
				e.LeasePrincipal = c.Actor
				st.IntegrationExecutions = append(st.IntegrationExecutions, *e)
			}
			assignUnassigned(in, executionAssignmentUser(ctx), c.Actor, now)
			detachIncompatibleExecutionGroups(st, in, now)
			resolveExecutionEvents(st, in.ID, now, "execution claimed")
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
			if e.State != "running" && e.State != "stalled" && e.State != "lost" {
				return invalid("INVALID_STATE")
			}
			if err := checkExecutionRenewal(ctx, st, e, now); err != nil {
				return err
			}
			if e.State == "lost" || e.State == "stalled" {
				executionCondition(st, e, "running", false, "", now)
			}
			nowp := now
			e.LastHeartbeat = &nowp
			until := extendLease(now, time.Duration(e.LeaseTTLSeconds)*time.Second, e.LeaseUntil)
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
			var inputErr error
			if c.Action == "report_execution" {
				inputErr = validateExecutionInput(c.Data, c.Action, "fence", "state", "waiting_ref", "blocker_ref", "reason", "note")
			} else {
				inputErr = validateExecutionInput(c.Data, c.Action, "fence", "outcome", "reason", "note")
			}
			if inputErr != nil {
				return inputErr
			}
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
			if state != "running" && state != "waiting_external" && state != "blocked" && state != "paused" && state != "cancelled" && state != "exited_incomplete" && state != "completed" {
				return invalid("INVALID_STATE")
			}
			if state == "running" {
				if err := checkExecutionRenewal(ctx, st, e, now); err != nil {
					return err
				}
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
				child := integration(st, e.IntegrationID)
				if !slices.ContainsFunc(st.Memories, func(memory domain.Memory) bool {
					return memory.ID == blockerRef && executionBlockerInScope(memory, child) && (memory.Kind == "blocker" && memory.Status == "open" || questionBlocked(memory))
				}) {
					return invalid("SCOPE_MISMATCH")
				}
				fence := e.Fence
				e.BlockerFence = &fence
			}
			required := state == "exited_incomplete"
			reason, _ := c.Data["reason"].(string)
			executionCondition(st, e, state, required, reason, now)
			e.ExitReason, e.WaitingRef, e.BlockerRef = reason, waitingRef, blockerRef
			if state == "running" {
				until := extendLease(now, time.Duration(e.LeaseTTLSeconds)*time.Second, e.LeaseUntil)
				e.LeaseUntil = &until
				e.LastHeartbeat = &now
				e.LastProgress = &now
			} else if state == "paused" || state == "cancelled" || state == "exited_incomplete" || state == "completed" {
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
				Name           string                  `json:"name"`
				OrchestratorID string                  `json:"orchestrator_id"`
				IntegrationIDs []string                `json:"integration_ids"`
				Harness        domain.ExecutionHarness `json:"orchestrator_harness"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			if x.Name == "" || x.OrchestratorID == "" || len(x.IntegrationIDs) == 0 {
				return invalid("group fields required")
			}
			if err := validateExecutionHarness(x.Harness); err != nil {
				return err
			}
			owner := executionAssignmentUser(ctx)
			for _, iid := range x.IntegrationIDs {
				in := integration(st, iid)
				if in == nil {
					return missing("integration", iid)
				}
				if in.ProductID != c.ProductID {
					return invalid("SCOPE_MISMATCH")
				}
				if err := requireIntegrationUser(ctx, in, true); err != nil {
					return err
				}
				if in.Assignee != "" && in.Assignee != owner {
					return notAssignee(in.Assignee)
				}
				assignUnassigned(in, owner, c.Actor, now)
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
			g := domain.ExecutionGroup{ID: id(), ProductID: c.ProductID, Owner: owner, Name: x.Name, OrchestratorID: x.OrchestratorID, OrchestratorHarness: x.Harness, MemberIntegrationIDs: x.IntegrationIDs, CreatedAt: now, Revisions: []domain.ExecutionGroupRevision{{Revision: 1, MemberIntegrationIDs: x.IntegrationIDs, CreatedAt: now}}}
			if x.Harness != (domain.ExecutionHarness{}) {
				g.HarnessRevision = 1
			}
			st.ExecutionGroups = append(st.ExecutionGroups, g)
			rerouteExecutionWakes(st, now)
			for _, iid := range x.IntegrationIDs {
				if e := executionFor(st, iid); e != nil && e.State == "unassigned" && e.Reconciliation.ActionRequired {
					typ := "execution_unassigned"
					createExecutionEvent(st, e, typ, e.Reconciliation.Reason, now)
				}
			}
			out = g
		case "set_execution_group_harness":
			gid, _ := c.Data["group_id"].(string)
			var harness domain.ExecutionHarness
			harnessData, ok := c.Data["harness"].(map[string]any)
			if !ok {
				return invalid("harness required")
			}
			if err := decode(harnessData, &harness); err != nil {
				return err
			}
			if err := validateExecutionHarness(harness); err != nil {
				return err
			}
			var g *domain.ExecutionGroup
			for i := range st.ExecutionGroups {
				if st.ExecutionGroups[i].ID == gid {
					g = &st.ExecutionGroups[i]
					break
				}
			}
			if g == nil {
				return missing("execution_group", gid)
			}
			if c.ProductID != g.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			adoptLegacyGroupOwner(ctx, st, g, "", now)
			if err := requireExecutionGroupOwner(ctx, *g); err != nil {
				return err
			}
			if g.OrchestratorHarness != harness {
				g.OrchestratorHarness = harness
				g.HarnessRevision++
				g.HarnessChangedAt = &now
				g.TurnEndAt, g.TurnStartAt = nil, nil
				g.TurnEndID, g.TurnStartID = "", ""
				rerouteExecutionWakes(st, now)
			}
			out = *g
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
			adoptLegacyGroupOwner(ctx, st, g, in.Assignee, now)
			if err := requireExecutionGroupOwner(ctx, *g); err != nil {
				return err
			}
			if c.Action == "add_execution_group_member" {
				if g.ClosedAt != nil {
					return invalid("INVALID_STATE")
				}
				if in.Assignee != "" && in.Assignee != g.Owner {
					return notAssignee(in.Assignee)
				}
				assignUnassigned(in, g.Owner, c.Actor, now)
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
			rerouteExecutionWakes(st, now)
			out = *g
		case "set_product_stalled_timeout", "set_product_idle_policy":
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
			if c.Action == "set_product_idle_policy" {
				if err := setProductIdlePolicy(ctx, p, c.Data); err != nil {
					return err
				}
			} else if c.Data["clear"] == true {
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
		cause := e.Reconciliation.Cause
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
				if (timeout == 0 || (e.LastProgress != nil && now.Sub(*e.LastProgress) <= time.Duration(timeout)*time.Second)) && guardFeatureStart(context.Background(), st, in) == nil {
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
				required, reason, cause = false, "", ""
				if blockedExecutionPending(st, in, e) && guardFeatureStart(context.Background(), st, in) == nil {
					required, reason, cause = true, "blocker resolved", domain.ExecutionCauseBlockerResolved
				}
			}
		}
		if next != "blocked" {
			cause = ""
		}
		if next != oldState || required != oldRequired || reason != oldReason || cause != e.Reconciliation.Cause {
			executionConditionWithCause(st, e, next, required, reason, cause, now)
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
		source := st.IntegrationExecutions[i]
		if source.TurnEndAt == nil || source.TurnEndID == "" || now.Before(source.TurnEndAt.Add(time.Duration(debounce)*time.Second)) {
			continue
		}
		st.IntegrationExecutions[i].TurnEndAt = nil
		changed = true
		for _, g := range st.ExecutionGroups {
			if g.OrchestratorID != source.OrchestratorID || !groupOwnsIntegration(g, integration(st, source.IntegrationID)) {
				continue
			}
			for _, iid := range g.MemberIntegrationIDs {
				e := executionFor(st, iid)
				in := integration(st, iid)
				if e == nil || !groupOwnsIntegration(g, in) || e.ProductID != source.ProductID || e.State == "completed" || e.State == "paused" || e.State == "cancelled" || e.State == "blocked" || (e.State == "waiting_external" && !e.Reconciliation.ActionRequired) || statusReachedIntegration(in.Status, e.DesiredState) {
					continue
				}
				if e.ClaimedAt != nil && e.ClaimedAt.After(*source.TurnEndAt) {
					continue
				}
				if e.State == "exited_incomplete" && e.Reconciliation.ActionRequired && e.Reconciliation.Reason == "turn ended with unfinished group work" {
					continue
				}
				if e.OrchestratorID == "" {
					e.OrchestratorID = source.OrchestratorID
				}
				e.TurnEndID = source.TurnEndID
				executionCondition(st, e, "exited_incomplete", true, "turn ended with unfinished group work", now)
				e.TurnEndAt = nil
			}
		}
	}
	changed = rerouteExecutionWakes(st, now) || changed
	return reconcileGroupIdle(st, now) || changed
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
		snapshot := domain.State{IntegrationExecutions: slices.Clone(st.IntegrationExecutions), Integrations: st.Integrations, Products: st.Products, Operations: st.Operations, Builds: st.Builds, Deployments: st.Deployments, Memories: st.Memories, ExecutionGroups: st.ExecutionGroups, ExecutionEvents: slices.Clone(st.ExecutionEvents), Features: st.Features, FeatureDependencies: st.FeatureDependencies, UserOrchestrators: slices.Clone(st.UserOrchestrators)}
		changed = reconcileExecutions(&snapshot, now, globalTimeout, debounce)
		changed = reconcileUserWork(&snapshot, now) || changed
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
	return scoped.UpdateScoped(ctx, domain.StateMutationScope{MutableCollections: []string{"integration_executions", "execution_events", "user_orchestrators"}, AppendCollections: []string{"events"}}, func(st *domain.State) error {
		reconcileExecutions(st, now, globalTimeout, debounce)
		reconcileUserWork(st, now)
		return nil
	})
}

func leasePrincipal(execution *domain.IntegrationExecution) string {
	if execution.LeasePrincipal != "" {
		return execution.LeasePrincipal
	}
	return execution.AgentID
}

func executionBlockerInScope(memory domain.Memory, child *domain.Integration) bool {
	return child != nil && (memory.ProductID == child.ProductID || memory.ProductID == "") && memory.FeatureID == child.FeatureID && (memory.IntegrationID == child.ID || memory.IntegrationID == "")
}

func blockedExecutionPending(st *domain.State, child *domain.Integration, execution *domain.IntegrationExecution) bool {
	if child == nil || execution == nil || execution.State != "blocked" || execution.BlockerFence == nil || *execution.BlockerFence != execution.Fence || statusReachedIntegration(child.Status, execution.DesiredState) {
		return false
	}
	return slices.ContainsFunc(st.Memories, func(memory domain.Memory) bool {
		return memory.ID == execution.BlockerRef && executionBlockerInScope(memory, child) && (memory.Kind == "blocker" && memory.Status == "resolved" || memory.Kind == "question" && memory.Question != nil && memory.Question.Blocking && (memory.Status == "answered" || memory.Status == "withdrawn"))
	})
}

func wakeResolvedQuestion(st *domain.State, question domain.Memory, now time.Time) bool {
	found := false
	for index := range st.IntegrationExecutions {
		execution := &st.IntegrationExecutions[index]
		if execution.ProductID != question.ProductID || execution.State != "blocked" || execution.BlockerRef != question.ID {
			continue
		}
		found = true
		if child := integration(st, execution.IntegrationID); blockedExecutionPending(st, child, execution) && guardFeatureStart(context.Background(), st, child) == nil {
			executionConditionWithCause(st, execution, "blocked", true, "blocker resolved", domain.ExecutionCauseBlockerResolved, now)
		}
	}
	return found
}

func executionEventVisibleTo(ctx context.Context, st *domain.State, event domain.ExecutionEvent) bool {
	if domain.IsGroupIdleEvent(event.Type) {
		return groupIdleOwnerVisible(ctx, event.Owner)
	}
	if event.Type == "work_available" {
		return event.Owner != "" && event.Owner == workUser(ctx)
	}
	return executionVisibleTo(ctx, integration(st, event.IntegrationID))
}

func requireExecutionEventUser(ctx context.Context, st *domain.State, event *domain.ExecutionEvent) error {
	if event.Type == "work_available" || domain.IsGroupIdleEvent(event.Type) {
		if !executionEventVisibleTo(ctx, st, *event) {
			return missing("execution_event", event.ID)
		}
		return nil
	}
	return requireIntegrationUser(ctx, integration(st, event.IntegrationID), false)
}
