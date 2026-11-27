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

func guardFeatureStart(ctx context.Context, st *domain.State, child *domain.Integration) error {
	if parent := feature(st, child.FeatureID); parent != nil && parent.Status == "idea" {
		return invalid("FEATURE_NOT_READY")
	}
	for _, memory := range st.Memories {
		if memory.FeatureID == child.FeatureID && (memory.IntegrationID == "" || memory.IntegrationID == child.ID) && memory.Status == "open" && memory.Question != nil && memory.Question.Blocking {
			return invalid("OPEN_BLOCKING_QUESTION: %s", memory.ID)
		}
	}
	for _, targetID := range featureWaitingOn(st, child.FeatureID) {
		if target := feature(st, targetID); target != nil {
			if _, err := workProduct(ctx, st, target.ProductID, "read"); err == nil {
				return invalid("FEATURE_DEPENDENCY_NOT_COMPLETED: %s", targetID)
			}
		}
	}
	return nil
}

func canApproveWork(ctx context.Context, st *domain.State, product *domain.Product, parentID, approvalID, purpose string) error {
	if humanWorkCaller(ctx) {
		return nil
	}
	principal, _ := auth.FromContext(ctx)
	if principal.Kind != "agent" || workUser(ctx) == "" {
		return auth.ErrForbidden
	}
	if approvalID != "" {
		question := questionByID(st, approvalID)
		if question == nil {
			return missing("question", approvalID)
		}
		if _, err := workProduct(ctx, st, question.ProductID, "read"); err != nil {
			return err
		}
		parent := feature(st, parentID)
		matchesPurpose := question.Question.DecisionFor == purpose
		if edgeID, dependency := strings.CutPrefix(purpose, "dependency:"); dependency && question.Question.DecisionFor == "dependency_approval" {
			edge := dependencyByID(st, edgeID)
			matchesPurpose = edge != nil && edge.ApprovalQuestionID == question.ID
		}
		if parent != nil && question.ProductID == product.ID && question.FeatureID == parentID && question.Status == "answered" && question.Question.Kind == "decision" && question.Question.Answer != nil && question.Question.Answer.OptionID == "approve" && (matchesPurpose || purpose == "idea_promotion" && question.Question.DecisionFor == "" && question.Question.AddressedTo == featureDecisionOwner(*parent, *product)) {
			return nil
		}
	}
	if product.IdeaPromotion == "agent_allowed" {
		return nil
	}
	return fmt.Errorf("%w: OWNER_APPROVAL_REQUIRED", auth.ErrForbidden)
}

func featureDecisionOwner(parent domain.Feature, product domain.Product) string {
	addressee := product.Owner
	if humanWorkID(parent.Owner) && (product.Visibility != "private" || parent.Owner == product.Owner || slices.Contains(product.Members, parent.Owner)) {
		addressee = parent.Owner
	}
	return addressee
}

func decisionQuestion(ctx context.Context, st *domain.State, c domain.Command, parent domain.Feature, product domain.Product, purpose, title, rationale string, now time.Time) (domain.Memory, error) {
	addressee := featureDecisionOwner(parent, product)
	if addressee == "" && humanWorkCaller(ctx) {
		addressee = workUser(ctx)
	}
	if !humanWorkID(addressee) {
		return domain.Memory{}, invalid("decision requires a human owner")
	}
	if strings.TrimSpace(rationale) == "" || len(rationale) > 16384 {
		return domain.Memory{}, invalid("decision rationale required (up to 16384 bytes)")
	}
	for _, memory := range st.Memories {
		if purpose != "dependency_approval" && memory.FeatureID == parent.ID && memory.Status == "open" && memory.Question != nil && memory.Question.DecisionFor == purpose {
			return domain.Memory{}, invalid("a decision question is already open: %s", memory.ID)
		}
	}
	question := domain.Memory{Meta: workMeta(c, product.ID, parent.ID, now), Kind: "question", Title: title, Body: rationale, Status: "open", Question: &domain.QuestionDetails{
		AddressedTo: addressee, AskedBy: c.Actor, AskedByUser: workUser(ctx), Kind: "decision", DecisionFor: purpose, Recommendation: "approve", ContextRefs: []string{parent.ID},
		Options: []domain.QuestionOption{{ID: "approve", Label: "Approve", Consequence: "Authorize the proposed change"}, {ID: "reject", Label: "Reject", Consequence: "Leave the proposal unapplied"}},
	}}
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
	if from == nil || to == nil {
		return nil, nil, missing("feature dependency", edge.ID)
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
		if !humanWorkCaller(ctx) {
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
			if target := feature(st, edge.ToFeatureID); target != nil && target.Status != "completed" {
				waiting = append(waiting, edge)
			}
		}
	}
	slices.SortFunc(waiting, func(left, right domain.FeatureDependency) int {
		return strings.Compare(left.ToFeatureID, right.ToFeatureID)
	})
	return waiting
}

func featureWaitingOn(st *domain.State, parentID string) []string {
	waiting := []string{}
	for _, dependency := range featurePrerequisites(st, parentID) {
		waiting = append(waiting, dependency.ToFeatureID)
	}
	return waiting
}
