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

func workUser(ctx context.Context) string {
	if principal, ok := auth.FromContext(ctx); ok {
		switch principal.Kind {
		case "human":
			return principal.ID
		case "agent":
			return principal.Owner
		}
		return ""
	}
	return "local"
}

func humanWorkCaller(ctx context.Context) bool {
	principal, authenticated := auth.FromContext(ctx)
	return !authenticated || principal.Kind == "human"
}

func workProduct(ctx context.Context, st *domain.State, productID, permission string) (*domain.Product, error) {
	product := productByID(*st, productID)
	if product == nil || !productVisibleTo(ctx, product) {
		return nil, missing("product", productID)
	}
	return product, authorize(ctx, permission, productID)
}

func workProductReadable(ctx context.Context, st *domain.State, productID string) bool {
	_, err := workProduct(ctx, st, productID, "read")
	return err == nil
}

func workMeta(c domain.Command, productID, featureID string, now time.Time) domain.Meta {
	return domain.Meta{ID: id(), ProductID: productID, FeatureID: featureID, Actor: c.Actor, CreatedAt: now, UpdatedAt: now}
}

func workAudit(st *domain.State, meta domain.Meta, action, actor string, now time.Time) {
	st.Events = append(st.Events, domain.Event{ID: id(), Action: action, Actor: actor, At: now, EntityID: meta.ID, ProductID: meta.ProductID, FeatureID: meta.FeatureID, Data: domain.Command{Action: action, Actor: actor}})
}

func questionByID(st *domain.State, questionID string) *domain.Memory {
	for index := range st.Memories {
		memory := &st.Memories[index]
		if memory.ID == questionID && memory.Kind == "question" && memory.Question != nil {
			return memory
		}
	}
	return nil
}

func questionBlocked(memory domain.Memory) bool {
	return memory.Kind == "question" && memory.Status == "open" && memory.Question != nil && memory.Question.Blocking
}

const supersededIdeaReason = "Superseded by a change to the idea revision"

func supersedeIdeaDecisions(st *domain.State, parent *domain.Feature, actor string, now time.Time) {
	for index := range st.Memories {
		question := &st.Memories[index]
		if question.FeatureID != parent.ID || question.ProductID != parent.ProductID || question.Status != "open" || question.Question == nil || question.Question.DecisionFor != "idea_promotion" || decisionMatches(question, parent, nil) {
			continue
		}
		details := *question.Question
		details.WithdrawnReason = supersededIdeaReason
		question.Question, question.Status, question.UpdatedAt = &details, "withdrawn", now
		workAudit(st, question.Meta, "supersede_idea_promotion", actor, now)
	}
}

func questionScope(ctx context.Context, st *domain.State, command domain.Command) (*domain.Feature, *domain.Integration, *domain.Product, error) {
	parent := feature(st, command.FeatureID)
	if parent == nil || !workProductReadable(ctx, st, parent.ProductID) {
		return nil, nil, nil, missing("feature", command.FeatureID)
	}
	product, err := workProduct(ctx, st, parent.ProductID, "write")
	if err != nil {
		return nil, nil, nil, err
	}
	if command.ProductID != "" && command.ProductID != product.ID {
		return nil, nil, nil, invalid("SCOPE_MISMATCH")
	}
	var child *domain.Integration
	if command.IntegrationID != "" {
		child = integration(st, command.IntegrationID)
		if child == nil || child.FeatureID != parent.ID || child.ProductID != product.ID {
			return nil, nil, nil, missing("integration", command.IntegrationID)
		}
	}
	return parent, child, product, nil
}

type questionReference struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

func resolveQuestionReference(st *domain.State, productID, ref string) (questionReference, bool) {
	switch {
	case ref == "":
		return questionReference{}, false
	}
	if item := feature(st, ref); item != nil && item.ProductID == productID {
		return questionReference{ref, "feature", contextExcerpt(item.Title, 120)}, true
	}
	if item := integration(st, ref); item != nil && item.ProductID == productID {
		return questionReference{ref, "integration", contextExcerpt(item.Title, 120)}, true
	}
	for _, item := range st.Memories {
		if item.ID == ref && item.ProductID == productID {
			return questionReference{ref, "memory", contextExcerpt(item.Title, 120)}, true
		}
	}
	for _, item := range st.Findings {
		if item.ID == ref && item.ProductID == productID {
			return questionReference{ref, "finding", contextExcerpt(item.Title, 120)}, true
		}
	}
	return questionReference{}, false
}

