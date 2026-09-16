package application

import (
	"context"
	"releasecontrol/internal/domain"
	"slices"
	"time"
)

func (s *Service) saveCompositionConflict(ctx context.Context, op domain.ExternalOperation, source domain.FlowSource, token string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token {
			return invalid("operation lease changed")
		}
		for _, c := range st.CompositionConflicts {
			if c.OperationID == op.ID && c.RepositoryID == source.RepositoryID {
				return nil
			}
		}
		now := time.Now().UTC()
		c := domain.CompositionConflict{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, Actor: op.RequestedBy, CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, RepositoryID: source.RepositoryID, Source: source, Status: "requires_resolution"}
		if op.CompositionSnapshot != nil {
			c.CompositionID = op.CompositionSnapshot.ID
			affected := []string{}
			for _, r := range op.CompositionSnapshot.RevisionSnapshots {
				if r.RepositoryID == source.RepositoryID && !slices.Contains(affected, r.IntegrationID) {
					affected = append(affected, r.IntegrationID)
				}
			}
			for _, iid := range affected {
				if in := integration(st, iid); in != nil {
					c.Integrations = append(c.Integrations, *in)
					if f := feature(st, in.FeatureID); f != nil {
						c.Features = append(c.Features, *f)
					}
					for _, m := range st.Memories {
						if m.FeatureID == in.FeatureID && (m.IntegrationID == "" || m.IntegrationID == iid) {
							c.Memories = append(c.Memories, m)
						}
					}
				}
			}
		}
		for _, gate := range st.Gates {
			relevant := false
			for _, in := range c.Integrations {
				if slices.Contains(gate.IntegrationIDs, in.ID) {
					relevant = true
				}
			}
			if relevant {
				c.Gates = append(c.Gates, gate)
				for _, check := range st.Checks {
					if check.GateID == gate.ID {
						c.Checks = append(c.Checks, check)
					}
				}
			}
		}
		st.CompositionConflicts = append(st.CompositionConflicts, c)
		st.Events = append(st.Events, domain.Event{ID: id(), Action: "composition_conflict_detected", Actor: op.RequestedBy, At: now, EntityID: c.ID, ProductID: op.ProductID})
		return nil
	})
}

func applyCompositionConflict(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	var input struct {
		ConflictID string   `json:"conflict_id"`
		Commit     string   `json:"commit"`
		Rationale  string   `json:"rationale"`
		ResultIDs  []string `json:"result_ids"`
	}
	if err := decode(c.Data, &input); err != nil {
		return nil, m, err
	}
	var v *domain.CompositionConflict
	for i := range st.CompositionConflicts {
		if st.CompositionConflicts[i].ID == input.ConflictID {
			v = &st.CompositionConflicts[i]
			break
		}
	}
	if v == nil {
		return nil, m, missing("conflict", input.ConflictID)
	}
	if c.ProductID == "" || c.ProductID != v.ProductID {
		return nil, m, invalid("conflict product mismatch")
	}
	switch c.Action {
	case "claim_composition_conflict":
		if v.Status != "requires_resolution" && !(v.Status == "claimed" && v.ClaimedBy == c.Actor) {
			return nil, m, invalid("conflict already claimed or resolved")
		}
		v.Status = "claimed"
		v.ClaimedBy = c.Actor
	case "record_conflict_resolution":
		if v.Status != "claimed" || v.ClaimedBy != c.Actor || !fullSHA.MatchString(input.Commit) || input.Rationale == "" {
			return nil, m, invalid("claim owner, exact commit and rationale required")
		}
		v.ResolutionCommit = input.Commit
		v.Rationale = input.Rationale
		v.Status = "verification_required"
	case "verify_conflict_resolution":
		if v.Status != "verification_required" || len(input.ResultIDs) == 0 {
			return nil, m, invalid("resolution and check evidence required")
		}
		seen := map[string]bool{}
		for _, rid := range input.ResultIDs {
			if seen[rid] {
				return nil, m, invalid("duplicate result")
			}
			seen[rid] = true
			found := false
			for resultIndex, r := range st.Results {
				if r.ID == rid && r.ProductID == v.ProductID && r.Result == "passed" && r.Commit == v.ResolutionCommit {
					for _, newer := range st.Results[resultIndex+1:] {
						if newer.CheckID == r.CheckID {
							return nil, m, invalid("resolution result superseded; use latest check evidence")
						}
					}
					for _, in := range v.Integrations {
						if in.FeatureID == r.FeatureID {
							found = true
						}
					}
				}
			}
			if !found {
				return nil, m, invalid("checks must pass on exact resolution commit within affected features")
			}
		}
		// Every affected Integration needs scoped check evidence, including two
		// integrations inside one Feature. All blocking checks must pass.
		for _, in := range v.Integrations {
			verified := false
			for _, gate := range st.Gates {
				if gate.ProductID != v.ProductID || !slices.Contains(gate.IntegrationIDs, in.ID) {
					continue
				}
				for _, check := range st.Checks {
					if check.GateID != gate.ID {
						continue
					}
					passed := false
					for _, r := range st.Results {
						if slices.Contains(input.ResultIDs, r.ID) && r.CheckID == check.ID {
							passed = true
						}
					}
					if gate.Blocking && !passed {
						return nil, m, invalid("blocking resolution check %s missing", check.ID)
					}
					verified = verified || passed
				}
			}
			if !verified {
				return nil, m, invalid("resolution verification missing for integration %s", in.ID)
			}
		}
		v.ResultIDs = append([]string{}, input.ResultIDs...)
		v.Status = "resolved"
	}
	v.UpdatedAt = time.Now().UTC()
	m = v.Meta
	m.Actor = c.Actor
	return *v, m, nil
}

// Rebuilding is explicit and reuses exactly the captured composition; resolutions
// cannot silently apply to a newly selected set of integrations.
func attachConflictResolutions(st *domain.State, comp domain.Composition, sources []domain.FlowSource, ids []string) error {
	used := map[string]bool{}
	for _, id := range ids {
		found := false
		for _, c := range st.CompositionConflicts {
			if c.ID != id {
				continue
			}
			if c.ProductID != comp.ProductID || c.CompositionID != comp.ID || c.Status != "resolved" || !fullSHA.MatchString(c.ResolutionCommit) {
				return invalid("verified resolution for exact composition required")
			}
			for i := range sources {
				source := &sources[i]
				if source.RepositoryID != c.RepositoryID {
					continue
				}
				if used[c.RepositoryID] || source.Plan.BaseSHA != c.Source.Plan.BaseSHA || !slices.Equal(source.Plan.HeadSHAs, c.Source.Plan.HeadSHAs) {
					return invalid("resolution source selection changed")
				}
				used[c.RepositoryID] = true
				source.Plan.ResolutionSHA = c.ResolutionCommit
				source.Plan.ResolutionConflictID = c.ID
				found = true
			}
		}
		if !found {
			return invalid("resolution unavailable for composition")
		}
	}
	return nil
}
