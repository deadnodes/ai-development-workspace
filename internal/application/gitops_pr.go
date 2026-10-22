package application

import (
	"context"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

func gitOpsPRApply(op domain.ExternalOperation) delivery.GitOpsApply {
	return delivery.GitOpsApply{Request: op.Snapshot.GitOpsRequest, ExpectedHeadSHA: op.GitOpsBase.HeadSHA, ExpectedBlobSHA: op.GitOpsBase.BlobSHA, Digest: op.Artifact.Digest, OperationID: op.ID, Message: "release-control: " + op.ID}
}
func (s *Service) proposeGitOpsPR(callCtx, ctx context.Context, op domain.ExternalOperation, token string) error {
	return s.proposeGitOpsPRMode(callCtx, ctx, op, token, false)
}
func (s *Service) proposeDevActivationPR(callCtx, ctx context.Context, op domain.ExternalOperation, token string) error {
	return s.proposeGitOpsPRMode(callCtx, ctx, op, token, true)
}
func (s *Service) proposeGitOpsPRMode(callCtx, ctx context.Context, op domain.ExternalOperation, token string, activation bool) error {
	p, ok := s.provider.(delivery.GitOpsPRProvider)
	if !ok {
		return s.finish(ctx, op, token, "BLOCKED", "provider does not support GitOps pull requests", nil)
	}
	var pr delivery.GitOpsPullRequest
	var e error
	if activation {
		guarded, ok := s.provider.(delivery.GitOpsGuardedPRProvider)
		if !ok {
			return s.retry(ctx, op, token, "candidate Dev proposal unavailable: per-HTTP guarded provider required; no unguarded fallback")
		}
		if e = s.validateDevActivationIO(callCtx, op); e != nil {
			return e
		}
		pr, e = guarded.ProposeGitOpsGuarded(callCtx, op.Snapshot.GitOpsConnection, gitOpsPRApply(op), func(callbackCtx context.Context) error { return s.validateDevActivationIO(callbackCtx, op) })
	} else {
		pr, e = p.ProposeGitOps(callCtx, op.Snapshot.GitOpsConnection, gitOpsPRApply(op))
	}
	if e != nil {
		return s.retry(ctx, op, token, "GitOps PR proposal: "+e.Error())
	}
	if pr.Number <= 0 || pr.URL == "" || pr.HeadSHA == "" {
		return s.finish(ctx, op, token, "BLOCKED", "PR provider returned incomplete evidence", nil)
	}
	if activation {
		lookup, ok := s.provider.(delivery.GitOpsProposalLookup)
		if !ok {
			return s.retry(ctx, op, token, "candidate Dev proposal outcome uncertain; complete GET-only first-head verification unavailable")
		}
		verified, err := lookup.LookupGitOpsProposal(callCtx, op.Snapshot.GitOpsConnection, gitOpsPRApply(op))
		if err != nil || verified.Number != pr.Number || verified.URL != pr.URL || verified.HeadSHA != pr.HeadSHA {
			return s.retry(ctx, op, token, "candidate Dev proposal outcome uncertain; complete first-head proof disagrees or is unavailable; no write repeated")
		}
		if err := s.validateDevActivationIO(ctx, op); err != nil {
			return err
		}
		pr = verified
	}
	op.GitOpsPR = &pr
	return s.advance(ctx, op, token, "WAIT_GITOPS_PR", "GITOPS_PENDING", "GitOps pull request awaits external review and merge; no deployment yet", map[string]string{"pull_request": pr.URL, "head": pr.HeadSHA})
}
func (s *Service) waitGitOpsPR(callCtx, ctx context.Context, op domain.ExternalOperation, token string) error {
	state, readErr := s.store.Read(ctx)
	if readErr != nil {
		return readErr
	}
	if devActivationParent(&state, op) != nil {
		if err := s.verifyNativeSource(callCtx, op); err != nil {
			return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
		}
		if err := s.validateDevActivationIO(ctx, op); err != nil {
			return err
		}
	}
	p, ok := s.provider.(delivery.GitOpsPRProvider)
	if !ok || op.GitOpsPR == nil || op.GitOpsBase == nil || op.Artifact == nil {
		return s.finish(ctx, op, token, "FAILED", "missing GitOps PR evidence/provider", nil)
	}
	st, e := s.State(ctx)
	if e != nil {
		return e
	}
	target := environmentBinding(&st, op.EnvironmentID, op.ApplicationID)
	if target == nil || !reflect.DeepEqual(*target, op.Snapshot.Target) || !target.AllowDeploy {
		return s.finish(ctx, op, token, "BLOCKED", "GitOps policy changed while PR awaiting merge", nil)
	}
	pr, e := p.ObserveGitOpsPR(callCtx, op.Snapshot.GitOpsConnection, gitOpsPRApply(op), *op.GitOpsPR)
	if e != nil {
		return s.retry(ctx, op, token, "observe GitOps PR: "+e.Error())
	}
	op.GitOpsPR = &pr
	if !pr.Merged {
		if pr.State == "closed" {
			return s.finish(ctx, op, token, "CANCELLED", "GitOps PR closed without merge", nil)
		}
		return s.save(ctx, op, token, "WAIT_GITOPS_PR", "GITOPS_PENDING", "Awaiting external PR merge", nil, true)
	}
	req := op.Snapshot.GitOpsRequest
	req.ExpectedHeadSHA = ""
	current, e := s.provider.ReadGitOps(callCtx, op.Snapshot.GitOpsConnection, req)
	if e != nil {
		return s.retry(ctx, op, token, e.Error())
	}
	if current.Digest != op.Artifact.Digest || current.ImageRepository != op.Snapshot.Build.ImageRepository {
		return s.finish(ctx, op, token, "BLOCKED", "merged PR does not match desired image in target branch", nil)
	}
	op.GitOpsResult = &delivery.GitOpsResult{CommitSHA: current.HeadSHA, BlobSHA: current.BlobSHA, URL: pr.URL}
	return s.finish(ctx, op, token, "GITOPS_APPLIED", "PR merged and exact image observed in GitOps; Flux/runtime still pending", map[string]string{"pull_request": pr.URL, "commit": current.HeadSHA, "digest": current.Digest})
}
