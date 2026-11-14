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
		operationID, commit, digest    string
		previousID                     string
		previousHealthy                bool
		previousDetails                string
		previousCommit, previousDigest string
		hasPrevious                    bool
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
					inputs[i].previousID = r.ID
					inputs[i].previousHealthy, inputs[i].previousDetails = r.Healthy, r.Details
					inputs[i].previousCommit, inputs[i].previousDigest = r.GitOpsCommit, r.ArtifactDigest
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Poll liveness is separate from append-only change evidence.
		if in.hasPrevious && in.previousHealthy == result.Healthy && in.previousDetails == result.Details && in.previousCommit == in.commit && in.previousDigest == in.digest {
			if err := s.touchRuntimeFreshness(ctx, in.operationID, in.previousID, result.Healthy); err != nil {
				return err
			}
			continue
		}
		recorded, err := s.Execute(ctx, domain.Command{Action: "record_runtime_observation", Actor: "system/runtime-observer", ID: in.operationID, Data: map[string]any{"environment_id": t.EnvironmentID, "gitops_commit": in.commit, "artifact_digest": in.digest, "healthy": result.Healthy, "details": result.Details}})
		if err != nil {
			return err
		}
		observation, ok := recorded.(domain.RuntimeObservation)
		if !ok {
			continue
		}
		if err := s.touchRuntimeFreshness(ctx, in.operationID, observation.ID, result.Healthy); err != nil {
			return err
		}
	}
	return nil
}

// RuntimeFreshnessStore is optional; lightweight liveness is never domain State.
type RuntimeFreshnessStore interface {
	TouchRuntimeFreshness(context.Context, string, string, time.Time, bool) error
	RuntimeFreshness(context.Context, []string) (map[string]domain.RuntimeFreshness, error)
}

// The ID belongs to the evidence this poll compared or recorded. Never reselect
// the latest row here: another observer may have appended different evidence.
func (s *Service) touchRuntimeFreshness(ctx context.Context, operationID, observationID string, healthy bool) error {
	if observationID == "" {
		return nil
	}
	if store, ok := s.store.(RuntimeFreshnessStore); ok {
		return store.TouchRuntimeFreshness(ctx, operationID, observationID, time.Now().UTC(), healthy)
	}
	return nil
}
func (s *Service) runtimeFreshness(ctx context.Context, st *domain.State) (map[string]time.Time, error) {
	store, ok := s.store.(RuntimeFreshnessStore)
	if !ok {
		return nil, nil
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, r := range st.RuntimeObservations {
		if !seen[r.OperationID] {
			seen[r.OperationID] = true
			ids = append(ids, r.OperationID)
		}
	}
	freshness, err := store.RuntimeFreshness(ctx, ids)
	return domain.BindRuntimeFreshness(st.RuntimeObservations, freshness), err
}

// runtimeObservationView preserves provenance timestamps and exposes liveness.
type runtimeObservationView struct {
	domain.RuntimeObservation
	ObservedAt time.Time `json:"observed_at"`
}

func observationViews(rows []domain.RuntimeObservation, freshness map[string]time.Time) []runtimeObservationView {
	out := make([]runtimeObservationView, len(rows))
	for i, r := range rows {
		out[i] = runtimeObservationView{r, domain.RuntimeObservedAt(r, freshness)}
	}
	return out
}

func (s *Service) loadRuntimeFreshness(ctx context.Context) (map[string]time.Time, error) {
	// Do not borrow the store's read transaction while acquiring a freshness
	// connection: installations may have a single-connection pool.
	var snapshot domain.State
	if err := s.view(ctx, func(st *domain.State) error {
		snapshot.RuntimeObservations = append([]domain.RuntimeObservation(nil), st.RuntimeObservations...)
		return nil
	}); err != nil {
		return nil, err
	}
	return s.runtimeFreshness(ctx, &snapshot)
}

func operationObservations(st *domain.State, operationID string) []domain.RuntimeObservation {
	rows := []domain.RuntimeObservation{}
	for _, r := range st.RuntimeObservations {
		if r.OperationID == operationID {
			rows = append(rows, r)
		}
	}
	return rows
}
