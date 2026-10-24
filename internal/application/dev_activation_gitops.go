package application

import (
	"context"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"time"
)

func (s *Service) activationGroupState(ctx context.Context, op domain.ExternalOperation) (domain.State, *domain.DevActivationGroup, error) {
	st, e := s.store.Read(ctx)
	if e != nil {
		return st, nil, e
	}
	p := devActivationParent(&st, op)
	if p == nil || p.DevActivation == nil {
		return st, nil, invalid("candidate Dev group unavailable")
	}
	g := activationGroup(p.DevActivation, op.DevActivationGroupID)
	if g == nil {
		return st, nil, invalid("candidate Dev group missing")
	}
	return st, g, nil
}
func (s *Service) applyDevActivation(callCtx, ctx context.Context, op domain.ExternalOperation, token string) error {
	callCtx = delivery.WithCachedAuthentication(callCtx)
	st, g, e := s.activationGroupState(ctx, op)
	if e != nil {
		return e
	}
	if e = s.validateDevActivationIO(ctx, op); e != nil {
		return e
	}
	if op.ID != g.LeaderID {
		return s.retry(ctx, op, token, "waiting for group leader; no follower proposal authority")
	}
	provider, ok := s.provider.(delivery.GitOpsBatchPRProvider)
	if !ok {
		return s.finish(ctx, op, token, "BLOCKED", "grouped GitOps provider required", nil)
	}
	if op.Phase == "APPLY_UNCERTAIN" {
		if op.DevActivationBatch == nil {
			return s.finish(ctx, op, token, "BLOCKED", "prepared grouped intent missing", nil)
		}
		pr, e := provider.LookupGitOpsBatchProposal(callCtx, g.Connection, *op.DevActivationBatch)
		if e != nil || pr.Number <= 0 || pr.URL == "" || !fullSHA.MatchString(pr.HeadSHA) {
			return s.retry(ctx, op, token, "group proposal uncertain; exact cached-token GET-only proof unavailable; no write repeated")
		}
		op.GitOpsPR = &pr
		return s.advance(ctx, op, token, "WAIT_GITOPS_PR", "GITOPS_PENDING", "exact grouped proposal recovered; external merge required", nil)
	}
	parent := devActivationParent(&st, op)
	for _, id := range g.MemberIDs {
		child := operationByID(&st, id)
		if child == nil || child.Status == "FAILED" || child.Status == "CANCELLED" {
			return s.finish(ctx, op, token, "BLOCKED", "group member failed; reservation retained", nil)
		}
		if child.Phase != "APPLY" || child.Artifact == nil || child.GitOpsBase == nil {
			return s.retry(ctx, op, token, "waiting for all exact group artifacts and baseline inspections")
		}
	}
	if e = s.recheckActivationBaseline(callCtx, op); e != nil {
		return s.finish(ctx, op, token, "BLOCKED", "shared baseline recheck failed", nil)
	}
	verifier, ok := s.provider.(delivery.GitOpsRevisionVerifier)
	if !ok {
		return s.finish(ctx, op, token, "BLOCKED", "group ancestry verifier required", nil)
	}
	batch := delivery.GitOpsBatchApply{Repository: g.Repository, Ref: g.Ref, OperationID: op.ID, Message: "release-control: " + op.ID}
	for _, id := range g.MemberIDs {
		child := operationByID(&st, id)
		c := activationComponent(parent.DevActivation, id)
		if e = s.verifyNativeSource(callCtx, *child); e != nil {
			return s.finish(ctx, op, token, "BLOCKED", "group native source unavailable", nil)
		}
		if e = s.validateDevActivationIO(ctx, op); e != nil {
			return e
		}
		request := delivery.GitOpsRevisionRequest{Repository: g.Repository, Ref: g.Ref, HeadSHA: batch.ExpectedHeadSHA, AncestorSHAs: []string{c.Baseline.HeadSHA}}
		revision, e := verifier.VerifyGitOpsRevision(callCtx, g.Connection, request)
		if e != nil || !validRevisionProof(request, revision) {
			return s.finish(ctx, op, token, "BLOCKED", "group baseline ancestry changed", nil)
		}
		batch.ExpectedHeadSHA = revision.HeadSHA
		selector := c.Selector
		selector.ExpectedHeadSHA = batch.ExpectedHeadSHA
		base, e := s.provider.ReadGitOps(callCtx, g.Connection, selector)
		if e != nil || base.HeadSHA != batch.ExpectedHeadSHA || base.BlobSHA != c.Baseline.BlobSHA || base.ImageRepository != c.Baseline.ImageRepository || base.Digest != c.Baseline.Digest || workerconfig.Fingerprint(base.Content) != c.Baseline.ContentHash {
			return s.finish(ctx, op, token, "BLOCKED", "selected group baseline changed", nil)
		}
		batch.Changes = append(batch.Changes, delivery.GitOpsImageChange{ID: id, Selector: c.Selector, ExpectedBlobSHA: c.Baseline.BlobSHA, ImageRepository: c.Artifact.ImageRepository, Digest: c.Artifact.Digest})
	}
	// Re-read every member with the common head after all preparation reads.
	for _, v := range batch.Changes {
		req := v.Selector
		req.ExpectedHeadSHA = batch.ExpectedHeadSHA
		current, e := s.provider.ReadGitOps(callCtx, g.Connection, req)
		if e != nil || current.HeadSHA != batch.ExpectedHeadSHA || current.BlobSHA != v.ExpectedBlobSHA {
			return s.finish(ctx, op, token, "BLOCKED", "group head changed during preparation", nil)
		}
	}
	op.DevActivationBatch = &batch
	op.DevActivationIntentHash = workerconfig.Fingerprint(batch)
	if e = s.save(ctx, op, token, "APPLY_UNCERTAIN", "GITOPS_PENDING", "one-shot complete group proposal intent frozen", nil, false); e != nil {
		return e
	}
	op.Phase = "APPLY_UNCERTAIN"
	op.ReservationMutationIntent = "GITOPS"
	if e = s.validateDevActivationIO(ctx, op); e != nil {
		return e
	}
	pr, e := provider.ProposeGitOpsBatchGuarded(callCtx, g.Connection, batch, func(c context.Context) error { return s.validateDevActivationIO(c, op) })
	if e != nil {
		return s.retry(ctx, op, token, "group proposal uncertain; GET-only reconciliation required")
	}
	proof, e := provider.LookupGitOpsBatchProposal(callCtx, g.Connection, batch)
	if e != nil || proof.Number != pr.Number || proof.URL != pr.URL || proof.HeadSHA != pr.HeadSHA || pr.Number <= 0 || !fullSHA.MatchString(pr.HeadSHA) {
		return s.retry(ctx, op, token, "complete first grouped proposal proof unavailable; no write repeated")
	}
	op.GitOpsPR = &proof
	return s.advance(ctx, op, token, "WAIT_GITOPS_PR", "GITOPS_PENDING", "group proposal awaits external review and merge", nil)
}

