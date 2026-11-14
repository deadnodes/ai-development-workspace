package application

import (
	"context"
	"errors"
	"releasecontrol/internal/domain"
	"time"
)

var errNoRuntimePrune = errors.New("runtime observation prune became idle")

// PruneRuntimeObservations plans without a write and revalidates its bounded batch under the writer
// lock. Events are implicitly append-only in StateMutationScope.
func (s *Service) PruneRuntimeObservations(ctx context.Context, now time.Time, retention time.Duration) error {
	if retention == 0 {
		return nil
	}
	cutoff := now.Add(-retention)
	var candidates map[string]bool
	if err := s.view(ctx, func(st *domain.State) error {
		candidates = domain.PrunableRuntimeObservations(st, cutoff, 5000)
		return nil
	}); err != nil || len(candidates) == 0 {
		return err
	}
	scoped, ok := s.store.(interface {
		UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
	})
	// Never silently fall back to a whole-state mutation for this maintenance pass.
	if !ok {
		return nil
	}
	err := scoped.UpdateScoped(ctx, domain.StateMutationScope{MutableCollections: []string{"runtime_observations"}}, func(st *domain.State) error {
		remove := domain.RevalidateRuntimeObservationCandidates(st, candidates, cutoff)
		if len(remove) == 0 {
			return errNoRuntimePrune
		}
		rows := st.RuntimeObservations[:0]
		for _, r := range st.RuntimeObservations {
			if !remove[r.ID] {
				rows = append(rows, r)
			}
		}
		st.RuntimeObservations = rows
		st.Events = append(st.Events, domain.Event{ID: id(), Actor: "system/runtime-observer", Action: "prune_runtime_observations", At: now, Data: domain.Command{Data: map[string]any{"deleted": len(remove), "cutoff": cutoff.Format(time.RFC3339Nano)}}})
		return nil
	})
	if errors.Is(err, errNoRuntimePrune) {
		return nil
	}
	return err
}
