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
	Kind             string `json:"kind"`
	ProductID        string `json:"product_id"`
	FeatureID        string `json:"feature_id"`
	FeatureTitle     string `json:"feature_title"`
	IntegrationID    string `json:"integration_id,omitempty"`
	IntegrationTitle string `json:"integration_title,omitempty"`
	Reason           string `json:"reason"`
	QuestionID       string `json:"question_id,omitempty"`
	DependencyID     string `json:"dependency_id,omitempty"`
	WaitingOn        string `json:"waiting_on,omitempty"`
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
	statuses := map[string]string{}
	for _, child := range st.Integrations {
		children[child.FeatureID] = append(children[child.FeatureID], child)
		statuses[child.ID] = child.Status
	}
	executions := map[string]domain.IntegrationExecution{}
	for _, execution := range st.IntegrationExecutions {
		executions[execution.IntegrationID] = execution
	}
	featureQuestions, integrationQuestions := map[string]string{}, map[string]string{}
	for _, question := range st.Memories {
		if questionBlocked(question) {
			if question.IntegrationID == "" {
				featureQuestions[question.FeatureID] = question.ID
			} else {
				integrationQuestions[question.IntegrationID] = question.ID
			}
		}
	}
	for _, parent := range parents {
		base := workItem{ProductID: parent.ProductID, FeatureID: parent.ID, FeatureTitle: parent.Title}
		if questionID := featureQuestions[parent.ID]; questionID != "" {
			base.Reason, base.QuestionID = "open blocking question", questionID
		} else {
			for _, dependency := range featurePrerequisites(st, parent.ID) {
				if target := feature(st, dependency.ToFeatureID); target != nil && visible(target.ProductID) {
					base.Reason, base.DependencyID, base.WaitingOn = "required feature not completed", dependency.ID, target.ID
					break
				}
			}
		}
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
			base.Kind = "plan_feature"
			appendItem(base, "ready feature needs integrations")
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
			if questionID := integrationQuestions[child.ID]; questionID != "" {
				item.Reason, item.QuestionID, item.DependencyID, item.WaitingOn = "open blocking question", questionID, "", ""
			}
			if item.Reason == "" {
				for _, dependency := range child.Dependencies {
					if statuses[dependency] != "ready" && statuses[dependency] != "released" {
						item.Reason, item.DependencyID = "integration dependency not ready", dependency
						break
					}
				}
			}
			execution := executions[child.ID]
			switch execution.State {
			case "", "unassigned", "lost", "exited_incomplete":
			case "blocked":
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

func raiseWorkEvent(st *domain.State, registration *domain.UserOrchestrator, productID, reason string, now time.Time) {
	registration.Generation++
	wake := domain.ExecutionWakeTarget{Kind: registration.Harness.Kind, SessionRef: registration.Harness.SessionRef, TmuxTarget: registration.Harness.TmuxTarget, Source: "user_orchestrator"}
	event := domain.ExecutionEvent{ID: id(), ProductID: productID, Owner: registration.UserID, RegistrationID: registration.ID, Type: "work_available", ConditionGeneration: registration.Generation, Reason: reason, DedupeKey: fmt.Sprintf("%s|work_available|%d", registration.ID, registration.Generation), WakeTarget: wake, HarnessKind: wake.Kind, SessionRef: wake.SessionRef, TmuxTarget: wake.TmuxTarget, CreatedAt: now, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: now}}
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
		if available == registration.Available {
			continue
		}
		registration.Available, registration.UpdatedAt, changed = available, now, true
		if available {
			raiseWorkEvent(st, registration, items[0].ProductID, "work queue became non-empty", now)
		}
	}
	return changed
}

func wakeQuestionOwner(st *domain.State, question domain.Memory, now time.Time) {
	for _, scope := range []string{question.ProductID, ""} {
		for index := range st.UserOrchestrators {
			registration := &st.UserOrchestrators[index]
			if registration.UnregisteredAt == nil && registration.UserID == question.Question.AddressedTo && registration.ProductID == scope && registrationVisible(st, *registration, question.ProductID) {
				for _, event := range st.ExecutionEvents {
					if event.RegistrationID == registration.ID && event.ProductID == question.ProductID && event.CreatedAt.Equal(now) && event.Resolution.ResolvedAt == nil {
						return
					}
				}
				raiseWorkEvent(st, registration, question.ProductID, "owner question answered", now)
				return
			}
		}
	}
}
