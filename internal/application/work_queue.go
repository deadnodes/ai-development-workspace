package application

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

type workItem struct {
	Kind             string  `json:"kind"`
	ProductID        string  `json:"product_id"`
	FeatureID        string  `json:"feature_id"`
	FeatureTitle     string  `json:"feature_title"`
	IntegrationID    string  `json:"integration_id,omitempty"`
	IntegrationTitle string  `json:"integration_title,omitempty"`
	Reason           string  `json:"reason"`
	QuestionID       string  `json:"question_id,omitempty"`
	DependencyID     string  `json:"dependency_id,omitempty"`
	WaitingOn        string  `json:"waiting_on,omitempty"`
	ExecutionFence   *uint64 `json:"execution_fence,omitempty"`
	NextAction       string  `json:"next_action,omitempty"`
}

func featurePriority(parent domain.Feature) int {
	if parent.Priority == nil {
		return 100
	}
	return *parent.Priority
}

func nextWork(st *domain.State, user, productID string, visible func(string) bool, limit int) ([]workItem, []workItem) {
	items, skipped := []workItem{}, []workItem{}
	parents := []domain.Feature{}
	for _, parent := range st.Features {
		if (productID == "" || parent.ProductID == productID) && visible(parent.ProductID) {
			parents = append(parents, parent)
		}
	}
	sort.Slice(parents, func(left, right int) bool {
		first, second := parents[left], parents[right]
		if featurePriority(first) != featurePriority(second) {
			return featurePriority(first) < featurePriority(second)
		}
		if !first.CreatedAt.Equal(second.CreatedAt) {
			return first.CreatedAt.Before(second.CreatedAt)
		}
		return first.ID < second.ID
	})
	children := map[string][]domain.Integration{}
	for _, child := range st.Integrations {
		children[child.FeatureID] = append(children[child.FeatureID], child)
	}
	executions := map[string]domain.IntegrationExecution{}
	for _, execution := range st.IntegrationExecutions {
		executions[execution.IntegrationID] = execution
	}
	for _, parent := range parents {
		base := workItem{ProductID: parent.ProductID, FeatureID: parent.ID, FeatureTitle: parent.Title}
		appendItem := func(item workItem, reason string) {
			if item.Reason != "" {
				if len(skipped) < 20 {
					skipped = append(skipped, item)
				}
			} else if len(items) < limit {
				item.Reason = reason
				items = append(items, item)
			}
		}
		if len(children[parent.ID]) == 0 && parent.Status == "ready_for_execution" && (!humanWorkID(parent.Owner) || parent.Owner == user) {
			item := base
			blocker := featureStartBlocker(st, parent.ID, "", visible)
			item.Kind, item.Reason, item.QuestionID, item.DependencyID, item.WaitingOn = "plan_feature", blocker.Reason, blocker.QuestionID, blocker.DependencyID, blocker.WaitingOn
			appendItem(item, "ready feature needs integrations")
		}
		sort.Slice(children[parent.ID], func(left, right int) bool {
			first, second := children[parent.ID][left], children[parent.ID][right]
			if first.Position != second.Position {
				return first.Position < second.Position
			}
			return first.ID < second.ID
		})
		for _, child := range children[parent.ID] {
			if child.Assignee != "" && child.Assignee != user {
				continue
			}
			execution := executions[child.ID]
			resume := user != "" && child.Assignee == user && blockedExecutionPending(st, &child, &execution)
			fresh := (parent.Status == "ready_for_execution" || parent.Status == "active") && child.Status != "ready" && child.Status != "released"
			if !resume && !fresh {
				continue
			}
			item := base
			item.Kind, item.IntegrationID, item.IntegrationTitle = "integration", child.ID, child.Title
			blocker := integrationStartBlocker(st, &child, visible)
			item.Reason, item.QuestionID, item.DependencyID, item.WaitingOn = blocker.Reason, blocker.QuestionID, blocker.DependencyID, blocker.WaitingOn
			switch execution.State {
			case "", "unassigned", "lost", "exited_incomplete":
			case "blocked":
				if resume && item.Reason == "" {
					item.Kind, item.ExecutionFence, item.NextAction = "resume_integration", &execution.Fence, "report_execution with state=running and the supplied execution_fence as fence"
					appendItem(item, "execution blocker resolved")
					continue
				}
				if item.Reason == "" {
					continue
				}
			default:
				continue
			}
			appendItem(item, "eligible integration")
		}
	}
	return items, skipped
}

