package application

import (
	"context"
	"reflect"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

func (s *Service) nativeRecipes(product string, app domain.Application, profile, image string, binding *domain.NativeBinding) (workerconfig.Recipe, workerconfig.PublicationRecipe, error) {
	var b workerconfig.Recipe
	var p workerconfig.PublicationRecipe
	if s.externalWorkers == nil || binding == nil {
		return b, p, invalid("automatic native binding required")
	}
	b, bok := s.externalWorkers.Recipe(binding.BuildRecipeID)
	p, pok := s.externalWorkers.Publication(binding.PublicationRecipeID)
	if !bok || !pok || b.ProductID != product || p.ProductID != product || b.RepositoryID != app.RepositoryID || p.RepositoryID != app.RepositoryID || p.ComponentID != app.ID || b.Capability != "local_oci_build" || b.Hash != p.BuildRecipeHash || p.Profile != profile || nativeImage(p) != image || workerconfig.ValidateParameters(b, binding.Parameters) != nil {
		return b, p, invalid("automatic native recipe identity mismatch")
	}
	return b, p, nil
}
func (s *Service) freezeNative(product string, app domain.Application, build domain.ComponentBuild, now time.Time) (*domain.NativeDelivery, error) {
	b, p, e := s.nativeRecipes(product, app, build.Profile, build.ImageRepository, build.Native)
	if e != nil {
		return nil, e
	}
	return &domain.NativeDelivery{ConfigRevision: s.externalWorkers.Revision(), BuildRecipeHash: b.Hash, BuildRecipeRevision: b.Revision, PublicationRecipeHash: p.Hash, PublicationRecipeRevision: p.Revision, Deadline: now.Add(time.Hour)}, nil
}

// nativeDeliveryAllowed is the live fence shared by all managed-job mutations.
// Historical artifacts use their immutable provenance rather than this live fence.
func (s *Service) nativeDeliveryAllowed(st *domain.State, op *domain.ExternalOperation) error {
	if op == nil || op.Kind != "DEPLOY" || op.Snapshot == nil || op.Snapshot.Build.Mode != "NATIVE" || nativeQuarantined(st, op.ProductID) || terminalOperation(op.Status) {
		return invalid("native delivery unavailable or stopped")
	}
	if err := reservationOperationAllowed(st, *op); err != nil {
		return err
	}
	snap := op.Snapshot
	candidate := snap.MainCandidate != nil
	if candidate {
		if err := s.nativeMainChildAllowed(st, op); err != nil {
			return err
		}
	}
	if (!candidate && (op.BuildOnly || snap.Target.Purpose != "DEV" && snap.Target.Purpose != "TEST")) || op.ReleaseCandidateID != "" || snap.Target.GitOpsMode != "PR" || !snap.Target.AllowDeploy || snap.Build.RebuildMissing || op.Run != nil {
		return invalid("native delivery policy denied")
	}
	if e := s.flowChildAllowed(st, *op); e != nil {
		return e
	}
	if op.ParentID != "" {
		parent := operationByID(st, op.ParentID)
		if parent == nil || op.Native == nil || op.Native.ParentHash != nativeParentHash(parent) {
			return invalid("native composition inputs changed")
		}
	}
	env := environmentByID(st, op.EnvironmentID)
	current := componentBuild(st, op.ApplicationID)
	expectedBuild := &snap.Build
	if candidate {
		expectedBuild = snap.BuildBaseline
	}
	target := environmentBinding(st, op.EnvironmentID, op.ApplicationID)
	source := repositoryBinding(st, snap.Source.RepositoryID)
	gitops := repositoryBinding(st, snap.GitOps.RepositoryID)
	repoMatch := false
	for _, repo := range st.Repositories {
		if repo.ID == snap.Revision.RepositoryID && repo.ProductID == op.ProductID {
			b, _, e := s.nativeRecipes(op.ProductID, snap.Application, snap.Build.Profile, snap.Build.ImageRepository, snap.Build.Native)
			repoMatch = e == nil && repo.URL == b.RepositoryURL
		}
	}
	if op.Native != nil && !repoMatch {
		return invalid("native recipe repository changed")
	}
	if env == nil || env.Cluster != snap.Environment.Cluster || env.Namespace != snap.Environment.Namespace || current == nil || !reflect.DeepEqual(current, expectedBuild) || target == nil || !reflect.DeepEqual(*target, snap.Target) || source == nil || !reflect.DeepEqual(*source, snap.Source) || gitops == nil || !reflect.DeepEqual(*gitops, snap.GitOps) {
		return invalid("native delivery configuration changed")
	}
	sc, e := scopedConnection(st, snap.Source.ConnectionID, op.ProductID)
	if e != nil {
		return e
	}
	gc, e := scopedConnection(st, snap.GitOps.ConnectionID, op.ProductID)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(sc.Config, snap.SourceConnection) || !reflect.DeepEqual(gc.Config, snap.GitOpsConnection) {
		return invalid("native provider authority changed")
	}
	if op.Native != nil && op.Native.PublicationJobID != "" && !s.workerCheckGate(st, workerJob(st, op.Native.BuildJobID)) {
		return invalid("native build check success missing")
	}
	if op.Native != nil {
		b, p, e := s.nativeRecipes(op.ProductID, snap.Application, snap.Build.Profile, snap.Build.ImageRepository, snap.Build.Native)
		if e != nil {
			return e
		}
		n := op.Native
		if n.ConfigRevision != s.externalWorkers.Revision() || n.BuildRecipeHash != b.Hash || n.BuildRecipeRevision != b.Revision || n.PublicationRecipeHash != p.Hash || n.PublicationRecipeRevision != p.Revision || !time.Now().UTC().Before(n.Deadline) {
			return invalid("native authority changed or delivery deadline exceeded")
		}
	}
	return nil
}
func nativeSourceProof(op *domain.ExternalOperation) *domain.WorkerSourceProof {
	proof := &domain.WorkerSourceProof{DeliveryID: op.ID, ParentID: op.ParentID, SnapshotHash: workerconfig.Fingerprint(op.Snapshot)}
	if op.Native != nil {
		proof.ParentHash = op.Native.ParentHash
	}
	return proof
}
func (s *Service) managedWorkerAllowed(st *domain.State, j *domain.WorkerJob) error {
	if j.Inputs.SourceProof == nil {
		return nil
	}
	proof := j.Inputs.SourceProof
	op := operationByID(st, proof.DeliveryID)
	if e := s.nativeDeliveryAllowed(st, op); e != nil {
		return e
	}
	if op.Native == nil || !reflect.DeepEqual(nativeSourceProof(op), proof) || j.ProductID != op.ProductID || j.IntegrationID != op.IntegrationID || j.Inputs.RepositoryID != op.Snapshot.Revision.RepositoryID || j.Inputs.RevisionID != op.Snapshot.Revision.ID || j.Inputs.SourceSHA != op.Snapshot.Revision.HeadCommit {
		return invalid("managed worker source proof mismatch")
	}
	n := op.Native
	if j.Capability == "local_oci_build" && n.BuildJobID == j.ID || j.Capability == "registry_publish" && n.PublicationJobID == j.ID {
		return nil
	}
	return invalid("managed worker link mismatch")
}
func (s *Service) tickNativeDelivery(ctx context.Context, op domain.ExternalOperation, token string) error {
	if op.Snapshot != nil && op.Snapshot.MainCandidate != nil {
		if err := s.verifyNativeSource(ctx, op); err != nil {
			return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
		}
	}
	return s.store.Update(ctx, func(st *domain.State) error {
		parent := operationByID(st, op.ID)
		if parent == nil || parent.LeaseOwner != token {
			return invalid("native delivery lease lost")
		}
		if parent.Snapshot != nil && parent.Snapshot.MainCandidate != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !parent.LeaseUntil.After(time.Now().UTC()) {
				return invalid("main candidate child lease expired")
			}
		}
		if e := s.nativeDeliveryAllowed(st, parent); e != nil {
			stopNativeDelivery(st, parent, "BLOCKED", e.Error(), time.Now().UTC())
			return nil
		}
		now := time.Now().UTC()
		n := parent.Native
		snap := parent.Snapshot
		appendJob := func(inputs domain.WorkerInputs, capability, key string) (string, error) {
			m := domain.Meta{ID: id(), ProductID: parent.ProductID, FeatureID: parent.FeatureID, Actor: parent.RequestedBy, CreatedAt: now, UpdatedAt: now}
			operation := domain.ExternalOperation{Meta: m, ParentID: parent.ID, Kind: domain.WorkerOperationKind, IntegrationID: parent.IntegrationID, RepositoryID: inputs.RepositoryID, RequestedBy: parent.RequestedBy, Status: "PENDING", Phase: "EXTERNAL_QUEUED"}
			m.ID = id()
			j := domain.WorkerJob{Meta: m, OperationID: operation.ID, IntegrationID: parent.IntegrationID, RequestKey: key, RequestHash: workerconfig.Fingerprint(inputs), Capability: capability, Inputs: inputs, InputHash: workerconfig.Fingerprint(inputs), Status: "QUEUED", Deadline: n.Deadline, MaxAttempts: 1}
			st.Operations = append(st.Operations, operation)
			st.WorkerJobs = append(st.WorkerJobs, j)
			if e := s.queueWorkerCheck(st, j.ID); e != nil {
				return "", e
			}
			workerAudit(st, &st.WorkerJobs[len(st.WorkerJobs)-1], parent.RequestedBy, "native_job_queued", now)
			return j.ID, nil
		}
		// Appending Operations invalidates pointers into that slice; reacquire after each job.
		if n.BuildJobID == "" {
			b, _, _ := s.nativeRecipes(parent.ProductID, snap.Application, snap.Build.Profile, snap.Build.ImageRepository, snap.Build.Native)
			inputs := domain.WorkerInputs{Check: b.Check, SourceProof: nativeSourceProof(parent), RecipeID: b.ID, RecipeRevision: b.Revision, RecipeHash: b.Hash, ConfigRevision: n.ConfigRevision, RepositoryID: b.RepositoryID, RepositoryURL: b.RepositoryURL, RevisionID: snap.Revision.ID, SourceSHA: snap.Revision.HeadCommit, Trust: b.Trust, Parameters: snap.Build.Native.Parameters, Execution: b.Execution}
			jobID, e := appendJob(inputs, "local_oci_build", "native-build:"+parent.ID)
			if e != nil {
				return e
			}
			parent = operationByID(st, op.ID)
			parent.Native.BuildJobID = jobID
		} else {
			b := workerJob(st, n.BuildJobID)
			if b == nil {
				return invalid("native build link missing")
			}
			recoverWorker(st, b, now)
			if b.Status == "SUCCEEDED" {
				if !s.workerCheckGate(st, b) {
					check := operationByID(st, b.CheckOperationID)
					if check != nil && terminalOperation(check.Status) {
						stopNativeDelivery(st, parent, "BLOCKED", "native check finalization unavailable", now)
					} else {
						parent.LeaseOwner = ""
						parent.LeaseUntil = time.Time{}
						parent.NextAttemptAt = now.Add(2 * time.Second)
					}
					return nil
				}
				a := currentWorkerAttempt(st, b)
				if a == nil || a.Report == nil || a.Report.LocalOCI == nil || validateWorkerLocalOCI(b, a, a.Report) != nil {
					stopNativeDelivery(st, parent, "BLOCKED", "accepted build receipt missing", now)
					return nil
				}
				if n.PublicationJobID == "" {
					_, p, _ := s.nativeRecipes(parent.ProductID, snap.Application, snap.Build.Profile, snap.Build.ImageRepository, snap.Build.Native)
					if a.Report.LocalOCI.ArchiveBytes > p.MaxBytes {
						stopNativeDelivery(st, parent, "BLOCKED", "publication archive too large", now)
						return nil
					}
					inputs := domain.WorkerInputs{SourceProof: nativeSourceProof(parent), RecipeID: p.ID, RecipeRevision: p.Revision, RecipeHash: p.Hash, ConfigRevision: n.ConfigRevision, RepositoryID: b.Inputs.RepositoryID, RepositoryURL: b.Inputs.RepositoryURL, RevisionID: b.Inputs.RevisionID, SourceSHA: b.Inputs.SourceSHA, Trust: "publication", Parameters: map[string]any{}, Publication: &domain.WorkerPublication{ComponentID: p.ComponentID, Profile: p.Profile, Origin: p.Origin, Repository: p.Repository, Receipt: *a.Report.LocalOCI}}
					jobID, e := appendJob(inputs, "registry_publish", "native-publish:"+parent.ID)
					if e != nil {
						return e
					}
					parent = operationByID(st, op.ID)
					parent.Native.PublicationJobID = jobID
				} else {
					publication := workerJob(st, n.PublicationJobID)
					if publication == nil {
						return invalid("native publication link missing")
					}
					recoverWorker(st, publication, now)
					if publication.Status == "SUCCEEDED" {
						for _, artifact := range st.DeliveryArtifacts {
							if artifact.Native != nil && artifact.Native.PublicationJobID == publication.ID {
								known, e := validatedExistingArtifact(st, artifact.ID, parent.ProductID, parent.ApplicationID, snap.Revision.RepositoryID, snap.Revision.HeadCommit, snap.Build.ImageRepository)
								if e != nil {
									return e
								}
								parent.ExistingArtifact = known
								parent.ExpectedDigest = known.Digest
								parent.Phase = "INSPECT"
								parent.CurrentStep = parent.Phase
								break
							}
						}
					} else if workerTerminal(publication.Status) {
						stopNativeDelivery(st, parent, "BLOCKED", "native publication stopped: "+publication.Status, now)
						return nil
					}
				}
			} else if workerTerminal(b.Status) {
				stopNativeDelivery(st, parent, "BLOCKED", "native build stopped: "+b.Status, now)
				return nil
			}
		}
		parent.UpdatedAt = now
		parent.LeaseOwner = ""
		parent.LeaseUntil = time.Time{}
		parent.NextAttemptAt = now.Add(2 * time.Second)
		return nil
	})
}
func stopNativeDelivery(st *domain.State, op *domain.ExternalOperation, status, detail string, now time.Time) {
	op.Status = status
	if status == "BLOCKED" {
		op.Status = "FAILED"
		op.Error = detail
	}
	op.DeploymentState = status
	op.Detail = detail
	op.FinishedAt = &now
	op.UpdatedAt = now
	op.LeaseOwner = ""
	op.LeaseUntil = time.Time{}
	if op.Native == nil {
		return
	}
	for _, jobID := range []string{op.Native.BuildJobID, op.Native.PublicationJobID} {
		j := workerJob(st, jobID)
		if j == nil || workerTerminal(j.Status) {
			continue
		}
		j.CancellationRequested = true
		if j.Status == "RUNNING" || j.Status == "RECONCILIATION_REQUIRED" {
			j.Status = "RECONCILIATION_REQUIRED"
		} else {
			j.Status = "CANCELLED"
		}
		if a := currentWorkerAttempt(st, j); a != nil {
			a.Status = j.Status
			a.UpdatedAt = now
		}
		workerAudit(st, j, "system/native-delivery", "native_delivery_stopped", now)
	}
}
func (s *Service) cancelNativeDelivery(ctx context.Context, c domain.Command) (any, error) {
	var out domain.ExternalOperation
	err := s.store.Update(ctx, func(st *domain.State) error {
		var empty struct{}
		if decode(c.Data, &empty) != nil || c.ID == "" || c.Actor == "" || c.IntegrationID != "" || c.FeatureID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
			return invalid("invalid native cancellation scope")
		}
		op := operationByID(st, c.ID)
		candidate := op != nil && op.Kind == "RELEASE_CANDIDATE" && op.MainCandidate != nil
		if candidate {
			if op.ProductID != c.ProductID || terminalOperation(op.Status) {
				return invalid("native main candidate cannot cancel after completion")
			}
			for _, cid := range op.ChildIDs {
				child := operationByID(st, cid)
				if child == nil || !child.BuildOnly || child.Snapshot == nil || child.Snapshot.MainCandidate == nil || child.GitOpsPR != nil || child.GitOpsResult != nil {
					return invalid("candidate child is not build-only")
				}
			}
			for _, cid := range op.ChildIDs {
				child := operationByID(st, cid)
				if !terminalOperation(child.Status) {
					stopNativeDelivery(st, child, "CANCELLED", "main candidate cancelled; physical execution requires reconciliation", time.Now().UTC())
				}
			}
			stopNativeDelivery(st, op, "CANCELLED", "main candidate cancelled", time.Now().UTC())
			st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: time.Now().UTC(), EntityID: op.ID, ProductID: op.ProductID, Data: c})
			out = *op
			return nil
		}
		if op == nil || op.ProductID != c.ProductID || op.Kind != "DEPLOY" || op.Snapshot == nil || op.Snapshot.Build.Mode != "NATIVE" || terminalOperation(op.Status) || op.Phase == "APPLY_UNCERTAIN" || op.Phase == "WAIT_GITOPS_PR" || op.GitOpsPR != nil || op.GitOpsResult != nil {
			return invalid("native delivery cannot cancel after external PR intent or completion")
		}
		stopNativeDelivery(st, op, "CANCELLED", "native delivery cancelled; physical effects may require reconciliation", time.Now().UTC())
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: time.Now().UTC(), EntityID: op.ID, ProductID: op.ProductID, FeatureID: op.FeatureID, Data: c})
		out = *op
		return nil
	})
	return out, err
}