func askQuestion(ctx context.Context, st *domain.State, c domain.Command, now time.Time) (domain.Memory, error) {
	parent, child, product, err := questionScope(ctx, st, c)
	if err != nil {
		return domain.Memory{}, err
	}
	var input struct {
		Title          string                  `json:"title"`
		Body           string                  `json:"body"`
		Kind           string                  `json:"kind"`
		Options        []domain.QuestionOption `json:"options"`
		Recommendation string                  `json:"recommendation"`
		Script         string                  `json:"script"`
		Why            string                  `json:"why"`
		Blocks         string                  `json:"blocks"`
		Verify         string                  `json:"verify"`
		URL            string                  `json:"url"`
		LookFor        string                  `json:"look_for"`
		ContextRefs    []string                `json:"context_refs"`
		Blocking       bool                    `json:"blocking"`
	}
	if err := decode(c.Data, &input); err != nil {
		return domain.Memory{}, err
	}
	validKind := slices.Contains([]string{"question", "conflict", "decision", "owner_action"}, input.Kind)
	if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Body) == "" || !validKind || input.Kind != "question" && input.Kind != "owner_action" && strings.TrimSpace(input.Recommendation) == "" {
		return domain.Memory{}, invalid("question title, body, kind and (for conflicts or decisions) recommendation required")
	}
	if input.Kind == "owner_action" {
		hasScript := strings.TrimSpace(input.Script) != ""
		hasURL := strings.TrimSpace(input.URL) != ""
		if strings.TrimSpace(input.Why) == "" || strings.TrimSpace(input.Verify) == "" || hasScript == hasURL || hasURL && strings.TrimSpace(input.LookFor) == "" {
			return domain.Memory{}, invalid("OWNER_ACTION_REQUIRES_SCRIPT")
		}
		if len(input.Script) > 16*1024 || len(input.Why) > 2*1024 || len(input.Blocks) > 2*1024 || len(input.Verify) > 2*1024 || len(input.URL) > 2*1024 || len(input.LookFor) > 2*1024 {
			return domain.Memory{}, invalid("question exceeds size limits")
		}
	}
	if len(input.Options) > 50 || len(input.ContextRefs) > 50 || len(input.Title) > 512 || len(input.Body) > 16384 || len(input.Recommendation) > 4096 {
		return domain.Memory{}, invalid("question exceeds size limits")
	}
	seen := map[string]bool{}
	for _, option := range input.Options {
		if strings.TrimSpace(option.ID) == "" || strings.TrimSpace(option.Label) == "" || seen[option.ID] {
			return domain.Memory{}, invalid("unique option id and label required")
		}
		seen[option.ID] = true
	}
	for _, ref := range input.ContextRefs {
		if _, ok := resolveQuestionReference(st, product.ID, ref); !ok {
			return domain.Memory{}, missing("context reference", ref)
		}
	}
	addressee := product.Owner
	principal, authenticated := auth.FromContext(ctx)
	if authenticated && principal.Kind == "agent" && principal.Owner != "" {
		addressee = principal.Owner
	}
	if child != nil && child.Assignee != "" {
		addressee = child.Assignee
	}
	if addressee == "" && !authenticated {
		addressee = workUser(ctx)
	}
	var execution *domain.IntegrationExecution
	if input.Blocking {
		if child == nil {
			if !isWorkDecisionOwner(ctx, product) {
				return domain.Memory{}, auth.ErrForbidden
			}
		} else {
			if err := requireIntegrationUser(ctx, child, false); err != nil {
				return domain.Memory{}, err
			}
			if authenticated && (child.Assignee == "" || child.Assignee != workUser(ctx)) {
				return domain.Memory{}, notAssignee(child.Assignee)
			}
			execution = executionFor(st, child.ID)
			if execution != nil && execution.LeaseUntil != nil && now.Before(*execution.LeaseUntil) && (execution.State == "running" || execution.State == "stalled") && leasePrincipal(execution) != c.Actor {
				return domain.Memory{}, fmt.Errorf("%w: ACTIVE_EXECUTION", ErrConflict)
			}
		}
	}
	question := domain.Memory{Meta: workMeta(c, product.ID, parent.ID, now), IntegrationID: c.IntegrationID, Kind: "question", Title: input.Title, Body: input.Body, Status: "open", Question: &domain.QuestionDetails{
		AddressedTo: addressee, AskedBy: c.Actor, AskedByUser: workUser(ctx), Kind: input.Kind, Options: input.Options, Recommendation: input.Recommendation, Script: input.Script, Why: input.Why, Blocks: input.Blocks, Verify: input.Verify, URL: input.URL, LookFor: input.LookFor, ContextRefs: input.ContextRefs, Blocking: input.Blocking,
	}}
	if err := storeQuestion(st, question, now); err != nil {
		return domain.Memory{}, err
	}
	if execution != nil && execution.LeaseUntil != nil && now.Before(*execution.LeaseUntil) && (execution.State == "running" || execution.State == "stalled") {
		executionCondition(st, execution, "blocked", false, "owner question", now)
		execution.WaitingRef, execution.BlockerRef = "", question.ID
		fence := execution.Fence
		execution.BlockerFence = &fence
	}
	return question, nil
}

