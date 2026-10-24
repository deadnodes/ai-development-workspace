package application

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

// Only the private scheduler call can carry this origin. Actors/public fields
// never grant automatic retention authority.
type composeRefreshOrigin struct {
	Selection domain.EnvironmentSelection
	ParentID  string
}

func cloneFlowOperation(op domain.ExternalOperation) domain.ExternalOperation {
	b, _ := json.Marshal(op)
	var copy domain.ExternalOperation
	_ = json.Unmarshal(b, &copy)
	return copy
}
func expectedSourceRetention(op domain.ExternalOperation, source domain.FlowSource) (*domain.SourceRetentionIntent, error) {
	if op.Kind != "COMPOSE" || op.SourceRetentionOriginID == "" || op.CompositionSnapshot == nil || source.Plan.OperationID != op.ID || source.RepositoryID == "" || !safeBranch(source.Plan.TargetBranch) || len(source.Plan.TargetBranch) > 192 || !safeBranch(source.Plan.BaseRef) || len(source.Plan.BaseRef) > 200 || !fullSHA.MatchString(source.Plan.BaseSHA) || len(source.Plan.HeadSHAs) > 20 {
		return nil, invalid("source retention intent missing or malformed; request a new explicit DEV/TEST PR composition")
	}
	intent := &domain.SourceRetentionIntent{ProductID: op.ProductID, OperationID: op.ID, EnvironmentID: op.EnvironmentID, RepositoryID: source.RepositoryID, OriginOperationID: op.SourceRetentionOriginID, Revisions: []domain.SourceRevisionRetention{}}
	// A source repository has one canonical ordered component selection.
	var ids []string
	found := false
	for _, component := range op.CompositionSnapshot.Components {
		var app *domain.Application
		for i := range op.CompositionSnapshot.ApplicationSnapshots {
			a := &op.CompositionSnapshot.ApplicationSnapshots[i]
			if a.ID == component.ApplicationID {
				app = a
			}
		}
		if app != nil && app.RepositoryID == source.RepositoryID {
			if component.BaseRef != source.Plan.BaseRef || component.BaseCommit != source.Plan.BaseSHA || (found && !slices.Equal(ids, component.RevisionIDs)) {
				return nil, invalid("retention source plan does not match frozen composition")
			}
			ids = component.RevisionIDs
			found = true
		}
	}
	if !found || len(ids) != len(source.Plan.HeadSHAs) {
		return nil, invalid("retention source selection missing or mismatched")
	}
	heads := map[string]bool{}
	for i, rid := range ids {
		var revision *domain.IntegrationRevision
		for j := range op.CompositionSnapshot.RevisionSnapshots {
			r := &op.CompositionSnapshot.RevisionSnapshots[j]
			if r.ID == rid {
				revision = r
			}
		}
		branch := "rcp/revisions/" + rid
		if revision == nil || revision.ProductID != op.ProductID || revision.RepositoryID != source.RepositoryID || revision.HeadCommit != source.Plan.HeadSHAs[i] || !fullSHA.MatchString(revision.HeadCommit) || len(revision.HeadCommit) != len(source.Plan.BaseSHA) || heads[revision.HeadCommit] || len(branch) > 200 || !safeBranch(branch) {
			return nil, invalid("invalid bounded retained revision identity or multiple integrations claim same source head")
		}
		for _, segment := range strings.Split(branch, "/") {
			if strings.HasSuffix(segment, ".") {
				return nil, invalid("invalid derived retained branch segment")
			}
		}
		heads[revision.HeadCommit] = true
		intent.Revisions = append(intent.Revisions, domain.SourceRevisionRetention{RevisionID: rid, Branch: branch, HeadCommit: revision.HeadCommit})
	}
	// Bind full plan, connection and frozen source/build/target configuration. Hash
	// is an integrity binding of server-derived data, not a signed Git protection.
	payload, err := json.Marshal(struct {
		Intent     *domain.SourceRetentionIntent
		Plan       delivery.SourcePlan
		Connection delivery.Connection
		Prepared   []domain.DeliverySnapshot
		ParentID   string
	}{intent, source.Plan, source.Connection, op.PreparedDeployments, op.SourceRetentionParentID})
	if err != nil {
		return nil, invalid("cannot freeze source retention intent")
	}
	intent.InputHash = backupHash(payload)
	return intent, nil
}
func validSourceRetention(op domain.ExternalOperation) error {
	if op.Kind != "COMPOSE" || len(op.Sources) == 0 || len(op.Sources) > 20 || len(op.PreparedDeployments) == 0 {
		return invalid("explicit source retention intent required; request a new DEV/TEST PR composition")
	}
	for _, snap := range op.PreparedDeployments {
		if !snap.Target.AllowDeploy || snap.Target.GitOpsMode != "PR" || (snap.Target.Purpose != "DEV" && snap.Target.Purpose != "TEST") {
			return invalid("retained compositions require enabled DEV/TEST PR-only targets")
		}
	}
	total := 0
	seenRepos := map[string]bool{}
	for _, source := range op.Sources {
		if seenRepos[source.RepositoryID] {
			return invalid("duplicate retained repository")
		}
		seenRepos[source.RepositoryID] = true
		expected, err := expectedSourceRetention(op, source)
		if err != nil {
			return err
		}
		if source.Retention == nil || !reflect.DeepEqual(*source.Retention, *expected) {
			return invalid("source retention intent scope/hash changed; request a new explicit composition")
		}
		total += len(expected.Revisions)
		if total > 100 || len(source.Retained) > len(expected.Revisions) {
			return invalid("retention progress exceeds bounded frozen selection")
		}
		for i, proof := range source.Retained {
			revision := expected.Revisions[i]
			if proof.IntentHash != expected.InputHash || proof.RevisionID != revision.RevisionID || proof.Branch != revision.Branch || proof.HeadCommit != revision.HeadCommit || proof.ObservedAt.IsZero() || proof.ObservedAt.Before(op.CreatedAt) || proof.ObservedAt.After(time.Now().UTC().Add(time.Second)) {
				return invalid("retained source observation does not bind exact frozen intent")
			}
		}
	}
	return nil
}
func sameRetentionIntegrationScope(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// sourceRetentionProvenance uses durable append order, not clocks from possibly
// different installations. Every ancestor including the explicit root must have
// exactly one scoped creation event after the last restore event. Build indexes
// once so the lineage walk is bounded by current state without repeated scans.
func sourceRetentionProvenance(st *domain.State, start domain.ExternalOperation) error {
	type creation struct {
		index, count int
		scoped       bool
	}
	creations := make(map[string]creation, len(st.Operations))
	operations := make(map[string]*domain.ExternalOperation, len(st.Operations))
	duplicateOps := map[string]bool{}
	for i := range st.Operations {
		op := &st.Operations[i]
		if operations[op.ID] != nil {
			duplicateOps[op.ID] = true
		}
		operations[op.ID] = op
	}
	lastRestore := -1
	for index, event := range st.Events {
		if event.Action == "restore_backup" {
			lastRestore = index
		}
		if event.Action != "assemble_environment" && event.Action != "reconcile_composition" && event.Action != "update_environment_feature" {
			continue
		}
		op := operations[event.EntityID]
		if op == nil {
			continue
		}
		entry := creations[op.ID]
		entry.count++
		entry.index = index
		scoped := event.ProductID == op.ProductID && event.Data.Action == event.Action && (event.Data.ProductID == "" || event.Data.ProductID == op.ProductID)
		if event.Action == "reconcile_composition" {
			scoped = scoped && op.CompositionSnapshot != nil && event.Data.ID == op.CompositionSnapshot.ID
		} else {
			environment, _ := event.Data.Data["environment_id"].(string)
			scoped = scoped && environment == op.EnvironmentID
		}
		entry.scoped = scoped
		creations[op.ID] = entry
	}
	seen := make(map[string]bool)
	current := operations[start.ID]
	childIndex := len(st.Events)
	for current != nil {
		if seen[current.ID] || duplicateOps[current.ID] || len(seen) >= len(operations) {
			return invalid("cyclic or ambiguous source retention lineage")
		}
		seen[current.ID] = true
		if current.ProductID != start.ProductID || current.EnvironmentID != start.EnvironmentID || current.SourceRetentionOriginID != start.SourceRetentionOriginID || current.Status == "CANCELLED" || validSourceRetention(*current) != nil || current.CompositionSnapshot.ProductID != current.ProductID || current.CompositionSnapshot.EnvironmentID != current.EnvironmentID {
			return invalid("source retention lineage scope or intent unavailable")
		}
		event := creations[current.ID]
		if event.count != 1 || !event.scoped || event.index <= lastRestore || event.index >= childIndex {
			return invalid("source retention creation provenance missing, ambiguous or predates restore; request new explicit work")
		}
		childIndex = event.index
		if current.SourceRetentionParentID == "" {
			if current.ID != start.SourceRetentionOriginID {
				return invalid("source retention origin is not an explicit root")
			}
			return nil
		}
		current = operations[current.SourceRetentionParentID]
	}
	return invalid("source retention ancestor missing")
}
func sourceRetentionRefreshAllowed(st *domain.State, op domain.ExternalOperation) bool {
	return sourceRetentionProvenance(st, op) == nil
}

func freezeSourceRetention(st *domain.State, op *domain.ExternalOperation, origin *composeRefreshOrigin) error {
	op.SourceRetentionOriginID = op.ID
	if origin != nil {
		env := environmentByID(st, op.EnvironmentID)
		// applyFlow has selected the new operation within this transaction. Compare
		// against its frozen preselection environment snapshot instead.
		if env == nil || op.CompositionSnapshot == nil || selectionToken(op.CompositionSnapshot.EnvironmentSnapshot) != origin.Selection || origin.ParentID != origin.Selection.OperationID {
			return invalid("automatic composition selection changed")
		}
		parent := operationByID(st, origin.ParentID)
		if parent == nil || parent.ProductID != op.ProductID || parent.EnvironmentID != op.EnvironmentID || !sourceRetentionRefreshAllowed(st, *parent) || !sameRetentionIntegrationScope(selectedIntegrations(*parent.CompositionSnapshot), op.IntegrationIDs) {
			return invalid("automatic source refresh requires current explicit parent retention intent")
		}
		op.SourceRetentionOriginID = parent.SourceRetentionOriginID
		op.SourceRetentionParentID = parent.ID
	}
	for i := range op.Sources {
		intent, err := expectedSourceRetention(*op, op.Sources[i])
		if err != nil {
			return err
		}
		op.Sources[i].Retention = intent
	}
	return validSourceRetention(*op)
}
func (s *Service) retentionLease(st *domain.State, checked domain.ExternalOperation, token string) error {
	current := operationByID(st, checked.ID)
	if current == nil || current.LeaseOwner != token || !current.LeaseUntil.After(time.Now().UTC()) {
		return invalid("composition lease expired or lost")
	}
	if err := sourceRetentionProvenance(st, *current); err != nil {
		return err
	}
	if err := s.flowGuard(st, *current); err != nil {
		return err
	}
	if err := validSourceRetention(*current); err != nil {
		return err
	}
	if !reflect.DeepEqual(current.Sources, checked.Sources) || !reflect.DeepEqual(current.CompositionSnapshot, checked.CompositionSnapshot) || !reflect.DeepEqual(current.PreparedDeployments, checked.PreparedDeployments) || current.SourceRetentionOriginID != checked.SourceRetentionOriginID || current.SourceRetentionParentID != checked.SourceRetentionParentID || current.Phase != checked.Phase {
		return invalid("composition source snapshot changed after verification")
	}
	return nil
}
func (s *Service) checkRetentionLease(ctx context.Context, op domain.ExternalOperation, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	return s.retentionLease(&st, op, token)
}
func (s *Service) admitSourceProgress(ctx context.Context, before, after domain.ExternalOperation, token, phase, status, detail string, evidence map[string]string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.retentionLease(st, before, token); err != nil {
			return err
		}
		if err := validSourceRetention(after); err != nil {
			return err
		}
		current := operationByID(st, before.ID)
		now := time.Now().UTC()
		current.ReservationMutationIntent = ""
		current.Sources = after.Sources
		current.Phase = phase
		current.CurrentStep = phase
		current.DeploymentState = status
		current.Status = "RUNNING"
		current.Detail = detail
		current.UpdatedAt = now
		current.NextAttemptAt = now.Add(2 * time.Second)
		current.LeaseOwner = ""
		current.LeaseUntil = time.Time{}
		st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: before.ProductID, Actor: "worker", CreatedAt: now, UpdatedAt: now}, OperationID: before.ID, Phase: phase, Status: "SUCCEEDED", Detail: detail, Evidence: evidence})
		return nil
	})
}

