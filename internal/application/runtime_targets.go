package application

import "releasecontrol/internal/domain"

// latestRuntimeTargets retains each target, including the legacy empty target.
func latestRuntimeTargets(rows []domain.RuntimeObservation, operation string) map[string]domain.RuntimeObservation {
	out := map[string]domain.RuntimeObservation{}
	for _, r := range rows {
		if r.OperationID != operation {
			continue
		}
		old, ok := out[r.Target]
		if !ok || r.CreatedAt.After(old.CreatedAt) || r.CreatedAt.Equal(old.CreatedAt) && r.ID > old.ID {
			out[r.Target] = r
		}
	}
	return out
}