func (s *Service) waitDevActivationGroup(callCtx, ctx context.Context, op domain.ExternalOperation, token string) error {
	callCtx = delivery.WithCachedAuthentication(callCtx)
	st, g, e := s.activationGroupState(ctx, op)
	if e != nil {
		return e
	}
	if op.ID != g.LeaderID || op.DevActivationBatch == nil || op.GitOpsPR == nil {
		return s.finish(ctx, op, token, "BLOCKED", "exact grouped leader proposal required", nil)
	}
	if e = s.validateDevActivationIO(ctx, op); e != nil {
		return e
	}
	if e = s.verifyNativeSource(callCtx, op); e != nil {
		return s.finish(ctx, op, token, "BLOCKED", "group original main source no longer eligible", nil)
	}
	provider, ok := s.provider.(delivery.GitOpsBatchPRProvider)
	if !ok {
		return s.retry(ctx, op, token, "group observation unavailable")
	}
	pr, e := provider.ObserveGitOpsBatchPR(callCtx, g.Connection, *op.DevActivationBatch, *op.GitOpsPR)
	if e != nil {
		return s.retry(ctx, op, token, "exact group PR observation unavailable")
	}
	if pr.Number != op.GitOpsPR.Number || pr.HeadSHA != op.GitOpsPR.HeadSHA || pr.URL != op.GitOpsPR.URL {
		return s.finish(ctx, op, token, "BLOCKED", "group PR identity changed", nil)
	}
	op.GitOpsPR = &pr
	if !pr.Merged {
		if pr.State == "closed" {
			return s.finish(ctx, op, token, "CANCELLED", "group PR closed; full reservation retained", nil)
		}
		return s.save(ctx, op, token, "WAIT_GITOPS_PR", "GITOPS_PENDING", "awaiting external group merge", nil, true)
	}
	verifier, ok := s.provider.(delivery.GitOpsRevisionVerifier)
	if !ok {
		return s.retry(ctx, op, token, "merged group ancestry unavailable")
	}
	request := delivery.GitOpsRevisionRequest{Repository: g.Repository, Ref: g.Ref, AncestorSHAs: []string{pr.HeadSHA}}
	rev, e := verifier.VerifyGitOpsRevision(callCtx, g.Connection, request)
	if e != nil || !validRevisionProof(request, rev) {
		return s.retry(ctx, op, token, "merged group head ancestry unavailable")
	}
	results := map[string]delivery.GitOpsResult{}
	parent := devActivationParent(&st, op)
	for _, id := range g.MemberIDs {
		c := activationComponent(parent.DevActivation, id)
		req := c.Selector
		req.ImageRepository = c.Artifact.ImageRepository
		req.ExpectedHeadSHA = rev.HeadSHA
		current, e := s.provider.ReadGitOps(callCtx, g.Connection, req)
		if e != nil || current.HeadSHA != rev.HeadSHA || !fullSHA.MatchString(current.BlobSHA) || current.ImageRepository != c.Artifact.ImageRepository || current.Digest != c.Artifact.Digest {
			return s.retry(ctx, op, token, "all merged group images not proven at common head")
		}
		results[id] = delivery.GitOpsResult{CommitSHA: rev.HeadSHA, BlobSHA: current.BlobSHA, URL: pr.URL}
	}
	request.HeadSHA = rev.HeadSHA
	stable, e := verifier.VerifyGitOpsRevision(callCtx, g.Connection, request)
	if e != nil || !validRevisionProof(request, stable) || stable.HeadSHA != rev.HeadSHA {
		return s.retry(ctx, op, token, "merged group target moved during observation")
	}
	return s.store.Update(ctx, func(st *domain.State) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		leader := operationByID(st, op.ID)
		if leader == nil || leader.LeaseOwner != token || !leader.LeaseUntil.After(time.Now().UTC()) {
			return invalid("group leader lease lost")
		}
		if e := s.devActivationAllowed(st, op); e != nil {
			return e
		}
		if !reflect.DeepEqual(leader.DevActivationBatch, op.DevActivationBatch) {
			return invalid("group prepared intent changed")
		}
		now := time.Now().UTC()
		for _, id := range g.MemberIDs {
			child := operationByID(st, id)
			result := results[id]
			child.GitOpsPR = &pr
			child.DevActivationPRHash = activationPRHash(pr)
			child.GitOpsResult = &result
			child.DevActivationResultHash = workerconfig.Fingerprint(result)
			child.Status = "SUCCEEDED"
			child.DeploymentState = "GITOPS_APPLIED"
			child.ReservationMutationIntent = ""
			child.Detail = "group desired state applied; runtime unverified; reservation retained"
			child.UpdatedAt = now
			child.FinishedAt = &now
			child.LeaseOwner = ""
			child.LeaseUntil = time.Time{}
		}
		p := devActivationParent(st, op)
		return validateActivationGroups(st, p)
	})
}
