package application

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"releasecontrol/internal/domain"
)

// These DTOs deliberately do not embed conflict/source/configuration snapshots.
// This bound applies only to agent_action, not the existing get_operation reply.
const conflictAgentActionBytes = 32 << 10

type conflictAgentAction struct {
	Type          string                 `json:"type"`
	Status        string                 `json:"status"`
	Reason        string                 `json:"reason,omitempty"`
	ProductID     string                 `json:"product_id,omitempty"`
	OperationID   string                 `json:"operation_id,omitempty"`
	CompositionID string                 `json:"composition_id,omitempty"`
	Conflicts     []conflictAgentContext `json:"conflicts"`
	Instructions  []string               `json:"instructions,omitempty"`
	Workflow      []conflictAgentStep    `json:"workflow,omitempty"`
}

type conflictAgentContext struct {
	ConflictID       string               `json:"conflict_id"`
	Status           string               `json:"status"`
	RepositoryID     string               `json:"repository_id"`
	Repository       string               `json:"provider_repository"`
	BaseRef          string               `json:"base_ref"`
	BaseSHA          string               `json:"base_sha"`
	TargetBranch     string               `json:"target_branch"`
	ResolutionCommit string               `json:"resolution_commit,omitempty"`
	Inputs           []conflictAgentInput `json:"inputs"`
	RequiredChecks   []conflictAgentCheck `json:"required_checks"`
}

type conflictAgentInput struct {
	RevisionID    string     `json:"revision_id"`
	FeatureID     string     `json:"feature_id"`
	IntegrationID string     `json:"integration_id"`
	OriginBranch  string     `json:"origin_branch"`
	HeadSHA       string     `json:"head_sha"`
	RetainedRef   string     `json:"retained_ref,omitempty"`
	ObservedAt    *time.Time `json:"retained_ref_observed_at,omitempty"`
}

type conflictAgentCheck struct {
	CheckID        string   `json:"check_id"`
	GateID         string   `json:"gate_id"`
	IntegrationIDs []string `json:"integration_ids"`
	Blocking       bool     `json:"blocking"`
}

type conflictAgentStep struct {
	Action            string   `json:"action"`
	When              string   `json:"when"`
	RequiredFields    []string `json:"required_fields"`
	MayBuildAndDeploy bool     `json:"may_build_and_prepare_gitops,omitempty"`
}

func conflictLabel(v string, max int) bool {
	return v != "" && len(v) <= max && utf8.ValidString(v) && strings.TrimSpace(v) == v && !strings.ContainsFunc(v, unicode.IsControl)
}