// The immutable origin survives completion; it grants no new execution rights.
func nativeWorkerOrigin(st *domain.State, j *domain.WorkerJob) error {
	if j == nil || j.Inputs.SourceProof == nil {
		return nil
	}
	proof := j.Inputs.SourceProof
	op := operationByID(st, proof.DeliveryID)
	if op == nil || op.Native == nil || op.Snapshot == nil || !reflect.DeepEqual(nativeSourceProof(op), proof) || j.ProductID != op.ProductID || j.IntegrationID != op.IntegrationID || j.Inputs.SourceSHA != op.Snapshot.Revision.HeadCommit || j.Inputs.RevisionID != op.Snapshot.Revision.ID || j.Inputs.RepositoryID != op.Snapshot.Revision.RepositoryID {
		return invalid("immutable native delivery origin mismatch")
	}
	if op.ParentID != "" {
		parent := operationByID(st, op.ParentID)
		if parent == nil || op.Native.ParentHash != nativeParentHash(parent) {
			return invalid("immutable native composition origin mismatch")
		}
	}
	if j.Capability == "local_oci_build" && op.Native.BuildJobID == j.ID || j.Capability == "registry_publish" && op.Native.PublicationJobID == j.ID {
		return nil
	}
	return invalid("immutable native job link mismatch")
}
func (s *Service) verifyNativeSource(ctx context.Context, op domain.ExternalOperation) error {
	if op.Snapshot != nil && op.Snapshot.MainCandidate != nil {
		st, err := s.store.Read(ctx)
		if err != nil {
			return err
		}
		if err = s.nativeMainChildAllowed(&st, &op); err != nil {
			return err
		}
		parent := operationByID(&st, op.ParentID)
		return s.observeMainCandidate(ctx, parent, false)
	}
	if op.Snapshot == nil || op.Snapshot.Build.Mode != "NATIVE" {
		return nil
	}
	snap := op.Snapshot
	branches, e := s.provider.Branches(ctx, snap.SourceConnection, snap.BuildRequest.Repository)
	if e != nil {
		return e
	}
	var head, base string
	for _, b := range branches {
		if b.Name == snap.Revision.Branch {
			head = b.SHA
		}
		if b.Name == configuredBase(snap.Source) {
			base = b.SHA
		}
	}
	if head != snap.Revision.HeadCommit || base == "" {
		return invalid("native source moved or missing")
	}
	if op.ParentID == "" && base != snap.Revision.BaseCommit {
		return invalid("native base moved; capture new revision")
	}
	comparisonBase := base
	if op.ParentID != "" {
		comparisonBase = snap.Revision.BaseCommit
	}
	comparison, e := s.provider.Compare(ctx, snap.SourceConnection, snap.BuildRequest.Repository, comparisonBase, head)
	if e != nil {
		return e
	}
	if comparison.Behind > 0 {
		return invalid("native source is behind base")
	}
	return nil
}

