package application

import (
	"context"
	"errors"
	"releasecontrol/internal/domain"
	"time"
)

const OperationPruneLimit = 5000

var errNoOperationPrune = errors.New("operation prune became idle")

// PruneOperations plans without a write and revalidates its bounded batch under the writer
// lock. Events are implicitly append-only in StateMutationScope.
func (s *Service) PruneOperations(ctx context.Context, now time.Time, retention, refreshRetention time.Duration) (int, error) {
	if retention == 0 {
		return 0, nil
	}
	if refreshRetention == 0 {
		refreshRetention = retention
	}
	cutoff := now.Add(-retention)
	refreshCutoff := now.Add(-refreshRetention)
	var candidates map[string]bool
	if err := s.view(ctx, func(st *domain.State) error {
		candidates = domain.PrunableOperations(st, cutoff, refreshCutoff, OperationPruneLimit)
		return nil
	}); err != nil || len(candidates) == 0 {
		return 0, err
	}
	scoped, ok := s.store.(interface {
		UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
	})
	// Never silently fall back to a whole-state mutation for this maintenance pass.
	if !ok {
		return 0, nil
	}
	deleted := 0
	err := scoped.UpdateScoped(ctx, domain.StateMutationScope{MutableCollections: []string{"operations", "operation_steps"}}, func(st *domain.State) error {
		remove := domain.RevalidateOperationCandidates(st, candidates, cutoff, refreshCutoff)
		if len(remove) == 0 {
			return errNoOperationPrune
		}
		deleted = len(remove)
		rows := st.Operations[:0]
		for _, r := range st.Operations {
			if !remove[r.ID] {
				rows = append(rows, r)
			}
		}
		st.Operations = rows
		steps := st.OperationSteps[:0]
		deletedSteps := 0
		for _, step := range st.OperationSteps {
			if remove[step.OperationID] {
				deletedSteps++
				continue
			}
			steps = append(steps, step)
		}
		st.OperationSteps = steps
		st.Events = append(st.Events, domain.Event{ID: id(), Actor: "system/operation-retention", Action: "prune_operations", At: now, Data: domain.Command{Data: map[string]any{"deleted_operations": len(remove), "deleted_steps": deletedSteps, "cutoff": cutoff.Format(time.RFC3339Nano), "refresh_cutoff": refreshCutoff.Format(time.RFC3339Nano)}}})
		return nil
	})
	if errors.Is(err, errNoOperationPrune) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
