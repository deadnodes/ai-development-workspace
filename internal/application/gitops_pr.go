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
	p, ok := s.provider.(delivery.GitOpsPRProvider)
	if !ok {
		return s.finish(ctx, op, token, "BLOCKED", "provider does not support GitOps pull requests", nil)
	}
	pr, e := p.ProposeGitOps(callCtx, op.Snapshot.GitOpsConnection, gitOpsPRApply(op))
	if e != nil {
		return s.retry(ctx, op, token, "GitOps PR proposal: "+e.Error())
	}
	if pr.Number <= 0 || pr.URL == "" || pr.HeadSHA == "" {
		return s.finish(ctx, op, token, "BLOCKED", "PR provider returned incomplete evidence", nil)
	}
	op.GitOpsPR = &pr
	return s.advance(ctx, op, token, "WAIT_GITOPS_PR", "GITOPS_PENDING", "GitOps pull request awaits external review and merge; no deployment yet", map[string]string{"pull_request": pr.URL, "head": pr.HeadSHA})
}
func (s *Service) waitGitOpsPR(callCtx, ctx context.Context, op domain.ExternalOperation, token string) error {
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