func nativeReleaseDenied(st *domain.State, op domain.ExternalOperation) error {
	if op.Kind != "RELEASE_CANDIDATE" && op.Kind != "RELEASE_PROMOTION" {
		return nil
	}
	for _, snap := range op.PreparedDeployments {
		if snap.Build.Mode == "NATIVE" {
			return invalid("native release/main advancement is unsupported")
		}
	}
	for _, childID := range op.ChildIDs {
		child := operationByID(st, childID)
		if child != nil && child.Snapshot != nil && child.Snapshot.Build.Mode == "NATIVE" {
			return invalid("native release/main advancement is unsupported")
		}
	}
	if op.CompositionSnapshot != nil {
		for _, component := range op.CompositionSnapshot.Components {
			b := componentBuild(st, component.ApplicationID)
			if b != nil && b.Mode == "NATIVE" {
				return invalid("native release/main advancement is unsupported")
			}
		}
	}
	return nil
}

func nativeParentHash(parent *domain.ExternalOperation) string {
	if parent.MainCandidate != nil {
		return workerconfig.Fingerprint(struct {
			Composition *domain.Composition
			Proof       *domain.MainCandidateProof
			Prepared    []domain.DeliverySnapshot
		}{parent.CompositionSnapshot, parent.MainCandidate, parent.PreparedDeployments})
	}
	return workerconfig.Fingerprint(struct {
		Composition *domain.Composition
		Sources     []domain.FlowSource
	}{parent.CompositionSnapshot, parent.Sources})
}

