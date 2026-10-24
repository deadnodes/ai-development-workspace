package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"releasecontrol/internal/domain"
)

// ErrSelectionChanged is actionable: reread desired intent and explicitly reapply.
var ErrSelectionChanged = errors.New("environment selection changed; reread environment state and reapply the patch")

func selectionToken(env domain.Environment) domain.EnvironmentSelection {
	return domain.EnvironmentSelection{CompositionID: env.DesiredCompositionID, OperationID: env.DesiredOperationID, Generation: env.SelectionGeneration}
}
func advanceSelection(env *domain.Environment) error {
	if env.SelectionGeneration == math.MaxUint64 {
		return invalid("environment selection generation exhausted")
	}
	env.SelectionGeneration++
	return nil
}
func checkSelection(env *domain.Environment, expected *domain.EnvironmentSelection) error {
	if env == nil || expected == nil || selectionToken(*env) != *expected {
		return fmt.Errorf("%w: %w", ErrValidation, ErrSelectionChanged)
	}
	return nil
}
func featureUpdateInput(c domain.Command) (domain.FeatureUpdateRequest, string, error) {
	var input domain.FeatureUpdateRequest
	// This command addresses a Feature and environment, never another entity.
	// Reject irrelevant envelope refs rather than omit them from the receipt hash.
	if c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return input, "", invalid("update_environment_feature does not accept integration_id, gate_id, check_id or result_id")
	}
	if err := decode(c.Data, &input); err != nil {
		return input, "", err
	}
	if c.FeatureID == "" || input.EnvironmentID == "" || len(input.RevisionIDs) == 0 || len(input.RevisionIDs) > 1000 || len(input.Name) > 512 {
		return input, "", invalid("feature_id, environment_id and nonempty bounded revision_ids required")
	}
	if len(input.IdempotencyKey) < 1 || len(input.IdempotencyKey) > 128 || strings.TrimSpace(input.IdempotencyKey) != input.IdempotencyKey || strings.ContainsFunc(input.IdempotencyKey, func(r rune) bool { return r < 32 || r == 127 }) {
		return input, "", invalid("idempotency_key must be an opaque nonempty key of at most 128 bytes without control characters")
	}
	if input.ExpectedSelection == nil {
		return input, "", invalid("expected_selection with composition_id, operation_id and generation required")
	}
	raw, _ := json.Marshal(c.Data["expected_selection"])
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, key := range []string{"composition_id", "operation_id", "generation"} {
		if value, ok := fields[key]; !ok || string(value) == "null" {
			return input, "", invalid("expected_selection.%s required (empty/zero values must be explicit)", key)
		}
	}
	payload, _ := json.Marshal(struct {
		Product, Feature, Actor, ID string
		Input                       domain.FeatureUpdateRequest
	}{c.ProductID, c.FeatureID, c.Actor, c.ID, input})
	hash := sha256.Sum256(payload)
	return input, hex.EncodeToString(hash[:]), nil
}