// Conflict observations are new post-provider evidence too. Admit the conflict,
// its audit and terminal parent outcome together against the untouched input.
func (s *Service) admitSourceConflict(ctx context.Context, before domain.ExternalOperation, index int, result delivery.SourceResult, token string, evidence map[string]string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.retentionLease(st, before, token); err != nil {
			return err
		}
		if index < 0 || index >= len(before.Sources) || !result.Conflict {
			return invalid("invalid composition conflict observation")
		}
		after := cloneFlowOperation(before)
		after.Sources[index].Result = &result
		if err := validSourceRetention(after); err != nil {
			return err
		}
		now := time.Now().UTC()
		appendCompositionConflict(st, after, after.Sources[index], now)
		current := operationByID(st, before.ID)
		current.ReservationMutationIntent = ""
		current.Sources = after.Sources
		current.Status = "FAILED"
		current.DeploymentState = "BLOCKED"
		current.CurrentStep = current.Phase
		detail := "source conflict: " + result.Detail
		if len(detail) > 1000 {
			detail = detail[:1000]
		}
		current.Detail = detail
		current.Error = detail
		current.UpdatedAt = now
		current.FinishedAt = &now
		current.NextAttemptAt = now.Add(2 * time.Second)
		current.LeaseOwner = ""
		current.LeaseUntil = time.Time{}
		st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: before.ProductID, FeatureID: before.FeatureID, Actor: "worker", CreatedAt: now, UpdatedAt: now}, OperationID: before.ID, Phase: before.Phase, Status: "FAILED", Detail: detail, Evidence: evidence})
		return nil
	})
}