func conflictRepositoryAddress(v string) bool {
	if !conflictLabel(v, 512) {
		return false
	}
	if repositoryName.MatchString(v) {
		return true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == v
}

func unavailableConflictAction(op domain.ExternalOperation, reason string) *conflictAgentAction {
	out := &conflictAgentAction{Type: "RESOLVE_CONFLICT", Status: "UNAVAILABLE", Reason: reason, Conflicts: []conflictAgentContext{}}
	if conflictLabel(op.ProductID, 128) && conflictLabel(op.ID, 128) {
		out.ProductID, out.OperationID = op.ProductID, op.ID
	}
	return out
}

// The requesting agent already exists. This read supplies its next task, never
// starts an agent, contacts Git or grants authority for source/deployment writes.
func operationConflictAction(st *domain.State, op domain.ExternalOperation, now time.Time) *conflictAgentAction {
	conflicts := []domain.CompositionConflict{}
	for _, c := range st.CompositionConflicts {
		if c.OperationID == op.ID && c.ProductID == op.ProductID {
			conflicts = append(conflicts, c)
		}
	}
	// Every observed conflicting repository must have its own scoped record.
	// One complete record cannot make another missing conflict actionable.
	for _, source := range op.Sources {
		if source.Result == nil || !source.Result.Conflict {
			continue
		}
		matches := 0
		for _, c := range conflicts {
			if c.RepositoryID == source.RepositoryID {
				matches++
			}
		}
		if matches != 1 {
			return unavailableConflictAction(op, "CONFLICT_RECORD_MISSING")
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	comp := op.CompositionSnapshot
	if op.Status == "CANCELLED" || op.Kind != "COMPOSE" || comp == nil || comp.ProductID != op.ProductID || !conflictLabel(comp.ID, 128) || !conflictLabel(op.ID, 128) || !conflictLabel(op.ProductID, 128) {
		return unavailableConflictAction(op, "CONFLICT_CONTEXT_UNAVAILABLE")
	}
	if len(conflicts) > 20 || len(comp.Components) > 100 || len(comp.ApplicationSnapshots) > 100 || len(comp.RevisionSnapshots) > 100 || len(op.Sources) > 20 {
		return unavailableConflictAction(op, "CONFLICT_CONTEXT_TOO_LARGE")
	}
	out := &conflictAgentAction{Type: "RESOLVE_CONFLICT", Status: "ACTION_REQUIRED", ProductID: op.ProductID, OperationID: op.ID, CompositionID: comp.ID, Conflicts: []conflictAgentContext{}, Instructions: []string{
		"The requesting agent resolves code externally; RCP does not launch another agent.",
		"Read the affected Features and Integrations through resume/get_integration_context and preserve every side's acceptance criteria.",
		"Branches are frozen input labels; use the exact SHAs. Retained refs are dated stored observations, not current remote availability proof.",
		"Use a separate resolution commit; do not replace the original developer branches. Ask the human if the requirements cannot be reconciled.",
		"Claim ownership, actor labels and this handoff grant no additional permissions. Reconciliation is an explicit new request and may build and prepare configured DEV/TEST GitOps changes.",
	}, Workflow: []conflictAgentStep{
		{Action: "claim_composition_conflict", When: "requires_resolution; an existing claim must belong to the caller", RequiredFields: []string{"product_id", "data.conflict_id"}},
		{Action: "record_conflict_resolution", When: "claimed by the caller, after external resolution", RequiredFields: []string{"product_id", "data.conflict_id", "data.commit", "data.rationale"}},
		{Action: "record_check_result", When: "verification_required, or resolved with superseded evidence; after actual fresh checks on the same resolution commit", RequiredFields: []string{"check_id", "data.result", "data.commit"}},
		{Action: "verify_conflict_resolution", When: "verification_required or resolved; explicitly verify current passing evidence for every affected Integration and blocking check on the same immutable resolution commit", RequiredFields: []string{"product_id", "data.conflict_id", "data.result_ids"}},
		{Action: "reconcile_composition", When: "all required conflicts resolved; only within existing execution authorization", RequiredFields: []string{"id (original composition_id)", "data.resolution_conflict_ids"}, MayBuildAndDeploy: true},
	}}
	seenIDs, seenRepos := map[string]bool{}, map[string]bool{}
	for _, c := range conflicts {
		if seenIDs[c.ID] || seenRepos[c.RepositoryID] {
			return unavailableConflictAction(op, "CONFLICT_CONTEXT_UNAVAILABLE")
		}
		seenIDs[c.ID], seenRepos[c.RepositoryID] = true, true
		v, ok := conflictAgentDetails(op, c, now)
		if !ok {
			return unavailableConflictAction(op, "CONFLICT_CONTEXT_UNAVAILABLE")
		}
		out.Conflicts = append(out.Conflicts, v)
	}
	b, err := json.Marshal(out)
	if err != nil || len(b) > conflictAgentActionBytes {
		return unavailableConflictAction(op, "CONFLICT_CONTEXT_TOO_LARGE")
	}
	return out
}

func conflictAgentDetails(op domain.ExternalOperation, c domain.CompositionConflict, now time.Time) (conflictAgentContext, bool) {
	v := conflictAgentContext{ConflictID: c.ID, Status: c.Status, RepositoryID: c.RepositoryID, Repository: c.Source.Plan.Repository, BaseRef: c.Source.Plan.BaseRef, BaseSHA: c.Source.Plan.BaseSHA, TargetBranch: c.Source.Plan.TargetBranch, ResolutionCommit: c.ResolutionCommit, Inputs: []conflictAgentInput{}, RequiredChecks: []conflictAgentCheck{}}
	comp := op.CompositionSnapshot
	if !conflictLabel(c.ID, 128) || !conflictLabel(c.RepositoryID, 128) || c.CompositionID != comp.ID || c.Source.RepositoryID != c.RepositoryID || c.Source.Plan.OperationID != op.ID || c.Source.Result == nil || !c.Source.Result.Conflict || !conflictRepositoryAddress(v.Repository) || !safeBranch(v.BaseRef) || len(v.BaseRef) > 200 || !safeBranch(v.TargetBranch) || len(v.TargetBranch) > 200 || !fullSHA.MatchString(v.BaseSHA) || !domain.ValidStatus("composition_conflict", c.Status) || len(c.Source.Plan.HeadSHAs) == 0 || len(c.Source.Plan.HeadSHAs) > 20 || len(c.Source.Retained) > 20 || len(c.Integrations) > 20 || len(c.Features) > 20 || len(c.Gates) > 128 || len(c.Checks) > 128 {
		return v, false
	}
	if (c.Status == "verification_required" || c.Status == "resolved") && !fullSHA.MatchString(v.ResolutionCommit) || v.ResolutionCommit != "" && !fullSHA.MatchString(v.ResolutionCommit) {
		return v, false
	}
	sources := 0
	for _, source := range op.Sources {
		if source.RepositoryID == c.RepositoryID {
			sources++
			if source.Result == nil || !source.Result.Conflict || !reflect.DeepEqual(source.Plan, c.Source.Plan) {
				return v, false
			}
		}
	}
	if sources != 1 {
		return v, false
	}
	var revisionIDs []string
	for _, component := range comp.Components {
		appMatches := 0
		for _, app := range comp.ApplicationSnapshots {
			if app.ID != component.ApplicationID {
				continue
			}
			appMatches++
			if app.ProductID != op.ProductID {
				return v, false
			}
			if app.RepositoryID == c.RepositoryID {
				if component.BaseRef != v.BaseRef || component.BaseCommit != v.BaseSHA || revisionIDs != nil && !slices.Equal(revisionIDs, component.RevisionIDs) {
					return v, false
				}
				revisionIDs = component.RevisionIDs
			}
		}
		if appMatches != 1 {
			return v, false
		}
	}
	if len(revisionIDs) != len(c.Source.Plan.HeadSHAs) {
		return v, false
	}
	selected := map[string]bool{}
	selectedFeatures := map[string]string{}
	seenRevisions := map[string]bool{}
	for i, rid := range revisionIDs {
		var revision *domain.IntegrationRevision
		for j := range comp.RevisionSnapshots {
			r := &comp.RevisionSnapshots[j]
			if r.ID == rid {
				if revision != nil {
					return v, false
				}
				revision = r
			}
		}
		if seenRevisions[rid] || revision == nil || revision.ProductID != op.ProductID || revision.RepositoryID != c.RepositoryID || revision.HeadCommit != c.Source.Plan.HeadSHAs[i] || !fullSHA.MatchString(revision.HeadCommit) || len(revision.HeadCommit) != len(v.BaseSHA) || !conflictLabel(rid, 128) || !conflictLabel(revision.FeatureID, 128) || !conflictLabel(revision.IntegrationID, 128) || !safeBranch(revision.Branch) || len(revision.Branch) > 200 {
			return v, false
		}
		seenRevisions[rid] = true
		matches := 0
		for _, in := range c.Integrations {
			if in.ID == revision.IntegrationID {
				matches++
				if in.ProductID != op.ProductID || in.FeatureID != revision.FeatureID {
					return v, false
				}
			}
		}
		featureFound := false
		for _, f := range c.Features {
			if f.ID == revision.FeatureID {
				if f.ProductID != op.ProductID {
					return v, false
				}
				featureFound = true
			}
		}
		if matches != 1 || !featureFound || selected[revision.IntegrationID] {
			return v, false
		}
		selected[revision.IntegrationID] = true
		selectedFeatures[revision.IntegrationID] = revision.FeatureID
		input := conflictAgentInput{RevisionID: rid, FeatureID: revision.FeatureID, IntegrationID: revision.IntegrationID, OriginBranch: revision.Branch, HeadSHA: revision.HeadCommit}
		// Optional retained refs require the exact frozen intent and observation.
		// Even an old matching observation is historical evidence, not a live check.
		if expected, err := expectedSourceRetention(op, c.Source); err == nil && c.Source.Retention != nil && reflect.DeepEqual(expected, c.Source.Retention) {
			for _, proof := range c.Source.Retained {
				if proof.RevisionID == rid && proof.IntentHash == expected.InputHash && proof.HeadCommit == revision.HeadCommit && proof.Branch == "rcp/revisions/"+rid && !proof.ObservedAt.IsZero() && !proof.ObservedAt.Before(op.CreatedAt) && !proof.ObservedAt.After(now) {
					at := proof.ObservedAt
					input.RetainedRef, input.ObservedAt = proof.Branch, &at
				}
			}
		}
		v.Inputs = append(v.Inputs, input)
	}
	if len(selected) != len(c.Integrations) {
		return v, false
	}
	seenChecks, seenGates := map[string]bool{}, map[string]bool{}
	for _, gate := range c.Gates {
		if !conflictLabel(gate.ID, 128) || gate.ProductID != op.ProductID || seenGates[gate.ID] || len(gate.IntegrationIDs) > 100 {
			return v, false
		}
		seenGates[gate.ID] = true
		affected := []string{}
		for _, iid := range gate.IntegrationIDs {
			if selected[iid] && !slices.Contains(affected, iid) {
				if gate.FeatureID != selectedFeatures[iid] {
					return v, false
				}
				affected = append(affected, iid)
			}
		}
		if len(affected) == 0 {
			return v, false
		}
		for _, check := range c.Checks {
			if check.GateID != gate.ID {
				continue
			}
			if !conflictLabel(check.ID, 128) || check.ProductID != op.ProductID || check.FeatureID != gate.FeatureID || seenChecks[check.ID] {
				return v, false
			}
			seenChecks[check.ID] = true
			v.RequiredChecks = append(v.RequiredChecks, conflictAgentCheck{CheckID: check.ID, GateID: gate.ID, IntegrationIDs: affected, Blocking: gate.Blocking})
		}
	}
	return v, len(seenChecks) == len(c.Checks)
}
