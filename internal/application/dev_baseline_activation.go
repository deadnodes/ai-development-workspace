package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"time"
)

func baselineCaptureHash(op domain.ExternalOperation) string {
	return workerconfig.Fingerprint(struct {
		Proof  *domain.DevBaselineProof
		Result *domain.DevBaselineResult
	}{op.DevBaseline, op.DevBaselineResult})
}
func activationHasBaselineEffect(st *domain.State, parent domain.ExternalOperation) bool {
	for _, cid := range parent.ChildIDs {
		child := operationByID(st, cid)
		if child != nil && (child.ReservationMutationIntent == "GITOPS" || child.DevActivationIntentHash != "" || child.GitOpsPR != nil || child.GitOpsResult != nil) {
			return true
		}
	}
	return false
}
func (s *Service) activationBaselineAllowed(st *domain.State, parent domain.ExternalOperation) error {
	if parent.DevActivation == nil || parent.DevActivation.BaselineCaptureID == "" {
		return invalid("physical preactivation baseline required; historical activation grants no new rights")
	}
	p := parent.DevActivation
	capture := operationByID(st, p.BaselineCaptureID)
	if capture == nil || capture.DevBaseline == nil || capture.DevBaselineResult == nil || capture.ProductID != parent.ProductID || capture.EnvironmentID != parent.EnvironmentID || capture.RequestedBy != parent.RequestedBy || capture.ReservationEpoch != parent.ReservationEpoch || capture.ParentID != p.ReservationOperationID || baselineCaptureHash(*capture) != p.BaselineCaptureHash || workerconfig.Fingerprint(capture.DevBaselineResult.Components) != p.OriginalBaselineHash || capture.Status != "SUCCEEDED" || capture.Phase != "BASELINE_CAPTURED" || capture.ReservationQuarantined {
		return invalid("exact original baseline capture relation changed")
	}
	if err := s.baselineAllowed(st, *capture, true, false); err != nil {
		return err
	}
	if err := s.validateBaselineResult(st, *capture, *capture.DevBaselineResult); err != nil {
		return err
	}
	if !activationHasBaselineEffect(st, parent) {
		at := capture.DevBaselineResult.ObservedAt
		if at.IsZero() || at.After(time.Now().UTC()) || time.Since(at) > 5*time.Minute {
			return invalid("fresh baseline required before first activation effect")
		}
	}
	return nil
}
func (s *Service) recheckActivationBaseline(ctx context.Context, op domain.ExternalOperation) error {
	st, e := s.store.Read(ctx)
	if e != nil {
		return e
	}
	parent := devActivationParent(&st, op)
	if parent == nil {
		return invalid("candidate activation unavailable")
	}
	if err := s.activationBaselineAllowed(&st, *parent); err != nil {
		return err
	}
	if activationHasBaselineEffect(&st, *parent) {
		return nil
	}
	capture := operationByID(&st, parent.DevActivation.BaselineCaptureID)
	verifier, ok := s.provider.(delivery.GitOpsRevisionVerifier)
	if !ok {
		return invalid("baseline ancestry verifier unavailable")
	}
	heads := map[string]string{}
	for _, c := range capture.DevBaseline.Components {
		if e = s.validateDevActivationIO(ctx, op); e != nil {
			return e
		}
		connection, e := scopedConnection(&st, c.ConnectionID, op.ProductID)
		if e != nil {
			return e
		}
		key := runtimeGitOpsGroupKey(connection.Config, c.GitOps)
		request := delivery.GitOpsRevisionRequest{Repository: c.GitOps.Repository, Ref: c.GitOps.Ref, HeadSHA: heads[key], AncestorSHAs: []string{c.ObservedGitOps.HeadSHA}}
		revision, e := verifier.VerifyGitOpsRevision(ctx, connection.Config, request)
		if e != nil || !validRevisionProof(request, revision) {
			return invalid("baseline captured HEAD no longer contained")
		}
		heads[key] = revision.HeadSHA
		target := c.GitOps
		target.ExpectedHeadSHA = revision.HeadSHA
		current, e := s.provider.ReadGitOps(ctx, connection.Config, target)
		if e != nil || current.ImageRepository != c.ImageRepository || current.Digest != c.Digest || current.HeadSHA != revision.HeadSHA || c.ObservedGitOps == nil || current.BlobSHA != c.ObservedGitOps.BlobSHA || workerconfig.Fingerprint(current.Content) != c.ObservedGitOps.ContentHash {
			return invalid("original shared baseline drifted before candidate effect")
		}
	}
	for _, c := range capture.DevBaseline.Components {
		connection, e := scopedConnection(&st, c.ConnectionID, op.ProductID)
		if e != nil {
			return e
		}
		key := runtimeGitOpsGroupKey(connection.Config, c.GitOps)
		target := c.GitOps
		target.ExpectedHeadSHA = heads[key]
		if e = s.validateDevActivationIO(ctx, op); e != nil {
			return e
		}
		current, e := s.provider.ReadGitOps(ctx, connection.Config, target)
		if e != nil || current.HeadSHA != heads[key] || current.ImageRepository != c.ImageRepository || current.Digest != c.Digest || c.ObservedGitOps == nil || current.BlobSHA != c.ObservedGitOps.BlobSHA || workerconfig.Fingerprint(current.Content) != c.ObservedGitOps.ContentHash {
			return invalid("shared baseline changed during pre-effect read")
		}
	}
	return nil
}

// Admission resolves the latest complete capture before the activation receipt exists.
func (s *Service) currentBaselineForHold(st *domain.State, hold *domain.ExternalOperation) (*domain.ExternalOperation, error) {
	capture, ok := latestBaseline(st, hold.ID)
	if !ok || capture == nil || capture.ProductID != hold.ProductID || capture.RequestedBy != hold.RequestedBy {
		return nil, invalid("latest successful complete baseline capture required")
	}
	if e := s.baselineCurrent(st, *capture, true); e != nil {
		return nil, e
	}
	return capture, nil
}