func registrationVisible(st *domain.State, registration domain.UserOrchestrator, productID string) bool {
	product := productByID(*st, productID)
	return product != nil && (slices.Contains(registration.Products, "*") || slices.Contains(registration.Products, productID)) && (product.Visibility != "private" || product.Owner == registration.UserID || slices.Contains(product.Members, registration.UserID))
}

func changeUserOrchestrator(ctx context.Context, st *domain.State, c domain.Command, now time.Time) (any, error) {
	if err := authorize(ctx, "write", c.ProductID); err != nil {
		return nil, err
	}
	user := workUser(ctx)
	if user == "" {
		return nil, auth.ErrForbidden
	}
	index := slices.IndexFunc(st.UserOrchestrators, func(row domain.UserOrchestrator) bool { return row.UserID == user && row.ProductID == c.ProductID })
	if c.Action == "unregister_user_orchestrator" {
		if len(c.Data) != 0 {
			return nil, invalid("unregister_user_orchestrator accepts no data fields")
		}
		if index < 0 || st.UserOrchestrators[index].UnregisteredAt != nil {
			return map[string]bool{"removed": false}, nil
		}
		registration := st.UserOrchestrators[index]
		retireRegistrationEvents(st, registration.ID, now)
		st.UserOrchestrators[index].UnregisteredAt = &now
		st.UserOrchestrators[index].UpdatedAt = now
		st.UserOrchestrators[index].Available = false
		rerouteExecutionWakes(st, now)
		workAudit(st, registration.Meta, c.Action, c.Actor, now)
		return map[string]bool{"removed": true}, nil
	}
	var input struct {
		Harness struct {
			Kind       string `json:"kind"`
			TmuxTarget string `json:"tmux_target"`
			SessionRef string `json:"session_ref"`
		} `json:"harness"`
	}
	if err := decode(c.Data, &input); err != nil {
		return nil, err
	}
	harness := domain.ExecutionHarness{Kind: input.Harness.Kind, TmuxTarget: input.Harness.TmuxTarget, SessionRef: input.Harness.SessionRef}
	if err := validateExecutionHarness(harness); err != nil {
		return nil, err
	}
	if harness.TmuxTarget == "" && harness.SessionRef == "" || len(harness.SessionRef) > 128 {
		return nil, invalid("register a harness with a tmux target or session reference")
	}
	products := []string{"*"}
	if principal, authenticated := auth.FromContext(ctx); authenticated && !instanceAdmin(principal) {
		products = slices.Clone(principal.Products)
	}
	if index >= 0 {
		previous := st.UserOrchestrators[index]
		if previous.UnregisteredAt == nil && previous.Harness == harness && slices.Equal(previous.Products, products) {
			return previous, nil
		}
		retireRegistrationEvents(st, previous.ID, now)
		registration := &st.UserOrchestrators[index]
		registration.Harness, registration.Products = harness, products
		registration.UnregisteredAt, registration.Available = nil, false
		registration.UpdatedAt, registration.Actor = now, c.Actor
		workAudit(st, registration.Meta, c.Action, c.Actor, now)
		rerouteExecutionWakes(st, now)
		reconcileUserWork(st, now)
		return *registration, nil
	}
	st.UserOrchestrators = append(st.UserOrchestrators, domain.UserOrchestrator{Meta: workMeta(c, c.ProductID, "", now), UserID: user, Harness: harness, Products: products})
	rerouteExecutionWakes(st, now)
	workAudit(st, st.UserOrchestrators[len(st.UserOrchestrators)-1].Meta, c.Action, c.Actor, now)
	reconcileUserWork(st, now)
	return st.UserOrchestrators[len(st.UserOrchestrators)-1], nil
}

func retireRegistrationEvents(st *domain.State, registrationID string, now time.Time) {
	for index := range st.ExecutionEvents {
		event := &st.ExecutionEvents[index]
		if event.RegistrationID == registrationID && event.IntegrationID == "" && event.Resolution.ResolvedAt == nil {
			event.Resolution = domain.ExecutionEventResolution{ResolvedAt: &now, ResolvedBy: "system", Evidence: "registration changed"}
			appendExecutionEventAudit(st, *event, "execution_event_resolved", "system/execution-reconciler", now, nil)
		}
	}
}