func (s *Service) retainedRef(ctx context.Context, source domain.FlowSource, revision domain.SourceRevisionRetention) error {
	observer, ok := s.provider.(delivery.ScopedGitObserver)
	if !ok {
		return invalid("source provider lacks scoped retained-ref observation")
	}
	observed, err := observer.ObserveBranch(ctx, source.Connection, source.Plan.Repository, revision.Branch)
	if err != nil {
		return fmt.Errorf("retained revision %s unavailable: %w", revision.RevisionID, err)
	}
	if observed.Name != revision.Branch || observed.SHA != revision.HeadCommit {
		return invalid("retained revision %s missing or moved; request fresh explicit source intent", revision.RevisionID)
	}
	return ctx.Err()
}

// One absent pin/readback/admission per durable tick, under the existing 25s
// context. Previously admitted refs are only read; missing refs never autoheal.
func (s *Service) retainFlowSources(ctx context.Context, op domain.ExternalOperation, token string) (bool, error) {
	if _, ok := s.provider.(delivery.SourceProvider); !ok {
		return false, invalid("source provider lacks composition execution")
	}
	ref, ok := s.provider.(delivery.RefProvider)
	if !ok {
		return false, invalid("source provider lacks retained-ref creation")
	}
	if _, ok := s.provider.(delivery.ScopedGitObserver); !ok {
		return false, invalid("source provider lacks scoped retained-ref observation")
	}
	if _, ok := s.provider.(delivery.ComposedSourceVerifier); !ok {
		return false, invalid("source provider lacks read-only composition verification")
	}
	if err := validSourceRetention(op); err != nil {
		return false, err
	}
	for i, source := range op.Sources {
		for j, revision := range source.Retention.Revisions {
			if j < len(source.Retained) {
				if err := s.retainedRef(ctx, source, revision); err != nil {
					return false, err
				}
				continue
			}
			if err := s.checkRetentionLease(ctx, op, token); err != nil {
				return false, err
			}
			recovering, e := s.reservationMutationRecovery(ctx, op, token)
			if e != nil {
				return false, e
			}
			var sha string
			var err error
			if recovering {
				observed, readErr := s.provider.(delivery.ScopedGitObserver).ObserveBranch(ctx, source.Connection, source.Plan.Repository, revision.Branch)
				if readErr != nil || observed.Name != revision.Branch || observed.SHA != revision.HeadCommit {
					return false, invalid("retained source mutation outcome unresolved; no write repeated")
				}
				sha = revision.HeadCommit
			} else {
				if e := s.beginReservationMutation(ctx, op, token, "SOURCE"); e != nil {
					return false, e
				}
				sha, err = ref.EnsureRef(ctx, source.Connection, source.Plan.Repository, revision.Branch, revision.HeadCommit)
			}
			if err != nil {
				observer := s.provider.(delivery.ScopedGitObserver)
				observed, readErr := observer.ObserveBranch(ctx, source.Connection, source.Plan.Repository, revision.Branch)
				if readErr != nil {
					return true, s.retry(ctx, op, token, "retained source create response uncertain; next tick observes exact ref: "+err.Error())
				}
				if observed.Name != revision.Branch || observed.SHA != revision.HeadCommit {
					return false, invalid("initial retained revision ref already points elsewhere; never overwrite")
				}
				sha = revision.HeadCommit
			}
			if sha != revision.HeadCommit {
				return false, invalid("provider returned wrong retained source identity")
			}
			if err = s.retainedRef(ctx, source, revision); err != nil {
				return false, err
			}
			after := cloneFlowOperation(op)
			after.Sources[i].Retained = append(after.Sources[i].Retained, domain.SourceRetentionObservation{IntentHash: source.Retention.InputHash, RevisionID: revision.RevisionID, Branch: revision.Branch, HeadCommit: revision.HeadCommit, ObservedAt: time.Now().UTC()})
			return true, s.admitSourceProgress(ctx, op, after, token, op.Phase, "COMPOSING", "exact retained revision observed", map[string]string{"repository_id": source.RepositoryID, "revision_id": revision.RevisionID, "branch": revision.Branch, "source_sha": sha, "intent_hash": source.Retention.InputHash})
		}
	}
	return false, nil
}
func (s *Service) verifyRetainedSource(ctx context.Context, op domain.ExternalOperation, source domain.FlowSource) error {
	if err := validSourceRetention(op); err != nil {
		return err
	}
	if len(source.Retained) != len(source.Retention.Revisions) {
		return invalid("retained revision observations incomplete")
	}
	for _, revision := range source.Retention.Revisions {
		if err := s.retainedRef(ctx, source, revision); err != nil {
			return err
		}
	}
	if err := validateFlowSource(ctx, s.provider, op, source); err != nil {
		return err
	}
	verifier, ok := s.provider.(delivery.ComposedSourceVerifier)
	if !ok {
		return invalid("source provider lacks read-only composition verifier")
	}
	if source.Result == nil {
		return invalid("missing generated source result")
	}
	if err := verifier.VerifyComposedSource(ctx, source.Connection, source.Plan, *source.Result); err != nil {
		return fmt.Errorf("generated source drift: %w", err)
	}
	return ctx.Err()
}
