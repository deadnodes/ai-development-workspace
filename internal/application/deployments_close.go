package application

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"releasecontrol/internal/domain"
)

// CloseDeployment records operator abandonment before any GitOps mutation.
func (s *Service) CloseDeployment(ctx context.Context, in domain.CloseDeploymentRequest) (domain.Deployment, error) {
	var out domain.Deployment
	if err := authorize(ctx, "admin", ""); err != nil {
		return out, err
	}
	if in.ProductID == "" || in.DeploymentID == "" {
		return out, invalid("product_id and deployment_id required")
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if n := utf8.RuneCountInString(in.Reason); n < 8 || n > 500 {
		return out, invalid("reason must contain 8..500 characters")
	}
	if len(in.RequestKey) > 128 {
		return out, invalid("invalid request_key")
	}
	err := s.store.Update(ctx, func(st *domain.State) error {
		// The audit event is the durable request-key receipt, including after restart.
		if in.RequestKey != "" {
			for _, event := range st.Events {
				if event.Action != "close_deployment" || event.ProductID != in.ProductID || event.Data.Data["request_key"] != in.RequestKey {
					continue
				}
				if event.EntityID != in.DeploymentID || event.Data.Data["reason"] != in.Reason {
					return invalid("REQUEST_KEY_CONFLICT")
				}
				for _, d := range st.Deployments {
					if d.ID == in.DeploymentID && d.ProductID == in.ProductID {
						out = copyDeployment(d)
						return nil
					}
				}
				return missing("deployment", in.DeploymentID)
			}
		}
		for i, d := range st.Deployments {
			if d.ID != in.DeploymentID || d.ProductID != in.ProductID {
				continue
			}
			if deploymentTerminal(d) {
				return invalid("only active deployments may be closed")
			}
			// Only PENDING_CHECKS is safe: the dispatcher CASes PENDING_CHECKS->PROPOSING
			// before any GitHub write, while a PROPOSING deployment may have a proposal
			// in flight that would be orphaned by a close.
			safe := d.Status == "PENDING_CHECKS" && d.PRNumber == 0 && d.UncertainAction == "" && d.HeadSHA == "" && d.BaseSHA == "" && d.ProposalBaseSHA == ""
			if !safe {
				return invalid("deployment has GitOps side effects; close only before a PR exists")
			}
			next := d
			now := time.Now().UTC()
			next.Status, next.FailureCode, next.CloseReason = "FAILED", "OPERATOR_CLOSED", in.Reason
			next.UpdatedAt, next.FinishedAt = now, &now
			st.Deployments[i] = next
			audit := next
			audit.Actor = attributedActor(ctx, "agent")
			deploymentEvent(st, audit, "close_deployment")
			data := st.Events[len(st.Events)-1].Data.Data
			data["reason"], data["request_key"] = in.Reason, in.RequestKey
			data["previous_status"], data["previous_failure_code"], data["pr_number"] = d.Status, d.FailureCode, d.PRNumber
			out = copyDeployment(next)
			return nil
		}
		return missing("deployment", in.DeploymentID)
	})
	return out, err
}