// The request-time authority is an immutable input, not a binding to resolve
// afresh when a delayed/restarted composition reaches child creation.
func (s *Service) validateFrozenNative(product string, snap domain.DeliverySnapshot, requestedAt time.Time) error {
	if snap.Build.Mode != "NATIVE" {
		if snap.Native != nil {
			return invalid("unexpected native component authority")
		}
		return nil
	}
	if snap.Native == nil {
		return invalid("missing request-time native component authority")
	}
	expected, err := s.freezeNative(product, snap.Application, snap.Build, requestedAt)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(*snap.Native, *expected) || !time.Now().UTC().Before(snap.Native.Deadline) {
		return invalid("request-time native authority changed or deadline expired")
	}
	return nil
}
func (s *Service) flowGuard(st *domain.State, op domain.ExternalOperation) error {
	if op.MainCandidate != nil {
		return s.mainCandidateAllowed(st, &op)
	}
	if err := flowGuard(st, op); err != nil {
		return err
	}
	native := flowHasNative(st, op)
	if !native {
		return nil
	}
	if nativeQuarantined(st, op.ProductID) || len(op.PreparedDeployments) != len(op.CompositionSnapshot.Components) {
		return invalid("native composition authority missing or quarantined")
	}
	expected := map[string]bool{}
	for _, component := range op.CompositionSnapshot.Components {
		if expected[component.ApplicationID] {
			return invalid("duplicate composition component")
		}
		expected[component.ApplicationID] = true
	}
	for _, snap := range op.PreparedDeployments {
		if !expected[snap.Application.ID] {
			return invalid("native composition component authority missing or duplicated")
		}
		delete(expected, snap.Application.ID)
		if err := s.validateFrozenNative(op.ProductID, snap, op.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func flowHasNative(st *domain.State, op domain.ExternalOperation) bool {
	for _, snap := range op.PreparedDeployments {
		if snap.Build.Mode == "NATIVE" || snap.Native != nil {
			return true
		}
	}
	if op.CompositionSnapshot != nil {
		for _, component := range op.CompositionSnapshot.Components {
			build := componentBuild(st, component.ApplicationID)
			if build != nil && build.Mode == "NATIVE" {
				return true
			}
		}
	}
	return false
}
