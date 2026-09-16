package application

import (
	"context"
	"reflect"
	"releasecontrol/internal/domain"
	"time"
)

func flowChildAllowed(st *domain.State, op domain.ExternalOperation) error {
	if op.ParentID == "" {
		return nil
	}
	parent := operationByID(st, op.ParentID)
	if parent == nil || parent.Status == "FAILED" || parent.Status == "CANCELLED" {
		return invalid("parent operation is unavailable or stopped")
	}
	if err := flowGuard(st, *parent); err != nil {
		return err
	}
	if parent.Kind != "RELEASE_CANDIDATE" {
		env := environmentByID(st, op.EnvironmentID)
		if env == nil || env.DesiredOperationID != parent.ID || parent.CompositionSnapshot == nil || env.DesiredCompositionID != parent.CompositionSnapshot.ID {
			return invalid("deployment superseded by another desired composition")
		}
	}
	if parent.Kind == "RELEASE_PROMOTION" {
		if err := releaseWorkReady(st, *parent.CompositionSnapshot); err != nil {
			return err
		}
		if err := scenarioCandidateReady(st, parent.ReleaseCandidateID); err != nil {
			return err
		}
	}
	if op.Snapshot != nil {
		current := environmentBinding(st, op.EnvironmentID, op.ApplicationID)
		if current == nil || !reflect.DeepEqual(*current, op.Snapshot.Target) || !current.AllowDeploy {
			return invalid("deployment configuration changed")
		}
	}
	return nil
}

func (s *Service) verifyPromotionSource(ctx context.Context, op domain.ExternalOperation) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	if err = flowChildAllowed(&st, op); err != nil {
		return err
	}
	parent := operationByID(&st, op.ParentID)
	if parent == nil || parent.Kind != "RELEASE_PROMOTION" {
		return nil
	}
	snap := op.Snapshot
	branches, err := s.provider.Branches(ctx, snap.SourceConnection, snap.BuildRequest.Repository)
	if err != nil {
		return err
	}
	for _, b := range branches {
		if b.Name == configuredBase(snap.Source) && b.SHA == snap.Revision.HeadCommit {
			return nil
		}
	}
	return invalid("main changed after candidate verification; prepare a new candidate")
}

// While an external GitOps write is in flight, selection waits for its bounded
// worker lease. This avoids accepting a new intent between CAS intent and POST.
func selectionAllowed(st *domain.State, environmentID string) error {
	for _, op := range st.Operations {
		if op.EnvironmentID == environmentID && op.Phase == "APPLY_UNCERTAIN" && op.LeaseUntil.After(time.Now().UTC()) && !terminalOperation(op.Status) {
			return invalid("GitOps write in flight; retry composition selection after operation step completes")
		}
	}
	return nil
}