// Receipt lookup precedes CAS/current configuration checks. Replaying accepted
// intent is a historical read and never reselects or grants provider authority.
func replayFeatureUpdate(st *domain.State, c domain.Command) (*domain.ExternalOperation, error) {
	input, hash, err := featureUpdateInput(c)
	if err != nil {
		return nil, err
	}
	for i := range st.Operations {
		op := &st.Operations[i]
		if op.FeatureUpdate != nil && op.RequestKey == input.IdempotencyKey {
			if op.RequestHash != hash || op.ProductID != c.ProductID || op.FeatureID != c.FeatureID || op.RequestedBy != c.Actor || op.EnvironmentID != input.EnvironmentID {
				return nil, invalid("idempotency_key already binds a different feature update payload, actor or scope")
			}
			return op, nil
		}
	}
	return nil, nil
}
func planFeatureUpdate(st *domain.State, c domain.Command, m domain.Meta) (*domain.Composition, error) {
	input, _, err := featureUpdateInput(c)
	if err != nil {
		return nil, err
	}
	f := feature(st, c.FeatureID)
	env := environmentByID(st, input.EnvironmentID)
	if f == nil || f.ProductID != c.ProductID || env == nil || env.ProductID != c.ProductID {
		return nil, invalid("feature and environment must belong to the same product")
	}
	if err := checkSelection(env, input.ExpectedSelection); err != nil {
		return nil, err
	}
	baseline := compositionByID(st, env.DesiredCompositionID)
	if baseline == nil || baseline.ProductID != c.ProductID || baseline.EnvironmentID != env.ID {
		return nil, invalid("current desired composition required; explicitly initialize or replan the environment first")
	}
	if baseline.EnvironmentSnapshot.Cluster != env.Cluster || baseline.EnvironmentSnapshot.Namespace != env.Namespace {
		return nil, invalid("environment target changed; explicitly replan the baseline")
	}
	if err := selectionAllowed(st, env.ID); err != nil {
		return nil, err
	}
	repoIDs := map[string]bool{}
	for _, app := range baseline.ApplicationSnapshots {
		repoIDs[app.RepositoryID] = true
	}
	patches := map[string]*domain.IntegrationRevision{}
	for _, rid := range input.RevisionIDs {
		rev := revisionByID(st, rid)
		if rev == nil || rev.ProductID != c.ProductID {
			return nil, invalid("patch revision must belong to product")
		}
		in := integration(st, rev.IntegrationID)
		if in == nil || in.ProductID != c.ProductID || in.FeatureID != c.FeatureID {
			return nil, invalid("all patch revisions must belong to target feature")
		}
		if in.Status == "released" {
			return nil, invalid("released target work requires explicit baseline replan")
		}
		if !repoIDs[rev.RepositoryID] {
			return nil, invalid("new repository/application cohort requires explicit replan")
		}
		key := rev.IntegrationID + "/" + rev.RepositoryID
		if patches[key] != nil {
			return nil, invalid("patch revisions must have unique integration/repository pairs")
		}
		patches[key] = rev
	}
	components := make([]domain.CompositionComponent, len(baseline.Components))
	changed := false
	for n, prior := range baseline.Components {
		app := applicationByID(st, prior.ApplicationID)
		if app == nil || app.ProductID != c.ProductID || !repoIDs[app.RepositoryID] {
			return nil, invalid("selected application cohort changed; explicitly replan")
		}
		target := environmentBinding(st, env.ID, app.ID)
		if target == nil || !target.AllowDeploy || (target.Purpose != "DEV" && target.Purpose != "TEST") || target.GitOpsMode != "PR" {
			return nil, invalid("feature patch requires configured enabled DEV/TEST reviewed PR-only targets for every component")
		}
		component := prior
		component.RevisionIDs = append([]string(nil), prior.RevisionIDs...)
		component.TargetBranch = "generated/" + env.ID + "/" + m.ID + "/" + app.RepositoryID
		included := map[string]bool{}
		for j, rid := range prior.RevisionIDs {
			rev := revisionByID(st, rid)
			if rev == nil {
				return nil, invalid("baseline revision missing; explicitly replan")
			}
			key := rev.IntegrationID + "/" + rev.RepositoryID
			included[key] = true
			if replacement := patches[key]; replacement != nil {
				component.RevisionIDs[j] = replacement.ID
				changed = changed || replacement.ID != rid
			}
		}
		for _, rid := range input.RevisionIDs {
			rev := revisionByID(st, rid)
			key := rev.IntegrationID + "/" + rev.RepositoryID
			if rev.RepositoryID == app.RepositoryID && !included[key] {
				component.RevisionIDs = append(component.RevisionIDs, rid)
				included[key] = true
				changed = true
			}
		}
		components[n] = component
	}
	if !changed {
		return nil, invalid("requested revisions already selected; inspect current operation %s or explicitly reconcile composition %s", env.DesiredOperationID, baseline.ID)
	}
	name := input.Name
	if strings.TrimSpace(name) == "" {
		name = baseline.Name + " (feature update)"
	}
	result, _, err := applyComposition(st, domain.Command{Action: "plan_composition", Actor: c.Actor, ProductID: c.ProductID, Data: map[string]any{"environment_id": env.ID, "name": name, "components": components, "auto_refresh": false}}, m)
	if err != nil {
		return nil, err
	}
	comp := result.(domain.Composition)
	return &comp, nil
}
