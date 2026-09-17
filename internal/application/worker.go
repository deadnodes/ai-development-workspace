package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

func (s *Service) RunWorker(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func operationByID(st *domain.State, id string) *domain.ExternalOperation {
	for i := range st.Operations {
		if st.Operations[i].ID == id {
			return &st.Operations[i]
		}
	}
	return nil
}

// Tick claims one durable operation step. Network I/O happens outside the store
// transaction. A persisted dispatch intent is never blindly dispatched again.
func (s *Service) Tick(ctx context.Context) (bool, error) {
	if s.provider == nil && len(s.runtimeObservers) == 0 {
		return false, nil
	}
	now := time.Now().UTC()
	var op *domain.ExternalOperation
	token := id()
	err := s.store.Update(ctx, func(st *domain.State) error {
		var candidate *domain.ExternalOperation
		for i := range st.Operations {
			v := &st.Operations[i]
			if s.provider == nil && v.Kind != "REFRESH_RUNTIME" {
				continue
			}
			if terminalOperation(v.Status) || v.NextAttemptAt.After(now) || v.LeaseUntil.After(now) {
				continue
			}
			if candidate == nil || v.NextAttemptAt.Before(candidate.NextAttemptAt) || (v.NextAttemptAt.Equal(candidate.NextAttemptAt) && v.ID < candidate.ID) {
				candidate = v
			}
		}
		if candidate != nil {
			v := candidate
			v.LeaseOwner = token
			v.LeaseUntil = now.Add(45 * time.Second)
			if v.Kind == "REFRESH_RUNTIME" {
				v.LeaseUntil = now.Add(10 * time.Minute)
			}
			v.Attempts++
			if v.StartedAt == nil {
				started := now
				v.StartedAt = &started
			}
			v.Status = "RUNNING"
			copy := *v
			op = &copy
		}

		return nil
	})
	if err != nil || op == nil {
		return false, err
	}
	timeout := 25 * time.Second
	if op.Kind == "REFRESH_RUNTIME" {
		timeout = 8 * time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if op.Kind == "REFRESH_RUNTIME" {
		return true, s.tickRuntime(callCtx, *op, token)
	}
	if op.Kind == "CREATE_BRANCH" || op.Kind == "PIN_REVISION" {
		return true, s.tickManagedBranch(callCtx, *op, token)
	}
	if op.Kind == "REFRESH_GIT" {
		return true, s.observeGit(callCtx, *op, token)
	}
	if op.Kind == "REFRESH_REPOSITORY_GIT" {
		return true, s.observeRepositoryGit(callCtx, *op, token)
	}
	if op.Kind == "COMPOSE" || op.Kind == "RELEASE_CANDIDATE" || op.Kind == "RELEASE_PROMOTION" {
		return true, s.tickFlow(callCtx, *op, token)
	}
	if op.Kind != "DEPLOY" || op.Snapshot == nil {
		return true, s.finish(ctx, *op, token, "FAILED", "invalid operation snapshot", nil)
	}
	snapshot := op.Snapshot
	if domain.ComponentKind(snapshot.Application) != "APPLICATION" {
		return true, s.finish(ctx, *op, token, "BLOCKED", "library components use publication, not environment deployment", nil)
	}
	if op.ParentID != "" {
		state, e := s.State(ctx)
		if e != nil {
			return true, e
		}
		if e = flowChildAllowed(&state, *op); e != nil {
			return true, s.finish(ctx, *op, token, "CANCELLED", e.Error(), nil)
		}
	}
	if op.BuildOnly && op.Phase == "APPLY" {
		return true, s.finish(ctx, *op, token, "ARTIFACT_READY", "main artifact ready; candidate verification required before any production deployment", nil)
	}
	switch op.Phase {
	case "WAIT_GITOPS_PR":
		return true, s.waitGitOpsPR(callCtx, ctx, *op, token)
	case "PROMOTION_PREFLIGHT":
		if e := s.verifyPromotionSource(callCtx, *op); e != nil {
			return true, s.finish(ctx, *op, token, "BLOCKED", e.Error(), nil)
		}
		base, e := s.provider.ReadGitOps(callCtx, snapshot.GitOpsConnection, snapshot.GitOpsRequest)
		if e != nil {
			return true, s.retry(ctx, *op, token, e.Error())
		}
		if base.HeadSHA == "" || base.BlobSHA == "" {
			return true, s.finish(ctx, *op, token, "BLOCKED", "GitOps evidence missing", nil)
		}
		op.GitOpsBase = &base
		// A private GHCR report expires in ten minutes. Run a fresh correlated probe
		// after human verification; keep the approved digest immutable even if CI
		// must reconstruct missing bytes under the configured rebuild policy.
		if snapshot.Build.RebuildMissing {
			snapshot.BuildRequest.OperationID = op.ID
			snapshot.BuildRequest.RequestedAt = time.Now().UTC()
			snapshot.BuildRequest.Inputs["operation_id"] = op.ID
			op.Run = nil
			return true, s.advance(ctx, *op, token, "DISPATCH", "WAITING_FOR_ARTIFACT", "refresh registry proof for approved main digest; different rebuild digest will block", nil)
		}
		return true, s.advance(ctx, *op, token, "INSPECT", "WAITING_FOR_ARTIFACT", "recheck exact verified main artifact before promotion", nil)
	case "PREFLIGHT":
		branches, e := s.provider.Branches(callCtx, snapshot.SourceConnection, snapshot.BuildRequest.Repository)
		if e != nil {
			return true, s.finish(ctx, *op, token, "FAILED", "source branch observation failed: "+e.Error(), nil)
		}
		var head, baseSHA, workflowSHA string
		for _, branch := range branches {
			if branch.Name == snapshot.Revision.Branch {
				head = branch.SHA
			}
			if branch.Name == configuredBase(snapshot.Source) {
				baseSHA = branch.SHA
			}
			if branch.Name == snapshot.BuildRequest.Ref {
				workflowSHA = branch.SHA
			}
		}
		if head == "" || baseSHA == "" || workflowSHA == "" {
			return true, s.finish(ctx, *op, token, "BLOCKED", "source, configured base or trusted workflow branch is missing", nil)
		}
		if snapshot.Revision.HeadCommit != "" && snapshot.Revision.HeadCommit != head {
			return true, s.finish(ctx, *op, token, "BLOCKED", "source branch moved; capture a new revision or deploy the currently bound branch", nil)
		}
		comparisonBase := baseSHA
		if op.ParentID != "" && !op.BuildOnly {
			comparisonBase = snapshot.Revision.BaseCommit
		}
		comparison, e := s.provider.Compare(callCtx, snapshot.SourceConnection, snapshot.BuildRequest.Repository, comparisonBase, head)
		if e != nil {
			return true, s.finish(ctx, *op, token, "FAILED", "source comparison failed: "+e.Error(), nil)
		}
		if comparison.Behind > 0 {
			return true, s.finish(ctx, *op, token, "BLOCKED", "source branch is behind configured base; update it explicitly before deployment", nil)
		}
		if snapshot.Revision.ID == "" {
			now := time.Now().UTC()
			snapshot.Revision.Meta = domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, Actor: op.RequestedBy, CreatedAt: now, UpdatedAt: now}
			snapshot.Revision.BaseCommit = baseSHA
			snapshot.Revision.HeadCommit = head
			snapshot.Revision.Commits = []string{}
			for _, commit := range comparison.Commits {
				snapshot.Revision.Commits = append(snapshot.Revision.Commits, commit.SHA)
			}
			if len(snapshot.Revision.Commits) == 0 {
				snapshot.Revision.Commits = []string{head}
			}
			if e := s.store.Update(ctx, func(st *domain.State) error {
				v := operationByID(st, op.ID)
				if v == nil || v.LeaseOwner != token {
					return invalid("lease lost")
				}
				st.IntegrationRevisions = append(st.IntegrationRevisions, snapshot.Revision)
				return nil
			}); e != nil {
				return true, e
			}
		}
		snapshot.BuildRequest.SourceSHA = head
		snapshot.BuildRequest.WorkflowSHA = workflowSHA
		snapshot.BuildRequest.ImageTag = "rcp-" + head
		snapshot.BuildRequest.Inputs["source_sha"] = head
		snapshot.BuildRequest.Inputs["image_tag"] = snapshot.BuildRequest.ImageTag
		base, e := s.provider.ReadGitOps(callCtx, snapshot.GitOpsConnection, snapshot.GitOpsRequest)
		if e != nil {
			return true, s.finish(ctx, *op, token, "FAILED", "GitOps preflight failed: "+e.Error(), nil)
		}
		if base.HeadSHA == "" || base.BlobSHA == "" {
			return true, s.finish(ctx, *op, token, "BLOCKED", "GitOps preflight did not provide pinned head/blob", nil)
		}
		op.GitOpsBase = &base
		if op.ExistingArtifact != nil {
			if op.ExistingArtifact.SourceCommit != head {
				return true, s.finish(ctx, *op, token, "BLOCKED", "selected artifact does not match observed source", nil)
			}
			return true, s.advance(ctx, *op, token, "INSPECT", "WAITING_FOR_ARTIFACT", "recheck selected immutable artifact; CI build is disabled for this operation", map[string]string{"artifact_id": op.ExistingArtifact.ID})
		}
		if op.ExpectedDigest == "" {
			state, readErr := s.State(ctx)
			if readErr != nil {
				return true, readErr
			}
			for n := len(state.DeliveryArtifacts) - 1; n >= 0; n-- {
				known := state.DeliveryArtifacts[n]
				if known.ProductID == op.ProductID && known.ApplicationID == op.ApplicationID && known.RepositoryID == snapshot.Revision.RepositoryID && known.SourceCommit == head && known.ImageRepository == snapshot.Build.ImageRepository && known.BuildRunID > 0 && validArtifactDigest(known.Digest) {
					op.ExpectedDigest = known.Digest
					break
				}
			}
		}
		artifact, e := s.provider.InspectArtifact(callCtx, snapshot.SourceConnection, delivery.ArtifactRequest{Repository: snapshot.Build.ImageRepository, Tag: snapshot.BuildRequest.ImageTag, ExpectedDigest: op.ExpectedDigest})
		knownProvenance := false
		if e == nil && artifact.Available {
			state, readErr := s.State(ctx)
			if readErr != nil {
				return true, readErr
			}
			for _, known := range state.DeliveryArtifacts {
				if known.ProductID == op.ProductID && known.ApplicationID == op.ApplicationID && known.RepositoryID == snapshot.Revision.RepositoryID && known.SourceCommit == head && known.ImageRepository == snapshot.Build.ImageRepository && known.Digest == artifact.Digest && known.BuildRunID > 0 && known.Availability == "PRESENT" {
					knownProvenance = true
				}
			}
		}
		if !op.BuildOnly && knownProvenance && e == nil && artifact.Available && validArtifactDigest(artifact.Digest) && !artifact.ObservedAt.IsZero() {
			if op.ExpectedDigest != "" && artifact.Digest != op.ExpectedDigest {
				return true, s.finish(ctx, *op, token, "BLOCKED", "observed artifact differs from requested digest; explicit new operation required", nil)
			}
			op.Artifact = &artifact
			return true, s.advance(ctx, *op, token, "APPLY", "ARTIFACT_READY", "existing source artifact observed; reuse without build", map[string]string{"digest": artifact.Digest})
		}
		// Private GHCR requires a fresh correlated Actions probe. The workflow reuses
		// the deterministic source tag when present, builds only when unavailable.
		if e == nil && !artifact.Available {
			if err := s.recordArtifactObservation(ctx, *op, "MISSING", artifact); err != nil {
				return true, err
			}
		}
		if !snapshot.Build.RebuildMissing {
			return true, s.finish(ctx, *op, token, "BLOCKED", "artifact missing or unverifiable and rebuild_missing policy is disabled", nil)
		}
		return true, s.advance(ctx, *op, token, "DISPATCH", "WAITING_FOR_ARTIFACT", "artifact absent or private; workflow will probe/reuse or build", nil)
	case "DISPATCH":
		// Commit this transition before calling dispatch. Crash/timeout recovery only
		// searches correlation; an uncertain POST must not launch another build.
		if e := s.save(ctx, *op, token, "BUILD_LOOKUP", "BUILDING", "dispatch intent committed", nil, false); e != nil {
			return true, e
		}
		dispatch, e := s.provider.Dispatch(callCtx, snapshot.SourceConnection, snapshot.BuildRequest)
		if dispatch.ProviderRunID > 0 {
			op.Run = &delivery.RunResult{Found: true, ID: dispatch.ProviderRunID, Status: "queued"}
		}
		detail := "workflow dispatch accepted; awaiting correlated run"
		if e != nil {
			detail = "dispatch outcome uncertain; discovering existing run only: " + e.Error()
		}
		return true, s.advance(ctx, *op, token, "BUILD_LOOKUP", "BUILDING", detail, nil)
	case "BUILD_LOOKUP":
		run, e := s.provider.FindRun(callCtx, snapshot.SourceConnection, snapshot.BuildRequest)
		if e != nil {
			return true, s.retry(ctx, *op, token, "build lookup failed: "+e.Error())
		}
		if !run.Found {
			if time.Since(snapshot.BuildRequest.RequestedAt) > 15*time.Minute {
				return true, s.finish(ctx, *op, token, "BLOCKED", "no correlated workflow run found; dispatch will not be repeated automatically", nil)
			}
			return true, s.retry(ctx, *op, token, "waiting for correlated workflow run")
		}
		op.Run = &run
		if run.HeadSHA != "" && run.HeadSHA != snapshot.BuildRequest.WorkflowSHA {
			return true, s.finish(ctx, *op, token, "BLOCKED", "workflow definition commit differs from pinned trusted ref", nil)
		}
		if run.Status != "completed" {
			return true, s.retry(ctx, *op, token, "workflow "+run.Status)
		}
		if run.Conclusion != "success" {
			return true, s.finish(ctx, *op, token, "FAILED", "workflow completed: "+run.Conclusion, map[string]string{"run_url": run.URL})
		}
		reportedDigest := run.Artifacts["digest"]
		if !validArtifactDigest(reportedDigest) || run.Artifacts["source_sha"] != snapshot.Revision.HeadCommit || run.Artifacts["image_repository"] != snapshot.Build.ImageRepository {
			return true, s.finish(ctx, *op, token, "BLOCKED", "build report lacks exact source, image repository or digest proof", nil)
		}
		if op.ExpectedDigest != "" && op.ExpectedDigest != reportedDigest {
			return true, s.finish(ctx, *op, token, "BLOCKED", "build produced a different digest; explicit replacement operation required", nil)
		}
		op.ExpectedDigest = reportedDigest
		return true, s.advance(ctx, *op, token, "INSPECT", "WAITING_FOR_ARTIFACT", "build succeeded; inspect immutable artifact", map[string]string{"run_url": run.URL})
	case "INSPECT":
		request := delivery.ArtifactRequest{Repository: snapshot.Build.ImageRepository, Tag: snapshot.BuildRequest.ImageTag, ExpectedDigest: op.ExpectedDigest}
		if op.Run != nil {
			request.EvidenceRepository = snapshot.BuildRequest.Repository
			request.EvidenceRunID = op.Run.ID
			request.EvidenceOperationID = snapshot.BuildRequest.OperationID
			request.SourceSHA = snapshot.Revision.HeadCommit
		}
		if known := op.ExistingArtifact; known != nil {
			request.Tag = known.Tag
			request.EvidenceRepository = known.BuildRequest.Repository
			request.EvidenceRunID = known.BuildRunID
			request.EvidenceOperationID = known.BuildRequest.OperationID
			request.SourceSHA = known.SourceCommit
		}
		artifact, e := s.provider.InspectArtifact(callCtx, snapshot.SourceConnection, request)
		if e != nil {
			if op.ExistingArtifact != nil {
				return true, s.finish(ctx, *op, token, "BLOCKED", "selected artifact unavailable or registry evidence expired; no build dispatched: "+e.Error(), nil)
			}
			return true, s.retry(ctx, *op, token, "artifact inspection failed: "+e.Error())
		}
		if !artifact.Available || artifact.Digest == "" {
			if err := s.recordArtifactObservation(ctx, *op, "MISSING", artifact); err != nil {
				return true, err
			}
			return true, s.finish(ctx, *op, token, "BLOCKED", "artifact is missing; rebuild requires a new operation with explicit intent", nil)
		}
		if !validArtifactDigest(artifact.Digest) || artifact.ObservedAt.IsZero() {
			return true, s.finish(ctx, *op, token, "BLOCKED", "artifact observation lacks exact digest or timestamp", nil)
		}
		if op.ExpectedDigest != "" && artifact.Digest != op.ExpectedDigest {
			return true, s.finish(ctx, *op, token, "BLOCKED", "artifact digest differs; a new approved operation is required", nil)
		}
		op.Artifact = &artifact
		return true, s.advance(ctx, *op, token, "APPLY", "ARTIFACT_READY", "artifact digest observed; GitOps update pending", map[string]string{"digest": artifact.Digest})
	case "APPLY", "APPLY_UNCERTAIN":
		if op.ParentID != "" {
			if e := s.verifyPromotionSource(callCtx, *op); e != nil {
				return true, s.finish(ctx, *op, token, "BLOCKED", e.Error(), nil)
			}
		}
		currentState, stateErr := s.State(ctx)
		if stateErr != nil {
			return true, stateErr
		}
		currentTarget := environmentBinding(&currentState, op.EnvironmentID, op.ApplicationID)
		currentEnv := environmentByID(&currentState, op.EnvironmentID)
		if currentTarget == nil || !reflect.DeepEqual(*currentTarget, snapshot.Target) || !currentTarget.AllowDeploy || currentEnv == nil || currentEnv.Cluster != snapshot.Environment.Cluster || currentEnv.Namespace != snapshot.Environment.Namespace {
			return true, s.finish(ctx, *op, token, "BLOCKED", "environment target or policy changed; create a new operation", nil)
		}
		if op.Artifact == nil || op.GitOpsBase == nil {
			return true, s.finish(ctx, *op, token, "FAILED", "missing pinned artifact/GitOps base", nil)
		}
		// Re-read permits safe recovery after a commit response was lost. Different
		// intervening content is blocked; stale branch heads are never overwritten.
		current, e := s.provider.ReadGitOps(callCtx, snapshot.GitOpsConnection, snapshot.GitOpsRequest)
		if e != nil {
			return true, s.retry(ctx, *op, token, "GitOps read failed: "+e.Error())
		}
		if current.Digest == op.Artifact.Digest && current.ImageRepository == snapshot.Build.ImageRepository {
			op.GitOpsResult = &delivery.GitOpsResult{CommitSHA: current.HeadSHA, BlobSHA: current.BlobSHA, AlreadyApplied: true}
			return true, s.finish(ctx, *op, token, "GITOPS_APPLIED", "PENDING_RECONCILIATION: desired digest is in GitOps; Flux and runtime are not observed", map[string]string{"commit": current.HeadSHA, "digest": op.Artifact.Digest})
		}
		if (op.ParentID == "" && current.HeadSHA != op.GitOpsBase.HeadSHA) || current.BlobSHA != op.GitOpsBase.BlobSHA {
			return true, s.finish(ctx, *op, token, "BLOCKED", "GitOps changed since planning; create a new operation", nil)
		}
		op.GitOpsBase.HeadSHA = current.HeadSHA
		if e := s.save(ctx, *op, token, "APPLY_UNCERTAIN", "GITOPS_PENDING", "GitOps CAS intent committed", nil, false); e != nil {
			if errors.Is(e, ErrValidation) {
				return true, s.finish(ctx, *op, token, "CANCELLED", e.Error(), nil)
			}
			return true, e
		}
		if snapshot.Target.GitOpsMode == "PR" {
			return true, s.proposeGitOpsPR(callCtx, ctx, *op, token)
		}
		applied, e := s.provider.ApplyGitOps(callCtx, snapshot.GitOpsConnection, delivery.GitOpsApply{Request: snapshot.GitOpsRequest, ExpectedHeadSHA: op.GitOpsBase.HeadSHA, ExpectedBlobSHA: op.GitOpsBase.BlobSHA, Digest: op.Artifact.Digest, OperationID: op.ID, Message: "release-control: " + op.ID})
		if e != nil {
			return true, s.advance(ctx, *op, token, "APPLY_UNCERTAIN", "GITOPS_PENDING", "GitOps outcome uncertain; reread before retry: "+e.Error(), nil)
		}
		if applied.CommitSHA == "" {
			return true, s.finish(ctx, *op, token, "BLOCKED", "GitOps provider returned no commit evidence", nil)
		}
		op.GitOpsResult = &applied
		return true, s.finish(ctx, *op, token, "GITOPS_APPLIED", "PENDING_RECONCILIATION: GitOps committed; Flux and runtime are not observed", map[string]string{"commit": applied.CommitSHA, "digest": op.Artifact.Digest})
	default:
		return true, s.finish(ctx, *op, token, "FAILED", "unknown operation phase", nil)
	}
}
func validArtifactDigest(d string) bool {
	if !strings.HasPrefix(d, "sha256:") || len(d) != 71 {
		return false
	}
	for _, r := range d[7:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func (s *Service) finish(ctx context.Context, op domain.ExternalOperation, token, status, detail string, evidence map[string]string) error {
	return s.save(ctx, op, token, op.Phase, status, detail, evidence, true)
}
func (s *Service) advance(ctx context.Context, op domain.ExternalOperation, token, phase, status, detail string, evidence map[string]string) error {
	op.Attempts = 0
	return s.save(ctx, op, token, phase, status, detail, evidence, true)
}
func (s *Service) retry(ctx context.Context, op domain.ExternalOperation, token, detail string) error {
	if time.Since(op.CreatedAt) > 45*time.Minute {
		return s.finish(ctx, op, token, "BLOCKED", "operation deadline exceeded: "+detail, nil)
	}
	return s.save(ctx, op, token, op.Phase, op.DeploymentState, detail, nil, true)
}
func (s *Service) save(ctx context.Context, op domain.ExternalOperation, token, phase, status, detail string, evidence map[string]string, release bool) error {
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	return s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token {
			return fmt.Errorf("operation lease lost")
		}
		if phase == "APPLY_UNCERTAIN" && op.ParentID != "" && status != "CANCELLED" && status != "BLOCKED" {
			if err := flowChildAllowed(st, op); err != nil {
				return err
			}
		}
		if (op.Kind == "COMPOSE" || op.Kind == "RELEASE_CANDIDATE" || op.Kind == "RELEASE_PROMOTION") && status != "FAILED" && status != "BLOCKED" && status != "CANCELLED" {
			if e := flowGuard(st, op); e != nil {
				status = "BLOCKED"
				detail = e.Error()
			}
		}
		now := time.Now().UTC()
		op.Phase = phase
		op.DeploymentState = status
		switch status {
		case "GITOPS_APPLIED", "SUCCEEDED", "READY_FOR_VERIFICATION", "DEPLOYED":
			op.Status = "SUCCEEDED"
		case "ARTIFACT_READY":
			op.Status = "RUNNING"
			if op.BuildOnly && phase == "APPLY" && release {
				op.Status = "SUCCEEDED"
			}
		case "FAILED", "BLOCKED":
			op.Status = "FAILED"
			op.Error = detail
		case "CANCELLED":
			op.Status = "CANCELLED"
		default:
			op.Status = "RUNNING"
		}
		op.CurrentStep = phase
		op.Detail = detail
		op.UpdatedAt = now
		op.NextAttemptAt = now.Add(2 * time.Second)
		if phase == "BUILD_LOOKUP" || phase == "WAIT_CHILDREN" || phase == "WAIT_RUNTIME" || phase == "WAIT_GITOPS_PR" {
			op.NextAttemptAt = now.Add(10 * time.Second)
		}
		if release {
			op.LeaseOwner = ""
			op.LeaseUntil = time.Time{}
		} else {
			op.LeaseOwner = token
			op.LeaseUntil = current.LeaseUntil
		}
		if terminalOperation(op.Status) {
			op.FinishedAt = &now
		}
		*current = op
		if op.Run != nil {
			bm := domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, Actor: "worker", CreatedAt: now, UpdatedAt: now}
			st.DeliveryBuildRuns = append(st.DeliveryBuildRuns, domain.DeliveryBuildRun{Meta: bm, OperationID: op.ID, ApplicationID: op.ApplicationID, Request: op.Snapshot.BuildRequest, Result: *op.Run})
		}
		if op.Artifact != nil && op.Snapshot != nil {
			am := domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, Actor: "worker", CreatedAt: now, UpdatedAt: now}
			runID := int64(0)
			if op.Run != nil {
				runID = op.Run.ID
			}
			buildRequest := op.Snapshot.BuildRequest
			if known := op.ExistingArtifact; known != nil {
				buildRequest = known.BuildRequest
				runID = known.BuildRunID
			}
			st.DeliveryArtifacts = append(st.DeliveryArtifacts, domain.DeliveryArtifact{Meta: am, OperationID: op.ID, ApplicationID: op.ApplicationID, RepositoryID: op.Snapshot.Revision.RepositoryID, SourceCommit: op.Snapshot.Revision.HeadCommit, ImageRepository: op.Snapshot.Build.ImageRepository, Tag: buildRequest.ImageTag, Digest: op.Artifact.Digest, Availability: "PRESENT", ObservedAt: op.Artifact.ObservedAt, BuildRequest: buildRequest, BuildRunID: runID})
		}
		stepMeta := domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, Actor: "worker", CreatedAt: now, UpdatedAt: now}
		st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: stepMeta, OperationID: op.ID, Phase: phase, Status: op.Status, Detail: detail, Evidence: evidence})
		return nil
	})
}

func (s *Service) recordArtifactObservation(ctx context.Context, op domain.ExternalOperation, availability string, a delivery.ArtifactResult) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		v := operationByID(st, op.ID)
		if v == nil || v.LeaseOwner != op.LeaseOwner {
			return invalid("lease lost")
		}
		now := time.Now().UTC()
		st.DeliveryArtifacts = append(st.DeliveryArtifacts, domain.DeliveryArtifact{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: op.FeatureID, Actor: "worker", CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, ApplicationID: op.ApplicationID, RepositoryID: op.Snapshot.Revision.RepositoryID, SourceCommit: op.Snapshot.Revision.HeadCommit, ImageRepository: op.Snapshot.Build.ImageRepository, Tag: op.Snapshot.BuildRequest.ImageTag, Digest: op.ExpectedDigest, Availability: availability, ObservedAt: a.ObservedAt, BuildRequest: op.Snapshot.BuildRequest})
		return nil
	})
}
