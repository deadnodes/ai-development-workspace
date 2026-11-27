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
		if (productID == "" || parent.ProductID == productID) && visible(parent.ProductID) && (parent.Status == "ready_for_execution" || parent.Status == "active") {
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
			if child.Assignee != "" && child.Assignee != user || child.Status == "ready" || child.Status == "released" {
				continue
			}
			item := base
			item.Kind, item.IntegrationID, item.IntegrationTitle = "integration", child.ID, child.Title
			blocker := featureStartBlocker(st, parent.ID, child.ID, visible)
			item.Reason, item.QuestionID, item.DependencyID, item.WaitingOn = blocker.Reason, blocker.QuestionID, blocker.DependencyID, blocker.WaitingOn
			if item.Reason == "" {
				if dependency := integrationWaitingOn(st, &child); dependency != "" {
					item.Reason, item.DependencyID = "integration dependency not ready", dependency
				}
			}
			execution := executions[child.ID]
			switch execution.State {
			case "", "unassigned", "lost", "exited_incomplete":
			case "blocked":
				if item.Reason == "" {
					if child.Assignee == "" || child.Assignee != user || !executionBlockerResolved(st, &execution) {
						continue
					}
					item.Kind, item.ExecutionFence, item.NextAction = "resume_integration", &execution.Fence, "report_execution with state=running and the supplied execution_fence as fence"
					appendItem(item, "execution blocker resolved")
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
		reconcileUserWork(st, now)
		return *registration, nil
	}
	st.UserOrchestrators = append(st.UserOrchestrators, domain.UserOrchestrator{Meta: workMeta(c, c.ProductID, "", now), UserID: user, Harness: harness, Products: products})
	workAudit(st, st.UserOrchestrators[len(st.UserOrchestrators)-1].Meta, c.Action, c.Actor, now)
	reconcileUserWork(st, now)
	return st.UserOrchestrators[len(st.UserOrchestrators)-1], nil
}

func retireRegistrationEvents(st *domain.State, registrationID string, now time.Time) {
	for index := range st.ExecutionEvents {
		event := &st.ExecutionEvents[index]
		if event.RegistrationID == registrationID && event.Resolution.ResolvedAt == nil {
			event.Resolution = domain.ExecutionEventResolution{ResolvedAt: &now, ResolvedBy: "system", Evidence: "registration changed"}
			appendExecutionEventAudit(st, *event, "execution_event_resolved", "system/execution-reconciler", now, nil)
		}
	}
}

func raiseWorkEvent(st *domain.State, registration *domain.UserOrchestrator, productID, reason string, cause domain.ExecutionEventCause, integrationID string, fence uint64, now time.Time) {
	registration.Generation++
	wake := domain.ExecutionWakeTarget{Kind: registration.Harness.Kind, SessionRef: registration.Harness.SessionRef, TmuxTarget: registration.Harness.TmuxTarget, Source: "user_orchestrator"}
	event := domain.ExecutionEvent{ID: id(), ProductID: productID, Owner: registration.UserID, RegistrationID: registration.ID, Type: "work_available", Cause: cause, ConditionGeneration: registration.Generation, Reason: reason, DedupeKey: fmt.Sprintf("%s|work_available|%d", registration.ID, registration.Generation), WakeTarget: wake, HarnessKind: wake.Kind, SessionRef: wake.SessionRef, TmuxTarget: wake.TmuxTarget, CreatedAt: now, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: now}}
	event.IntegrationID, event.Fence = integrationID, fence
	st.ExecutionEvents = append(st.ExecutionEvents, event)
	appendExecutionEventAudit(st, event, "execution_event_created", "system/execution-reconciler", now, map[string]any{"type": event.Type, "generation": event.ConditionGeneration})
}

func reconcileUserWork(st *domain.State, now time.Time) bool {
	changed := false
	for index := range st.UserOrchestrators {
		registration := &st.UserOrchestrators[index]
		if registration.UnregisteredAt != nil {
			continue
		}
		items, _ := nextWork(st, registration.UserID, registration.ProductID, func(productID string) bool { return registrationVisible(st, *registration, productID) }, 1)
		available := len(items) > 0
		if !available {
			for eventIndex := range st.ExecutionEvents {
				event := &st.ExecutionEvents[eventIndex]
				if event.RegistrationID == registration.ID && event.Cause == domain.ExecutionCauseQueueAvailable && event.Resolution.ResolvedAt == nil {
					event.Resolution = domain.ExecutionEventResolution{ResolvedAt: &now, ResolvedBy: "system", Evidence: "work queue drained"}
					appendExecutionEventAudit(st, *event, "execution_event_resolved", "system/execution-reconciler", now, nil)
					changed = true
				}
			}
		}
		if available == registration.Available {
			continue
		}
		registration.Available, registration.UpdatedAt, changed = available, now, true
		if available {
			raiseWorkEvent(st, registration, items[0].ProductID, "work queue became non-empty", domain.ExecutionCauseQueueAvailable, "", 0, now)
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
	wakeUserContinuation(st, owner, question.ProductID, "owner question answered", question.IntegrationID, fence, now)
}

func wakeUserContinuation(st *domain.State, owner, productID, reason, integrationID string, fence uint64, now time.Time) {
	if owner == "" {
		return
	}
	for _, scope := range []string{productID, ""} {
		for index := range st.UserOrchestrators {
			registration := &st.UserOrchestrators[index]
			if registration.UnregisteredAt == nil && registration.UserID == owner && registration.ProductID == scope && registrationVisible(st, *registration, productID) {
				for _, event := range st.ExecutionEvents {
					if event.IntegrationID == integrationID && event.Fence == fence && event.Cause == domain.ExecutionCauseQuestionContinuation && event.RegistrationID == registration.ID && event.ProductID == productID && event.CreatedAt.Equal(now) && event.Resolution.ResolvedAt == nil {
						return
					}
				}
				raiseWorkEvent(st, registration, productID, reason, domain.ExecutionCauseQuestionContinuation, integrationID, fence, now)
				return
			}
		}
	}
}
