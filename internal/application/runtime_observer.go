package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"time"
)

// ObserveRuntimeOnce only observes the latest successful GitOps write per target.
// Only legacy DEPLOY ExternalOperations with GitOpsResult are considered, not
// Forgejo Deployment records (deploy_build).
// Unknown/mismatched evidence is retained as unhealthy; it never advances a release.
func (s *Service) ObserveRuntimeOnce(ctx context.Context, observers []delivery.RuntimeObserver) error {
	type input struct {
		operationID, commit, digest string
		previousHealthy             bool
		previousDetails             string
		previousAt                  time.Time
		hasPrevious                 bool
	}
	targets := make([]delivery.RuntimeTarget, len(observers))
	for i, observer := range observers {
		targets[i] = observer.TargetIdentity()
	}
	inputs := make([]input, len(observers))
	err := s.view(ctx, func(st *domain.State) error {
		latest := make(map[delivery.RuntimeTarget]*domain.ExternalOperation)
		for i := range st.Operations {
			o := &st.Operations[i]
			if o.Status != "SUCCEEDED" || o.GitOpsResult == nil || o.Artifact == nil {
				continue
			}
			target := delivery.RuntimeTarget{ProductID: o.ProductID, EnvironmentID: o.EnvironmentID, ApplicationID: o.ApplicationID}
			if previous := latest[target]; previous == nil || o.CreatedAt.After(previous.CreatedAt) {
				latest[target] = o
			}
		}
		previous := make(map[string]*domain.RuntimeObservation)
		for i := range st.RuntimeObservations {
			r := &st.RuntimeObservations[i]
			if old := previous[r.OperationID]; old == nil || r.CreatedAt.After(old.CreatedAt) {
				previous[r.OperationID] = r
			}
		}
		for i, target := range targets {
			if o := latest[target]; o != nil {
				inputs[i] = input{operationID: o.ID, commit: o.GitOpsResult.CommitSHA, digest: o.Artifact.Digest}
				if r := previous[o.ID]; r != nil {
					inputs[i].hasPrevious = true
					inputs[i].previousHealthy, inputs[i].previousDetails, inputs[i].previousAt = r.Healthy, r.Details, r.CreatedAt
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i, observer := range observers {
		t, in := targets[i], inputs[i]
		if in.operationID == "" {
			continue
		}
		result := observer.Observe(ctx, in.commit, in.digest)
		// Keep append-only changes and periodic freshness evidence, without poll noise.
		if in.hasPrevious && in.previousHealthy == result.Healthy && in.previousDetails == result.Details && time.Since(in.previousAt) < time.Minute {
			continue
		}
		_, err = s.Execute(ctx, domain.Command{Action: "record_runtime_observation", Actor: "system/runtime-observer", ID: in.operationID, Data: map[string]any{"environment_id": t.EnvironmentID, "gitops_commit": in.commit, "artifact_digest": in.digest, "healthy": result.Healthy, "details": result.Details}})
		if err != nil {
			return err
		}
	}
	return nil
}
