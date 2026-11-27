package application

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func featureRestoreStatus(st *domain.State, parent *domain.Feature) string {
	if parent.ArchivedFrom != "" {
		return parent.ArchivedFrom
	}
	foundArchive := false
	reopened := false
	for index := len(st.Events) - 1; index >= 0; index-- {
		event := st.Events[index]
		if event.ProductID != parent.ProductID || event.FeatureID != parent.ID && !(event.Action == "approve_idea_promotion" && event.EntityID == parent.ID) {
			continue
		}
		status, _ := event.Data.Data["status"].(string)
		if event.EntityID != parent.ID {
			if foundArchive {
				switch event.Action {
				case "create_integration", "update_integration", "start_integration":
					reopened = true
				case "transition_integration":
					reopened = reopened || slices.Contains([]string{"planned", "working", "implemented", "verifying"}, status)
				}
			}
			continue
		}
		switch event.Action {
		case "create_feature":
			if status == "" {
				status = "active"
			}
		case "approve_idea_promotion":
			status = "ready_for_execution"
		case "update_feature":
		default:
			continue
		}
		if !domain.ValidStatus("feature", status) {
			continue
		}
		if !foundArchive {
			if status != "archived" {
				break
			}
			foundArchive = true
		} else if status == "archived" {
			reopened = false
		} else {
			if status == "completed" && reopened {
				return "active"
			}
			return status
		}
	}
	return ""
}

func featureCompletionReady(st *domain.State, featureID string) error {
	found := false
	for _, integration := range st.Integrations {
		if integration.FeatureID != featureID {
			continue
		}
		found = true
		switch integration.Status {
		case "ready":
			if err := readiness(st, integration); err != nil {
				return err
			}
		case "released":
		default:
			return invalid("feature completion requires all integrations ready or released")
		}
	}
	if !found {
		return invalid("feature completion requires integrations")
	}
	return nil
}

func featureStartBlocker(st *domain.State, parentID, integrationID string, visible func(string) bool) workItem {
	for _, memory := range st.Memories {
		if memory.FeatureID == parentID && (memory.IntegrationID == "" || memory.IntegrationID == integrationID) && questionBlocked(memory) {
			return workItem{Reason: "open blocking question", QuestionID: memory.ID}
		}
	}
	waiting := featurePrerequisites(st, parentID)
	for _, dependency := range waiting {
		if target := feature(st, dependency.ToFeatureID); target != nil && visible(target.ProductID) {
			return workItem{Reason: "required feature not completed", DependencyID: dependency.ID, WaitingOn: target.ID}
		}
	}
	if len(waiting) != 0 {
		return workItem{Reason: "required feature not completed"}
	}
	return workItem{}
}

func integrationWaitingOn(st *domain.State, child *domain.Integration) string {
	for _, dependencyID := range child.Dependencies {
		dependency := integration(st, dependencyID)
		if dependency == nil || dependency.Status != "ready" && dependency.Status != "released" {
			return dependencyID
		}
	}
	return ""
}

func integrationStartBlocker(st *domain.State, child *domain.Integration, visible func(string) bool) workItem {
	if child == nil {
		return workItem{Reason: "feature not ready"}
	}
	if parent := feature(st, child.FeatureID); parent == nil || parent.Status == "idea" || parent.Status == "archived" {
		return workItem{Reason: "feature not ready"}
	}
	if blocker := featureStartBlocker(st, child.FeatureID, child.ID, visible); blocker.Reason != "" {
		return blocker
	}
	if dependency := integrationWaitingOn(st, child); dependency != "" {
		return workItem{Reason: "integration dependency not ready", DependencyID: dependency}
	}
	return workItem{}
}

func guardFeatureStart(ctx context.Context, st *domain.State, child *domain.Integration) error {
	blocker := integrationStartBlocker(st, child, func(productID string) bool { return workProductReadable(ctx, st, productID) })
	switch blocker.Reason {
	case "feature not ready":
		return invalid("FEATURE_NOT_READY")
	case "open blocking question":
		return invalid("OPEN_BLOCKING_QUESTION: %s", blocker.QuestionID)
	case "required feature not completed":
		if blocker.WaitingOn != "" {
			return invalid("FEATURE_DEPENDENCY_NOT_COMPLETED: %s", blocker.WaitingOn)
		}
		return invalid("FEATURE_DEPENDENCY_NOT_COMPLETED")
	case "integration dependency not ready":
		return invalid("INTEGRATION_DEPENDENCY_NOT_READY: %s", blocker.DependencyID)
	}
	return nil
}

