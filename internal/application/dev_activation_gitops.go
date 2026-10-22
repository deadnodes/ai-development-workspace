package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

func (s *Service) applyDevActivation(callCtx, ctx context.Context, op domain.ExternalOperation, token string) error {
	if op.Artifact == nil || op.GitOpsBase == nil || op.Snapshot == nil || op.Snapshot.Target.GitOpsMode != "PR" {
		return s.finish(ctx, op, token, "BLOCKED", "candidate Dev exact artifact/base/PR target required", nil)
	}
	if err := s.recheckActivationBaseline(callCtx, op); err != nil {
		return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
	}
	if err := s.verifyNativeSource(callCtx, op); err != nil {
		return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
	}
	if err := s.validateDevActivationIO(ctx, op); err != nil {
		return err
	}
	// An unknown outcome authorizes GET-only recovery, never another proposal,
	// even when only a branch or a commit (rather than a PR) was created.
	if op.Phase == "APPLY_UNCERTAIN" {
		lookup, ok := s.provider.(delivery.GitOpsProposalLookup)
		if !ok {
			return s.retry(ctx, op, token, "candidate Dev outcome uncertain; provider has no GET-only proposal lookup")
		}
		pr, err := lookup.LookupGitOpsProposal(callCtx, op.Snapshot.GitOpsConnection, gitOpsPRApply(op))
		if err != nil || pr.Number <= 0 || pr.URL == "" || pr.HeadSHA == "" {
			return s.retry(ctx, op, token, "candidate Dev outcome uncertain; exact existing PR not proven, no write repeated")
		}
		if err := s.validateDevActivationIO(ctx, op); err != nil {
			return err
		}
		op.GitOpsPR = &pr
		return s.advance(ctx, op, token, "WAIT_GITOPS_PR", "GITOPS_PENDING", "existing exact candidate Dev proposal recovered using GET-only evidence", map[string]string{"pull_request": pr.URL, "head": pr.HeadSHA})
	}
	current, err := s.provider.ReadGitOps(callCtx, op.Snapshot.GitOpsConnection, op.Snapshot.GitOpsRequest)
	if err != nil {
		return s.retry(ctx, op, token, "candidate Dev GitOps read failed")
	}
	if err := s.validateDevActivationIO(ctx, op); err != nil {
		return err
	}
	if current.Digest == op.Artifact.Digest && current.ImageRepository == op.Snapshot.Build.ImageRepository && current.HeadSHA != "" && current.BlobSHA != "" {
		op.GitOpsResult = &delivery.GitOpsResult{CommitSHA: current.HeadSHA, BlobSHA: current.BlobSHA, AlreadyApplied: true}
		return s.finish(ctx, op, token, "GITOPS_APPLIED", "PENDING_RECONCILIATION: exact candidate digest already in desired GitOps; runtime unverified", nil)
	}
	if current.BlobSHA != op.GitOpsBase.BlobSHA {
		return s.finish(ctx, op, token, "BLOCKED", "candidate Dev GitOps path changed since inspection", nil)
	}
	// Sibling components may have prepared/merged a separate path. Freeze the
	// latest observed branch head only while our exact target blob is unchanged.
	op.GitOpsBase.HeadSHA = current.HeadSHA
	if op.GitOpsBase.HeadSHA == "" {
		return s.finish(ctx, op, token, "BLOCKED", "candidate Dev GitOps head missing", nil)
	}
	if _, ok := s.provider.(delivery.GitOpsPRProvider); !ok {
		return s.finish(ctx, op, token, "BLOCKED", "candidate Dev PR provider required", nil)
	}
	if err := s.save(ctx, op, token, "APPLY_UNCERTAIN", "GITOPS_PENDING", "candidate Dev one-shot proposal intent committed", nil, false); err != nil {
		return err
	}
	op.Phase = "APPLY_UNCERTAIN"
	op.ReservationMutationIntent = "GITOPS"
	op.DevActivationIntentHash = workerconfig.Fingerprint(gitOpsPRApply(op))
	// Revalidate once more after durable intent and immediately before POST.
	if err := s.verifyNativeSource(callCtx, op); err != nil {
		return err
	}
	if err := s.validateDevActivationIO(ctx, op); err != nil {
		return err
	}
	return s.proposeDevActivationPR(callCtx, ctx, op, token)
}