func userWakeTarget(registration domain.UserOrchestrator) domain.ExecutionWakeTarget {
	return domain.ExecutionWakeTarget{Kind: registration.Harness.Kind, SessionRef: registration.Harness.SessionRef, TmuxTarget: registration.Harness.TmuxTarget, Source: "user_orchestrator"}
}

func raiseWorkEvent(st *domain.State, registration *domain.UserOrchestrator, event domain.ExecutionEvent, now time.Time) {
	registration.Generation++
	wake := userWakeTarget(*registration)
	event.ID, event.Owner, event.RegistrationID, event.Type = id(), registration.UserID, registration.ID, "work_available"
	event.ConditionGeneration, event.DedupeKey = registration.Generation, fmt.Sprintf("%s|work_available|%d", registration.ID, registration.Generation)
	event.WakeTarget, event.HarnessKind, event.SessionRef, event.TmuxTarget = wake, wake.Kind, wake.SessionRef, wake.TmuxTarget
	event.CreatedAt, event.Delivery = now, domain.ExecutionEventDelivery{NextAttemptAt: now}
	st.ExecutionEvents = append(st.ExecutionEvents, event)
	appendExecutionEventAudit(st, event, "execution_event_created", "system/execution-reconciler", now, map[string]any{"type": event.Type, "generation": event.ConditionGeneration})
}

func registeredQueue(st *domain.State, registration domain.UserOrchestrator, productID string) []workItem {
	items, _ := nextWork(st, registration.UserID, productID, func(productID string) bool { return registrationVisible(st, registration, productID) }, 1)
	return items
}

func registrationEventEligible(st *domain.State, event domain.ExecutionEvent) bool {
	if event.Type != "work_available" || event.IntegrationID != "" && event.Cause == domain.ExecutionCauseQuestionContinuation {
		return true
	}
	for _, registration := range st.UserOrchestrators {
		if registration.ID == event.RegistrationID && registration.UnregisteredAt == nil && registration.UserID == event.Owner && (registration.ProductID == "" || registration.ProductID == event.ProductID) && registrationVisible(st, registration, event.ProductID) {
			return event.Cause != domain.ExecutionCauseQueueAvailable || len(registeredQueue(st, registration, event.ProductID)) != 0
		}
	}
	return false
}

func reconcileUserWork(st *domain.State, now time.Time) bool {
	changed := false
	for index := range st.UserOrchestrators {
		registration := &st.UserOrchestrators[index]
		if registration.UnregisteredAt != nil {
			continue
		}
		items := registeredQueue(st, *registration, registration.ProductID)
		available := len(items) > 0
		retiredQueue, pendingQueue := false, false
		for eventIndex := range st.ExecutionEvents {
			event := &st.ExecutionEvents[eventIndex]
			if event.RegistrationID != registration.ID || event.Resolution.ResolvedAt != nil {
				continue
			}
			queueWake := event.Cause == domain.ExecutionCauseQueueAvailable
			eligible := registrationVisible(st, *registration, event.ProductID) && executionEventAssignmentCurrent(st, *event)
			if queueWake && eligible {
				eligible = available && (items[0].ProductID == event.ProductID || len(registeredQueue(st, *registration, event.ProductID)) != 0)
			}
			if !eligible {
				retireExecutionEvent(st, event, now, "work is no longer eligible in this product")
				retiredQueue, changed = retiredQueue || queueWake, true
			} else {
				pendingQueue = pendingQueue || queueWake
			}
		}
		if available && !pendingQueue && (!registration.Available || retiredQueue) {
			raiseWorkEvent(st, registration, domain.ExecutionEvent{ProductID: items[0].ProductID, Reason: "work queue has eligible work", Cause: domain.ExecutionCauseQueueAvailable}, now)
			changed = true
		}
		if available != registration.Available {
			registration.Available, registration.UpdatedAt, changed = available, now, true
		}
	}
	return changed
}