func canApproveWork(ctx context.Context, st *domain.State, product *domain.Product, parent *domain.Feature, edge *domain.FeatureDependency, approvalID, action string) error {
	if isWorkDecisionOwner(ctx, product) {
		return nil
	}
	principal, _ := auth.FromContext(ctx)
	if principal.Kind == "agent" && workUser(ctx) != "" && approvalID != "" {
		question := questionByID(st, approvalID)
		if question == nil || !workProductReadable(ctx, st, question.ProductID) {
			return ErrNotFound
		}
		details := question.Question
		if product != nil && question.ProductID == product.ID && question.FeatureID == parent.ID && question.Status == "answered" && details.Kind == "decision" && details.Answer != nil && details.Answer.OptionID == "approve" && details.Answer.AnsweredBy == product.Owner && details.DecisionAction == action && decisionMatches(question, parent, edge) {
			return nil
		}
	}
	if principal.Kind == "agent" && workUser(ctx) != "" && approvalID == "" && product != nil && product.IdeaPromotion == "agent_allowed" {
		return nil
	}
	return fmt.Errorf("%w: OWNER_APPROVAL_REQUIRED", auth.ErrForbidden)
}

func isWorkDecisionOwner(ctx context.Context, product *domain.Product) bool {
	_, authenticated := auth.FromContext(ctx)
	return product != nil && humanWorkCaller(ctx) && (product.Owner == workUser(ctx) || !authenticated && product.Owner == "")
}

func decisionMatches(question *domain.Memory, parent *domain.Feature, edge *domain.FeatureDependency) bool {
	details := question.Question
	if edge != nil {
		return details.DecisionFor == "dependency_approval" && edge.Status == "proposed" && edge.ApprovalQuestionID == question.ID
	}
	return details.DecisionFor == "idea_promotion" && parent != nil && parent.Status == "idea" && details.DecisionRevision != nil && details.DecisionRevision.Equal(parent.UpdatedAt)
}

func decisionQuestion(ctx context.Context, st *domain.State, c domain.Command, parent domain.Feature, product domain.Product, purpose, action, title, rationale string, now time.Time) (domain.Memory, error) {
	addressee := product.Owner
	if addressee == "" && isWorkDecisionOwner(ctx, &product) {
		addressee = workUser(ctx)
	}
	if !humanWorkID(addressee) {
		return domain.Memory{}, invalid("decision requires a human owner")
	}
	if strings.TrimSpace(rationale) == "" || len(rationale) > 16384 {
		return domain.Memory{}, invalid("decision rationale required (up to 16384 bytes)")
	}
	for _, memory := range st.Memories {
		if purpose == "idea_promotion" && memory.FeatureID == parent.ID && memory.Status == "open" && memory.Question != nil && decisionMatches(&memory, &parent, nil) {
			return domain.Memory{}, invalid("a decision question is already open: %s", memory.ID)
		}
	}
	question := domain.Memory{Meta: workMeta(c, product.ID, parent.ID, now), Kind: "question", Title: title, Body: rationale, Status: "open", Question: &domain.QuestionDetails{
		AddressedTo: addressee, AskedBy: c.Actor, AskedByUser: workUser(ctx), Kind: "decision", DecisionFor: purpose, DecisionAction: action, Recommendation: "approve", ContextRefs: []string{parent.ID},
		Options: []domain.QuestionOption{{ID: "approve", Label: "Approve", Consequence: "Authorize the proposed change"}, {ID: "reject", Label: "Reject", Consequence: "Leave the proposal unapplied"}},
	}}
	if purpose == "idea_promotion" {
		question.Question.DecisionRevision = &parent.UpdatedAt
	}
	return question, storeQuestion(st, question, now)
}

func dependencyByID(st *domain.State, dependencyID string) *domain.FeatureDependency {
	for index := range st.FeatureDependencies {
		if st.FeatureDependencies[index].ID == dependencyID {
			return &st.FeatureDependencies[index]
		}
	}
	return nil
}

func dependencyProducts(ctx context.Context, st *domain.State, edge *domain.FeatureDependency) (*domain.Feature, *domain.Feature, error) {
	from, to := feature(st, edge.FromFeatureID), feature(st, edge.ToFeatureID)
	for _, endpoint := range []struct {
		id      string
		feature *domain.Feature
	}{{edge.FromFeatureID, from}, {edge.ToFeatureID, to}} {
		if endpoint.feature == nil || !workProductReadable(ctx, st, endpoint.feature.ProductID) {
			if edge.ID == "" {
				return nil, nil, missing("feature", endpoint.id)
			}
			return nil, nil, missing("feature dependency", edge.ID)
		}
	}
	for _, parent := range []*domain.Feature{from, to} {
		if _, err := workProduct(ctx, st, parent.ProductID, "write"); err != nil {
			return nil, nil, err
		}
	}
	return from, to, nil
}

