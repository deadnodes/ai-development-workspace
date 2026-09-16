package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"time"
)

// ObserveRuntimeOnce only observes the latest successful GitOps write per target.
// Unknown/mismatched evidence is retained as unhealthy; it never advances a release.
func (s *Service) ObserveRuntimeOnce(ctx context.Context, observers []delivery.RuntimeObserver) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	for _, observer := range observers {
		t := observer.TargetIdentity()
		var latest *domain.ExternalOperation
		for i := range st.Operations {
			o := &st.Operations[i]
			if o.ProductID == t.ProductID && o.EnvironmentID == t.EnvironmentID && o.ApplicationID == t.ApplicationID && o.Status == "SUCCEEDED" && o.GitOpsResult != nil && o.Artifact != nil && (latest == nil || o.CreatedAt.After(latest.CreatedAt)) {
				latest = o
			}
		}
		if latest == nil {
			continue
		}
		result := observer.Observe(ctx, latest.GitOpsResult.CommitSHA, latest.Artifact.Digest)
		// Keep append-only changes and periodic freshness evidence, without poll noise.
		var previous *domain.RuntimeObservation
		for i := range st.RuntimeObservations {
			r := &st.RuntimeObservations[i]
			if r.OperationID == latest.ID && (previous == nil || r.CreatedAt.After(previous.CreatedAt)) {
				previous = r
			}
		}
		if previous != nil && previous.Healthy == result.Healthy && previous.Details == result.Details && time.Since(previous.CreatedAt) < time.Minute {
			continue
		}
		_, err = s.Execute(ctx, domain.Command{Action: "record_runtime_observation", Actor: "system/runtime-observer", ID: latest.ID, Data: map[string]any{"environment_id": t.EnvironmentID, "gitops_commit": latest.GitOpsResult.CommitSHA, "artifact_digest": latest.Artifact.Digest, "healthy": result.Healthy, "details": result.Details}})
		if err != nil {
			return err
		}
	}
	return nil
}