func wakeQuestionOwner(st *domain.State, question domain.Memory, now time.Time) {
	owner := question.Question.AddressedTo
	var fence uint64
	if question.IntegrationID != "" {
		child := integration(st, question.IntegrationID)
		if child == nil {
			return
		}
		owner = child.Assignee
		if execution := executionFor(st, child.ID); execution != nil {
			fence = execution.Fence
		}
	}
	wakeUserContinuation(st, owner, domain.ExecutionEvent{ProductID: question.ProductID, FeatureID: question.FeatureID, IntegrationID: question.IntegrationID, Fence: fence, Reason: "owner question answered", QuestionIDs: []string{question.ID}}, now)
}

func wakeUserContinuation(st *domain.State, owner string, trigger domain.ExecutionEvent, now time.Time) {
	if owner == "" {
		return
	}
	if trigger.IntegrationID != "" {
		wake, registrationID := memberWakeTarget(st, integration(st, trigger.IntegrationID))
		for index := range st.ExecutionEvents {
			event := &st.ExecutionEvents[index]
			if event.Cause == domain.ExecutionCauseQuestionContinuation && event.ProductID == trigger.ProductID && event.IntegrationID == trigger.IntegrationID && event.Owner == owner && event.Fence == trigger.Fence && event.Resolution.ResolvedAt == nil && event.WakeTarget == wake && event.RegistrationID == registrationID {
				mergeWakeQuestions(st, event, trigger.QuestionIDs, now)
				return
			}
		}
		event := domain.ExecutionEvent{ID: id(), ProductID: trigger.ProductID, FeatureID: trigger.FeatureID, IntegrationID: trigger.IntegrationID, Fence: trigger.Fence, Reason: trigger.Reason, QuestionIDs: trigger.QuestionIDs, Cause: domain.ExecutionCauseQuestionContinuation, Type: "work_available", Owner: owner, RegistrationID: registrationID, WakeTarget: wake, SessionRef: wake.SessionRef, HarnessKind: wake.Kind, TmuxTarget: wake.TmuxTarget, EscalationRequired: wake.Kind == "", CreatedAt: now, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: now}}
		event.DedupeKey = event.ID + "|question_continuation"
		if wake.GroupID != "" {
			event.GroupIDs = []string{wake.GroupID}
		}
		st.ExecutionEvents = append(st.ExecutionEvents, event)
		appendExecutionEventAudit(st, event, "execution_event_created", "system/execution-reconciler", now, map[string]any{"type": event.Type})
		if event.EscalationRequired {
			appendExecutionEventAudit(st, event, "execution_event_escalated", "system/execution-reconciler", now, map[string]any{"reason": "no registered orchestrator harness"})
		}
		return
	}
	for _, scope := range []string{trigger.ProductID, ""} {
		for index := range st.UserOrchestrators {
			registration := &st.UserOrchestrators[index]
			if registration.UnregisteredAt == nil && registration.UserID == owner && registration.ProductID == scope && registrationVisible(st, *registration, trigger.ProductID) {
				for eventIndex := range st.ExecutionEvents {
					event := &st.ExecutionEvents[eventIndex]
					if event.IntegrationID == trigger.IntegrationID && event.FeatureID == trigger.FeatureID && event.Fence == trigger.Fence && event.Cause == domain.ExecutionCauseQuestionContinuation && event.RegistrationID == registration.ID && event.ProductID == trigger.ProductID && event.Resolution.ResolvedAt == nil {
						mergeWakeQuestions(st, event, trigger.QuestionIDs, now)
						return
					}
				}
				raiseWorkEvent(st, registration, domain.ExecutionEvent{ProductID: trigger.ProductID, FeatureID: trigger.FeatureID, IntegrationID: trigger.IntegrationID, Fence: trigger.Fence, Reason: trigger.Reason, QuestionIDs: trigger.QuestionIDs, Cause: domain.ExecutionCauseQuestionContinuation}, now)
				return
			}
		}
	}
}

func mergeWakeQuestions(st *domain.State, event *domain.ExecutionEvent, questionIDs []string, now time.Time) {
	var added []string
	for _, questionID := range questionIDs {
		if !slices.Contains(event.QuestionIDs, questionID) {
			added = append(added, questionID)
		}
	}
	if len(added) != 0 {
		event.QuestionIDs = append(slices.Clone(event.QuestionIDs), added...)
		event.Delivery.DeliveryID = ""
		appendExecutionEventAudit(st, *event, "execution_event_questions_added", "system/execution-reconciler", now, map[string]any{"question_ids": added})
	}
}
