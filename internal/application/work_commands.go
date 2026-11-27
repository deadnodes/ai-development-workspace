package application

import (
	"context"
	"slices"
	"strings"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func (s *Service) executeWorkCommand(ctx context.Context, command domain.Command) (any, error) {
	if strings.TrimSpace(command.Actor) == "" {
		command.Actor = "agent"
	}
	if command.ID != "" || command.GateID != "" || command.CheckID != "" || command.ResultID != "" || command.Action != "ask_question" && command.IntegrationID != "" || command.Action != "ask_question" && command.Action != "request_idea_promotion" && command.FeatureID != "" {
		return nil, invalid("unsupported work command scope")
	}
	var output any
	err := s.store.Update(ctx, func(st *domain.State) error {
		now := nowUTC()
		var err error
		if command.ProductID != "" && (command.Action == "register_user_orchestrator" || command.Action == "unregister_user_orchestrator") {
			if _, err := workProduct(ctx, st, command.ProductID, "write"); err != nil {
				return err
			}
		}
		switch command.Action {
		case "ask_question":
			output, err = askQuestion(ctx, st, command, now)
		case "answer_questions":
			output, err = answerQuestions(ctx, st, command, now)
		case "withdraw_question":
			output, err = withdrawQuestion(ctx, st, command, now)
		case "set_idea_promotion":
			product, err := workProduct(ctx, st, command.ProductID, "write")
			if err != nil {
				return err
			}
			if !isWorkDecisionOwner(ctx, product) {
				return auth.ErrForbidden
			}
			var input struct {
				Policy string `json:"idea_promotion"`
			}
			if err := decode(command.Data, &input); err != nil {
				return err
			}
			if input.Policy != "ask_owner" && input.Policy != "agent_allowed" {
				return invalid("idea_promotion must be ask_owner or agent_allowed")
			}
			product.IdeaPromotion, product.Actor, product.UpdatedAt = input.Policy, command.Actor, now
			workAudit(st, product.Meta, command.Action, command.Actor, now)
			output = *product
		case "request_idea_promotion":
			parent, _, product, err := questionScope(ctx, st, command)
			if err != nil {
				return err
			}
			if parent.Status != "idea" {
				return invalid("feature is not an idea")
			}
			var input struct {
				Rationale string `json:"rationale"`
			}
			if err := decode(command.Data, &input); err != nil {
				return err
			}
			supersedeIdeaDecisions(st, parent, command.Actor, now)
			output, err = decisionQuestion(ctx, st, command, *parent, *product, "idea_promotion", "ready_for_execution", "Promote idea: "+parent.Title, input.Rationale, now)
			return err
		case "propose_dependency":
			output, err = proposeDependency(ctx, st, command, now)
		case "decide_dependency", "request_dependency_decision":
			var input struct {
				ID       string `json:"dependency_id"`
				Decision string `json:"decision"`
				Approval string `json:"approval_question_id"`
			}
			if err := decode(command.Data, &input); err != nil {
				return err
			}
			edge := dependencyByID(st, input.ID)
			if edge == nil || command.ProductID != "" && command.ProductID != edge.ProductID {
				return missing("feature dependency", input.ID)
			}
			from, to, err := dependencyProducts(ctx, st, edge)
			if err != nil {
				return err
			}
			product := productByID(*st, from.ProductID)
			if command.Action == "request_dependency_decision" {
				if input.Approval != "" || edge.Status != "proposed" {
					return invalid("request a decision only for a proposed dependency")
				}
				if input.Decision == "" {
					input.Decision = "accept"
				}
				if input.Decision != "accept" && input.Decision != "reject" {
					return invalid("decision must be accept or reject")
				}
				if prior := questionByID(st, edge.ApprovalQuestionID); prior != nil && prior.Status == "open" {
					return invalid("a decision question is already open: %s", prior.ID)
				}
				title, rationale := "Approve request to "+input.Decision+" dependency on "+to.Title, edge.Rationale
				if to.ProductID != from.ProductID {
					targetProduct := productByID(*st, to.ProductID)
					owner := product.Owner
					if targetProduct.Visibility == "private" && owner != targetProduct.Owner && !slices.Contains(targetProduct.Members, owner) {
						return invalid("feature decision owner must be able to review both endpoints")
					}
					title, rationale = "Review request to "+input.Decision+" a dependency", "Review this proposal in the dependency graph before deciding. The graph shows the linked question only to readers who can see both endpoints. Do not include restricted endpoint details in this answer."
				}
				question, err := decisionQuestion(ctx, st, command, *from, *product, "dependency_approval", input.Decision, title, rationale, now)
				if err == nil {
					edge.ApprovalQuestionID, edge.UpdatedAt = question.ID, now
				}
				output = question
				return err
			}
			if err := canApproveWork(ctx, st, product, from, edge, input.Approval, input.Decision); err != nil {
				return err
			}
			if err := decideFeatureDependency(ctx, st, edge, input.Decision, command.Actor, now); err != nil {
				return err
			}
			workAudit(st, edge.Meta, command.Action, command.Actor, now)
			output = *edge
		case "register_user_orchestrator", "unregister_user_orchestrator":
			output, err = changeUserOrchestrator(ctx, st, command, now)
		}
		if err == nil && command.Action != "register_user_orchestrator" && command.Action != "unregister_user_orchestrator" && len(st.UserOrchestrators) != 0 {
			reconcileUserWork(st, now)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}
