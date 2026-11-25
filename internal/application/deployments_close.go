package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/github"
)

func deploymentSafeToClose(d domain.Deployment) bool {
	return d.Status == "PENDING_CHECKS" && d.PRNumber == 0 && d.UncertainAction == "" && d.HeadSHA == "" && d.BaseSHA == "" && d.ProposalBaseSHA == "" || d.Status == "VERIFYING_ROLLOUT"
}

// CloseDeployment records operator abandonment where no GitOps writes remain in flight.
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
			// PENDING_CHECKS is safe: the dispatcher CASes PENDING_CHECKS->PROPOSING
			// before any GitHub write, while a PROPOSING deployment may have a proposal
			// in flight that would be orphaned by a close. VERIFYING_ROLLOUT is read-only.
			if !deploymentSafeToClose(d) {
				return invalid("deployment has GitOps side effects; close before a PR exists or while verifying rollout")
			}
			next := d
			now := time.Now().UTC()
			next.Status, next.FailureCode, next.CloseReason = "FAILED", "OPERATOR_CLOSED", in.Reason
			next.StatusChangedAt = now
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

// ReconcileDeployment performs one provider-backed resolution while holding the
// dispatcher's mutex so provider observations cannot race a merge attempt.
func (s *Service) ReconcileDeployment(ctx context.Context, in domain.ReconcileDeploymentRequest) (domain.ReconcileDeploymentResult, error) {
	var out domain.ReconcileDeploymentResult
	if err := authorize(ctx, "admin", ""); err != nil {
		return out, err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.ProductID == "" || in.DeploymentID == "" || utf8.RuneCountInString(in.Reason) < 8 || utf8.RuneCountInString(in.Reason) > 500 || len(in.RequestKey) > 128 {
		return out, invalid("invalid reconcile_deployment request")
	}
	a := s.deployments
	if a == nil {
		return out, invalid("deployments not configured")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := s.deploymentSnapshot(ctx)
	if err != nil {
		return out, err
	}
	var d domain.Deployment
	for _, v := range st.Deployments {
		if v.ID == in.DeploymentID && v.ProductID == in.ProductID {
			d = v
			break
		}
	}
	if d.ID == "" {
		return out, missing("deployment", in.DeploymentID)
	}
	if in.RequestKey != "" {
		var receipts []domain.Event
		if err := s.view(ctx, func(state *domain.State) error {
			for _, event := range state.Events {
				if event.Action == "reconcile_deployment" && event.ProductID == in.ProductID && event.Data.Data["request_key"] == in.RequestKey {
					receipts = append(receipts, event)
				}
			}
			return nil
		}); err != nil {
			return out, err
		}
		for _, ev := range receipts {
			if ev.Action == "reconcile_deployment" && ev.ProductID == in.ProductID && ev.Data.Data["request_key"] == in.RequestKey {
				if ev.EntityID != d.ID || ev.Data.Data["reason"] != in.Reason {
					return out, invalid("REQUEST_KEY_CONFLICT")
				}
				return domain.ReconcileDeploymentResult{Deployment: d, Resolution: fmt.Sprint(ev.Data.Data["resolution"])}, nil
			}
		}
	}
	if deploymentTerminal(d) {
		return out, invalid("only active deployments may be reconciled")
	}
	p, ok := s.builds.config.Profile(d.ProfileID)
	g, gok := s.builds.config.DevGitOps()
	if !ok || !gok || p.ProductID != d.ProductID {
		return out, invalid("deployment authority unavailable")
	}
	if d.Branch != "rcp/deploy/"+d.ID {
		return out, invalid("deployment branch is outside its configured identity")
	}
	previousStatus := d.Status
	d, resolution, err := s.resolveDeployment(ctx, d, p, g, in.Reason, "OPERATOR_CLOSED")
	if err != nil {
		return out, err
	}
	d.Actor = attributedActor(ctx, "agent")
	if err = s.advanceDeployment(ctx, staleSnapshot(st, d.ID), &d, false); err != nil {
		return out, err
	}
	err = s.store.Update(ctx, func(state *domain.State) error {
		deploymentEvent(state, d, "reconcile_deployment")
		data := state.Events[len(state.Events)-1].Data.Data
		data["reason"] = in.Reason
		data["request_key"] = in.RequestKey
		data["resolution"] = resolution
		data["previous_status"] = previousStatus
		return nil
	})
	if err != nil {
		return out, err
	}
	out = domain.ReconcileDeploymentResult{Deployment: d, Resolution: resolution}
	return out, nil
}

func staleSnapshot(st domain.State, id string) domain.Deployment {
	for _, d := range st.Deployments {
		if d.ID == id {
			return d
		}
	}
	return domain.Deployment{}
}

// resolveDeployment is shared by the operator path and the dispatcher watchdog.
// The caller owns deployments.mu; this function deliberately does not lock it.
func (s *Service) resolveDeployment(ctx context.Context, d domain.Deployment, p buildconfig.Profile, g buildconfig.GitOps, reason, closeCode string) (domain.Deployment, string, error) {
	if d.Status == "VERIFYING_ROLLOUT" {
		d = failedDeployment(d, closeCode)
		d.CloseReason = reason
		return d, "closed", nil
	}
	a := s.deployments
	provider, ok := a.gitops.(deploymentReconcilerProvider)
	if !ok {
		return d, "", invalid("GitOps provider does not support reconciliation")
	}
	pr, findErr := a.gitops.FindDeployment(ctx, g, d.Branch)
	if findErr != nil && !errors.Is(findErr, github.ErrNotFound) {
		return d, "", findErr
	}
	prFound := findErr == nil
	closePR := func() (bool, error) {
		if !prFound || pr.State != "open" {
			return false, nil
		}
		closeErr := provider.CloseDeployment(ctx, g, pr.Number)
		if closeErr == nil {
			return false, nil
		}
		var api *github.APIError
		if !strings.Contains(strings.ToLower(closeErr.Error()), "already merged") && (!errors.As(closeErr, &api) || api.Status != 405 && api.Status != 409) {
			return false, closeErr
		}
		again, readErr := a.gitops.ReadDeployment(ctx, g, pr.Number)
		if readErr != nil {
			return false, readErr
		}
		if !again.Merged || !buildconfig.SHA.MatchString(again.MergeSHA) {
			return false, closeErr
		}
		d.PRNumber, d.PRURL, d.HeadSHA, d.MergeCommitSHA = again.Number, again.URL, again.Head.SHA, again.MergeSHA
		d = s.mergedDeployment(d, p)
		return true, nil
	}
	if prFound && pr.Merged {
		if !buildconfig.SHA.MatchString(pr.MergeSHA) {
			return d, "", github.ErrConflict
		}
		d.PRNumber, d.PRURL, d.HeadSHA, d.MergeCommitSHA = pr.Number, pr.URL, pr.Head.SHA, pr.MergeSHA
		return s.mergedDeployment(d, p), "merged", nil
	}
	sha, applied, err := provider.DeploymentAppliedOnMain(ctx, g, deploymentPlan(d, p))
	if err != nil {
		return d, "", err
	}
	if applied {
		d.MergeCommitSHA, d.FailureCode = sha, ""
		d = s.mergedDeployment(d, p)
		merged, closeErr := closePR()
		if closeErr != nil {
			return d, "", closeErr
		}
		if merged {
			return d, "merged", nil
		}
		return d, "applied_on_main", nil
	}
	merged, closeErr := closePR()
	if closeErr != nil {
		return d, "", closeErr
	}
	if merged {
		return d, "merged", nil
	}
	if _, err = a.gitops.DeploymentBranchHead(ctx, g, d.Branch); err == nil {
		if err = provider.DeleteDeploymentBranch(ctx, g, d.Branch); err != nil {
			return d, "", err
		}
	} else if !errors.Is(err, github.ErrNotFound) {
		return d, "", err
	}
	d = failedDeployment(d, closeCode)
	d.CloseReason = reason
	return d, "closed", nil
}