func storeQuestion(st *domain.State, question domain.Memory, now time.Time) error {
	if !humanWorkID(question.Question.AddressedTo) {
		return invalid("question requires a human addressee")
	}
	st.Memories = append(st.Memories, question)
	workAudit(st, question.Meta, "ask_question", question.Actor, now)
	return nil
}

func humanWorkID(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.HasPrefix(value, "agent/") && !strings.HasPrefix(value, "service/") && !strings.HasPrefix(value, "worker/")
}

type answerInput struct {
	QuestionID string                  `json:"question_id"`
	Text       string                  `json:"text"`
	OptionID   string                  `json:"option_id"`
	Validation domain.AnswerValidation `json:"validation"`
	Override   bool                    `json:"override"`
}

func editableQuestion(ctx context.Context, st *domain.State, c domain.Command, questionID string) (*domain.Memory, error) {
	question := questionByID(st, questionID)
	if question == nil || c.ProductID != "" && question.ProductID != c.ProductID || !workProductReadable(ctx, st, question.ProductID) {
		return nil, missing("question", questionID)
	}
	if _, err := workProduct(ctx, st, question.ProductID, "write"); err != nil {
		return nil, err
	}
	return question, nil
}

func answerQuestions(ctx context.Context, st *domain.State, c domain.Command, now time.Time) ([]domain.Memory, error) {
	var input struct {
		Answers []answerInput `json:"answers"`
	}
	if err := decode(c.Data, &input); err != nil {
		return nil, err
	}
	if len(input.Answers) == 0 || len(input.Answers) > 50 {
		return nil, invalid("answers must contain 1 to 50 entries")
	}
	answered := make([]domain.Memory, 0, len(input.Answers))
	for _, answer := range input.Answers {
		question, err := editableQuestion(ctx, st, c, answer.QuestionID)
		if err != nil {
			return nil, err
		}
		details := *question.Question
		if workUser(ctx) != details.AddressedTo {
			return nil, auth.ErrForbidden
		}
		if details.Kind == "owner_action" {
			if principal, ok := auth.FromContext(ctx); ok && principal.Kind == "agent" && (answer.Validation.Verdict != "consistent" || strings.TrimSpace(answer.Validation.Note) == "") {
				return nil, invalid("owner action agent closure requires a consistent verification result")
			}
		}
		if details.DecisionFor != "" && !isWorkDecisionOwner(ctx, productByID(*st, question.ProductID)) && !isDecisionRelay(ctx, productByID(*st, question.ProductID), &details) {
			return nil, auth.ErrForbidden
		}
		if question.Status != "open" {
			if details.DecisionFor == "idea_promotion" && details.WithdrawnReason == supersededIdeaReason {
				return nil, invalid("STALE_DECISION")
			}
			return nil, invalid("question %s is not open", question.ID)
		}
		optionIndex := slices.IndexFunc(details.Options, func(option domain.QuestionOption) bool { return option.ID == answer.OptionID })
		if strings.TrimSpace(answer.Text) == "" || answer.OptionID != "" && optionIndex < 0 || len(answer.Text) > 16384 || len(answer.Validation.Note) > 4096 {
			return nil, invalid("answer requires text and an option from the question if selected")
		}
		switch answer.Validation.Verdict {
		case "consistent":
		case "contradicts", "unclear":
			if !answer.Override {
				return nil, invalid("ANSWER_NEEDS_CLARIFICATION: question %s", question.ID)
			}
		default:
			return nil, invalid("invalid answer validation verdict")
		}
		if details.DecisionFor == "idea_promotion" {
			if answer.OptionID != "approve" && answer.OptionID != "reject" {
				return nil, invalid("choose approve or reject for idea promotion")
			}
			parent := feature(st, question.FeatureID)
			if details.DecisionAction != "ready_for_execution" || !decisionMatches(question, parent, nil) {
				return nil, invalid("STALE_DECISION")
			}
			if answer.OptionID == "approve" {
				parent.Status, parent.Actor, parent.UpdatedAt = "ready_for_execution", c.Actor, now
				parent.Revision++
				workAudit(st, parent.Meta, "approve_idea_promotion", c.Actor, now)
			}
		}
		if details.DecisionFor == "dependency_approval" {
			index := slices.IndexFunc(st.FeatureDependencies, func(edge domain.FeatureDependency) bool { return edge.ApprovalQuestionID == question.ID })
			if index < 0 || !decisionMatches(question, nil, &st.FeatureDependencies[index]) {
				return nil, invalid("STALE_DECISION")
			}
			if _, _, err := dependencyProducts(ctx, st, &st.FeatureDependencies[index]); err != nil {
				return nil, missing("question", question.ID)
			}
		}
		details.Answer = &domain.QuestionAnswer{Text: answer.Text, OptionID: answer.OptionID, AnsweredBy: c.Actor, AnsweredByUser: workUser(ctx), AnsweredAt: now, Validation: answer.Validation, Override: answer.Override}
		question.Question, question.Status, question.UpdatedAt = &details, "answered", now
		snapshot := *question
		optionText := ""
		if optionIndex >= 0 {
			optionText = fmt.Sprintf("\nSelected option: %s (%s)", details.Options[optionIndex].Label, answer.OptionID)
		}
		st.Memories = append(st.Memories, domain.Memory{Meta: workMeta(c, snapshot.ProductID, snapshot.FeatureID, now), IntegrationID: snapshot.IntegrationID, Kind: "decision", Title: snapshot.Title, Reason: "Answer to question " + snapshot.ID, Body: fmt.Sprintf("Question: %s\n\n%s\n\nAnswer: %s%s", snapshot.Title, snapshot.Body, answer.Text, optionText)})
		workAudit(st, snapshot.Meta, "answer_question", c.Actor, now)
		if details.DecisionFor == "idea_promotion" && answer.OptionID == "approve" {
			supersedeIdeaDecisions(st, feature(st, snapshot.FeatureID), c.Actor, now)
		}
		answered = append(answered, snapshot)
	}
	for _, question := range answered {
		if !wakeResolvedQuestion(st, question, now) {
			wakeQuestionOwner(st, question, now)
		}
	}
	return answered, nil
}