func dependencyPath(st *domain.State, start, goal string) []string {
	adjacent := map[string][]string{}
	for _, edge := range st.FeatureDependencies {
		if edge.Type == "requires" && edge.Status == "accepted" {
			adjacent[edge.FromFeatureID] = append(adjacent[edge.FromFeatureID], edge.ToFeatureID)
		}
	}
	queue := []string{start}
	previous := map[string]string{start: ""}
	for index := 0; index < len(queue); index++ {
		current := queue[index]
		if current == goal {
			path := []string{}
			for current != "" {
				path = append(path, current)
				current = previous[current]
			}
			slices.Reverse(path)
			return path
		}
		for _, neighbor := range adjacent[current] {
			if _, visited := previous[neighbor]; !visited {
				previous[neighbor] = current
				queue = append(queue, neighbor)
			}
		}
	}
	return nil
}

func decideFeatureDependency(ctx context.Context, st *domain.State, edge *domain.FeatureDependency, decision, actor string, now time.Time) error {
	if edge.Status != "proposed" || decision != "accept" && decision != "reject" {
		return invalid("decision requires a proposed dependency and accept or reject")
	}
	if decision == "accept" && edge.Type == "requires" {
		if path := dependencyPath(st, edge.ToFeatureID, edge.FromFeatureID); len(path) != 0 {
			for _, featureID := range path {
				parent := feature(st, featureID)
				if parent == nil {
					return invalid("DEPENDENCY_CYCLE")
				}
				if _, err := workProduct(ctx, st, parent.ProductID, "read"); err != nil {
					return invalid("DEPENDENCY_CYCLE: path unavailable")
				}
			}
			return invalid("DEPENDENCY_CYCLE: %s", strings.Join(append([]string{edge.FromFeatureID}, path...), " -> "))
		}
	}
	edge.Status = decision + "ed"
	edge.DecidedBy, edge.DecidedAt, edge.UpdatedAt = actor, &now, now
	return nil
}

func proposeDependency(ctx context.Context, st *domain.State, c domain.Command, now time.Time) (domain.FeatureDependency, error) {
	var input struct {
		From      string `json:"from_feature_id"`
		To        string `json:"to_feature_id"`
		Type      string `json:"type"`
		Rationale string `json:"rationale"`
		Accept    bool   `json:"accept"`
	}
	if err := decode(c.Data, &input); err != nil {
		return domain.FeatureDependency{}, err
	}
	edge := domain.FeatureDependency{FromFeatureID: input.From, ToFeatureID: input.To, Type: input.Type, Status: "proposed", Rationale: input.Rationale, ProposedBy: c.Actor}
	from, to, err := dependencyProducts(ctx, st, &edge)
	if err != nil {
		return domain.FeatureDependency{}, err
	}
	if c.ProductID != "" && c.ProductID != from.ProductID || from.ID == to.ID || input.Type != "requires" && input.Type != "relates" || strings.TrimSpace(input.Rationale) == "" || len(input.Rationale) > 16384 {
		return domain.FeatureDependency{}, invalid("distinct feature endpoints, valid dependency type and rationale required")
	}
	for _, existing := range st.FeatureDependencies {
		if existing.Type == edge.Type && (existing.FromFeatureID == edge.FromFeatureID && existing.ToFeatureID == edge.ToFeatureID || edge.Type == "relates" && existing.FromFeatureID == edge.ToFeatureID && existing.ToFeatureID == edge.FromFeatureID) {
			return domain.FeatureDependency{}, invalid("duplicate feature dependency")
		}
	}
	edge.Meta = workMeta(c, from.ProductID, from.ID, now)
	if input.Accept {
		if !isWorkDecisionOwner(ctx, productByID(*st, from.ProductID)) {
			return domain.FeatureDependency{}, auth.ErrForbidden
		}
		if err := decideFeatureDependency(ctx, st, &edge, "accept", c.Actor, now); err != nil {
			return domain.FeatureDependency{}, err
		}
	}
	st.FeatureDependencies = append(st.FeatureDependencies, edge)
	workAudit(st, edge.Meta, c.Action, c.Actor, now)
	return edge, nil
}

func featurePrerequisites(st *domain.State, parentID string) []domain.FeatureDependency {
	waiting := []domain.FeatureDependency{}
	for _, edge := range st.FeatureDependencies {
		if edge.FromFeatureID == parentID && edge.Type == "requires" && edge.Status == "accepted" {
			if target := feature(st, edge.ToFeatureID); target == nil || target.Status != "completed" {
				waiting = append(waiting, edge)
			}
		}
	}
	slices.SortFunc(waiting, func(left, right domain.FeatureDependency) int {
		return strings.Compare(left.ToFeatureID, right.ToFeatureID)
	})
	return waiting
}

func featureWaitingOn(st *domain.State, parentID string, visible func(string) bool) ([]string, bool) {
	waiting := []string{}
	unblocked := true
	for _, dependency := range featurePrerequisites(st, parentID) {
		unblocked = false
		if target := feature(st, dependency.ToFeatureID); target != nil && visible(target.ProductID) {
			waiting = append(waiting, dependency.ToFeatureID)
		}
	}
	return waiting, unblocked
}
