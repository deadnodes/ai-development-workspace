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
		appendCompositionConflict(st, op, source, time.Now().UTC())
		return nil
	})
}

// appendCompositionConflict is transaction-local: callers own admission policy.
func appendCompositionConflict(st *domain.State, op domain.ExternalOperation, source domain.FlowSource, now time.Time) {
	for _, c := range st.CompositionConflicts {
		if c.OperationID == op.ID && c.RepositoryID == source.RepositoryID {
			return
		}
	}
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
		if v.Status != "verification_required" && v.Status != "resolved" {
			return nil, m, invalid("resolution and check evidence required")
		}
		if err := validateConflictResolutionEvidence(st, *v, input.ResultIDs); err != nil {
			return nil, m, err
		}
		v.ResultIDs = append([]string{}, input.ResultIDs...)
		v.Status = "resolved"
	}
	v.UpdatedAt = time.Now().UTC()
	m = v.Meta
	m.Actor = c.Actor
	return *v, m, nil
}

// Evidence is current at admission, not a permanent permission conferred by the
// historical resolved status. Append order, never caller timestamps, is freshness.
func validateConflictResolutionEvidence(st *domain.State, v domain.CompositionConflict, resultIDs []string) error {
	if !fullSHA.MatchString(v.ResolutionCommit) || len(resultIDs) == 0 || len(v.Integrations) == 0 {
		return invalid("resolution and check evidence required")
	}
	affected := map[string]string{}
	for _, frozen := range v.Integrations {
		current := integration(st, frozen.ID)
		owner := feature(st, frozen.FeatureID)
		if frozen.ID == "" || affected[frozen.ID] != "" || frozen.ProductID != v.ProductID || current == nil || current.ProductID != v.ProductID || current.FeatureID != frozen.FeatureID || owner == nil || owner.ProductID != v.ProductID {
			return invalid("affected resolution integration scope changed")
		}
		affected[frozen.ID] = frozen.FeatureID
	}
	gates := map[string]domain.Gate{}
	for _, gate := range st.Gates {
		relevant := false
		seen := map[string]bool{}
		for _, iid := range gate.IntegrationIDs {
			if fid, ok := affected[iid]; ok {
				if seen[iid] || gate.ProductID != v.ProductID || gate.FeatureID != fid {
					return invalid("resolution gate scope changed")
				}
				seen[iid], relevant = true, true
			}
		}
		if relevant {
			if _, duplicate := gates[gate.ID]; duplicate || gate.ID == "" {
				return invalid("resolution gate identity unavailable")
			}
			gates[gate.ID] = gate
		}
	}
	checks := map[string]domain.Check{}
	checkCounts, gateChecks := map[string]int{}, map[string]int{}
	for _, check := range st.Checks {
		checkCounts[check.ID]++
		if gate, relevant := gates[check.GateID]; relevant {
			if check.ID == "" || check.ProductID != v.ProductID || check.FeatureID != gate.FeatureID {
				return invalid("resolution check scope changed")
			}
			checks[check.ID] = check
			gateChecks[gate.ID]++
		}
	}
	for _, gate := range gates {
		if gate.Blocking && gateChecks[gate.ID] == 0 {
			return invalid("blocking resolution gate has no checks")
		}
	}
	resultIndexes, resultCounts, latest := map[string]int{}, map[string]int{}, map[string]int{}
	for i, result := range st.Results {
		resultIndexes[result.ID], latest[result.CheckID] = i, i
		resultCounts[result.ID]++
	}
	seen, passed, covered := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, rid := range resultIDs {
		if seen[rid] || resultCounts[rid] != 1 {
			return invalid("unique current resolution results required")
		}
		seen[rid] = true
		i := resultIndexes[rid]
		r := st.Results[i]
		check, ok := checks[r.CheckID]
		if !ok || checkCounts[r.CheckID] != 1 || r.ProductID != v.ProductID || r.FeatureID != check.FeatureID || r.Result != "passed" || r.Commit != v.ResolutionCommit || latest[r.CheckID] != i {
			return invalid("resolution checks must be current, scoped and passed on the exact commit")
		}
		passed[check.ID] = true
		for _, iid := range gates[check.GateID].IntegrationIDs {
			if fid, ok := affected[iid]; ok && fid == r.FeatureID {
				covered[iid] = true
			}
		}
	}
	for _, check := range checks {
		if gates[check.GateID].Blocking && !passed[check.ID] {
			return invalid("blocking resolution check %s missing", check.ID)
		}
	}
	for iid := range affected {
		if !covered[iid] {
			return invalid("resolution verification missing for integration %s", iid)
		}
	}
	return nil
}

// Rebuilding is explicit and reuses exactly the captured composition; resolutions
// cannot silently apply to a newly selected set of integrations.
func attachConflictResolutions(st *domain.State, comp domain.Composition, sources []domain.FlowSource, ids []string) error {
	attached := append([]domain.FlowSource(nil), sources...)
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
			if err := validateConflictResolutionEvidence(st, c, c.ResultIDs); err != nil {
				return err
			}
			for i := range attached {
				source := &attached[i]
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
	copy(sources, attached)
	return nil
}

// Recheck persisted queued work through the existing parent/child mutation
// guards. This cannot revoke an external request already sent to a provider.
func validateFlowConflictResolutions(st *domain.State, op domain.ExternalOperation) error {
	ids := []string{}
	for _, source := range op.Sources {
		if source.Plan.ResolutionConflictID == "" && source.Plan.ResolutionSHA == "" {
			continue
		}
		if source.Plan.ResolutionConflictID == "" || source.Plan.ResolutionSHA == "" {
			return invalid("incomplete queued resolution identity")
		}
		ids = append(ids, source.Plan.ResolutionConflictID)
	}
	if len(ids) == 0 {
		return nil
	}
	if op.CompositionSnapshot == nil || op.ProductID != op.CompositionSnapshot.ProductID {
		return invalid("queued resolution composition unavailable")
	}
	checked := append([]domain.FlowSource(nil), op.Sources...)
	if err := attachConflictResolutions(st, *op.CompositionSnapshot, checked, ids); err != nil {
		return err
	}
	for i := range checked {
		if checked[i].Plan.ResolutionSHA != op.Sources[i].Plan.ResolutionSHA || checked[i].Plan.ResolutionConflictID != op.Sources[i].Plan.ResolutionConflictID {
			return invalid("queued resolution changed")
		}
	}
	return nil
}