func withdrawQuestion(ctx context.Context, st *domain.State, c domain.Command, now time.Time) (domain.Memory, error) {
	var input struct {
		QuestionID string `json:"question_id"`
		Reason     string `json:"reason"`
	}
	if err := decode(c.Data, &input); err != nil {
		return domain.Memory{}, err
	}
	question, err := editableQuestion(ctx, st, c, input.QuestionID)
	if err != nil {
		return domain.Memory{}, err
	}
	user := workUser(ctx)
	details := *question.Question
	if c.Actor != details.AskedBy && (user == "" || user != details.AddressedTo && user != details.AskedByUser) {
		return domain.Memory{}, auth.ErrForbidden
	}
	if question.Status != "open" || strings.TrimSpace(input.Reason) == "" {
		return domain.Memory{}, invalid("withdrawal requires an open question and a reason")
	}
	details.WithdrawnReason = input.Reason
	question.Question, question.Status, question.UpdatedAt = &details, "withdrawn", now
	workAudit(st, question.Meta, "withdraw_question", c.Actor, now)
	wakeResolvedQuestion(st, *question, now)
	return *question, nil
}

// isDecisionRelay lets the Product owner answer a server-generated decision
// from chat: an agent owned by the Product owner may submit the owner's answer,
// but never the same agent principal that requested the decision, so an
// orchestrator cannot approve its own request.
func isDecisionRelay(ctx context.Context, product *domain.Product, details *domain.QuestionDetails) bool {
	principal, ok := auth.FromContext(ctx)
	return ok && product != nil && principal.Kind == "agent" && principal.Owner != "" && principal.Owner == product.Owner && principal.ID != details.AskedBy
}
