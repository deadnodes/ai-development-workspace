package application

import (
	"context"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"slices"
	"sort"
	"time"
)

func (s *Service) baselineArtifact(st *domain.State, product string, c domain.DevBaselineComponent) (*domain.DeliveryArtifact, error) {
	for _, a := range st.DeliveryArtifacts {
		if a.ID != c.ArtifactID {
			continue
		}
		if workerconfig.Fingerprint(a) != c.ArtifactHash || a.ApplicationID != c.ApplicationID || a.Digest != c.Digest || a.ImageRepository != c.ImageRepository {
			return nil, invalid("original baseline artifact changed")
		}
		known, err := validatedExistingArtifact(st, a.ID, product, c.ApplicationID, c.RepositoryID, a.SourceCommit, c.ImageRepository)
		if err != nil {
			return nil, err
		}
		if known.Native != nil {
			if err := s.historicalPublicationEligible(st, workerJob(st, known.Native.PublicationJobID)); err != nil {
				return nil, err
			}
		}
		return known, nil
	}
	return nil, invalid("original baseline artifact missing")
}
func (s *Service) findBaselineArtifact(st *domain.State, product string, c domain.DevBaselineComponent) (*domain.DeliveryArtifact, error) {
	for _, a := range st.DeliveryArtifacts {
		if a.ProductID != product || a.ApplicationID != c.ApplicationID || a.ImageRepository != c.ImageRepository || a.Digest != c.Digest {
			continue
		}
		if a.Native == nil && a.BuildRunID <= 0 {
			continue
		}
		c.ArtifactID = a.ID
		c.ArtifactHash = workerconfig.Fingerprint(a)
		return s.baselineArtifact(st, product, c)
	}
	return nil, nil
}
func (s *Service) baselineObservationAuthority(st *domain.State, product string, c domain.DevBaselineComponent) (workerconfig.RegistryObserveRecipe, error) {
	var out workerconfig.RegistryObserveRecipe
	count := 0
	for _, a := range s.registryAuthorities(st, time.Now().UTC()) {
		if a.Current && a.Scope.ProductID == product && a.Scope.ComponentID == c.ApplicationID && a.Scope.RepositoryID == c.RepositoryID && a.Scope.ImageRepository == c.ImageRepository {
			r, ok := s.externalWorkers.Observation(a.RecipeID)
			if ok {
				out = r
				count++
			}
		}
	}
	if count != 1 {
		return out, invalid("one exact current configured baseline registry observer required")
	}
	return out, nil
}
func (s *Service) baselineRegistryJob(st *domain.State, op domain.ExternalOperation, c domain.DevBaselineComponent) (*domain.WorkerJob, error) {
	j := workerJob(st, c.RegistryJobID)
	if j == nil || j.ProductID != op.ProductID || j.Capability != "registry_observe" || j.Inputs.Registry == nil || j.InputHash != c.RegistryInputHash || j.Inputs.Registry.RootSetHash != c.RegistryRootSetHash || !slices.Contains(j.Inputs.Registry.Roots, c.Digest) || j.Inputs.Registry.Scope.ComponentID != c.ApplicationID || j.Inputs.Registry.Scope.ImageRepository != c.ImageRepository || j.Inputs.Registry.Scope.RepositoryID != c.RepositoryID || j.CreatedAt.Before(op.CreatedAt) || j.CancellationRequested {
		return nil, invalid("baseline causal registry job changed")
	}
	child := operationByID(st, j.OperationID)
	if child == nil || child.Kind != domain.RegistryObserveOperationKind || child.ProductID != op.ProductID || child.ApplicationID != c.ApplicationID || child.RepositoryID != c.RepositoryID || child.Status == "CANCELLED" || child.Status == "FAILED" || child.ParentID != op.ID || !slices.Contains(op.ChildIDs, child.ID) || child.RegistryObservation == nil || !reflect.DeepEqual(*child.RegistryObservation, j.Inputs) {
		return nil, invalid("baseline registry operation link changed")
	}
	if err := s.registryWorkerAllowed(st, j); err != nil {
		return nil, err
	}
	return j, nil
}
func (s *Service) capturePhysicalBaseline(ctx context.Context, op domain.ExternalOperation) (domain.DevBaselineProof, error) {
	st, err := s.store.Read(ctx)
	if err != nil {
		return domain.DevBaselineProof{}, err
	}
	p := *op.DevBaseline
	p.Components = append([]domain.DevBaselineComponent(nil), p.Components...)
	heads := map[string]string{}
	verifier := s.provider.(delivery.GitOpsRevisionVerifier)
	for i, c := range p.Components {
		connection, e := scopedConnection(&st, c.ConnectionID, op.ProductID)
		if e != nil {
			return p, e
		}
		key := runtimeGitOpsGroupKey(connection.Config, c.GitOps)
		request := c.GitOps
		if heads[key] != "" {
			request.ExpectedHeadSHA = heads[key]
		}
		if err := s.validateBaselineIO(ctx, op); err != nil {
			return p, err
		}
		actual, e := s.provider.ReadGitOps(ctx, connection.Config, request)
		if e != nil {
			return p, invalid("baseline GitOps read unavailable")
		}
		if actual.ImageRepository != c.ImageRepository || !validArtifactDigest(actual.Digest) || !fullSHA.MatchString(actual.HeadSHA) || !fullSHA.MatchString(actual.BlobSHA) || actual.Content == "" {
			return p, invalid("baseline exact immutable component GitOps image required")
		}
		if heads[key] != "" && heads[key] != actual.HeadSHA {
			return p, invalid("baseline common GitOps head drift")
		}
		heads[key] = actual.HeadSHA
		if err := s.validateBaselineIO(ctx, op); err != nil {
			return p, err
		}
		revision, e := verifier.VerifyGitOpsRevision(ctx, connection.Config, delivery.GitOpsRevisionRequest{Repository: c.GitOps.Repository, Ref: c.GitOps.Ref, HeadSHA: actual.HeadSHA, AncestorSHAs: []string{actual.HeadSHA}})
		if e != nil || !validRevisionProof(delivery.GitOpsRevisionRequest{Repository: c.GitOps.Repository, Ref: c.GitOps.Ref, HeadSHA: actual.HeadSHA, AncestorSHAs: []string{actual.HeadSHA}}, revision) {
			return p, invalid("baseline exact GitOps identity unavailable")
		}
		observed := runtimeGitOpsObservation(actual)
		c.Digest = actual.Digest
		c.ObservedGitOps = &observed
		c.Revision = &revision
		known, e := s.findBaselineArtifact(&st, op.ProductID, c)
		if e != nil {
			return p, e
		}
		if known != nil {
			c.ArtifactID = known.ID
			c.ArtifactHash = workerconfig.Fingerprint(*known)
		} else if _, e = s.baselineObservationAuthority(&st, op.ProductID, c); e != nil {
			return p, e
		}
		p.Components[i] = c
	}
	p.SnapshotHash = baselineProofHash(p)
	if !baselineSize(p, nil) {
		return p, invalid("baseline staged proof bound exceeded")
	}
	return p, nil
}
func (s *Service) stageBaseline(ctx context.Context, op domain.ExternalOperation, token string, p domain.DevBaselineProof) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token || !current.LeaseUntil.After(time.Now().UTC()) || ctx.Err() != nil {
			return invalid("baseline stage lease unavailable")
		}
		if err := s.baselineAllowed(st, op, true, true); err != nil {
			return err
		}
		current.DevBaseline = &p // protection roots are visible before deriving registry inputs
		for i, c := range p.Components {
			if c.ArtifactID != "" {
				continue
			}
			r, err := s.baselineObservationAuthority(st, op.ProductID, c)
			if err != nil {
				return err
			}
			scope := observationScope(r)
			retention := s.evaluateRetention(*st, op.ProductID, time.Now().UTC())
			roots := domain.RegistryKnownRoots(retention, scope.ImageRepository)
			if !slices.Contains(roots, c.Digest) || len(roots) > r.Limits.MaxNodes || len(roots) > 256 {
				return invalid("baseline protected registry roots missing or exceed limits")
			}
			sort.Strings(roots)
			registry := &domain.WorkerRegistryInputs{Scope: scope, Roots: roots, RootSetHash: domain.RegistryRootSetHash(scope, r.ID, r.Revision, r.Hash, s.externalWorkers.Revision(), roots), RequestRetentionInputHash: retention.InputHash, Limits: r.Limits}
			repoURL := ""
			for _, repo := range st.Repositories {
				if repo.ID == c.RepositoryID && repo.ProductID == op.ProductID {
					repoURL = repo.URL
				}
			}
			inputs := domain.WorkerInputs{RecipeID: r.ID, RecipeRevision: r.Revision, RecipeHash: r.Hash, ConfigRevision: s.externalWorkers.Revision(), RepositoryID: c.RepositoryID, RepositoryURL: repoURL, Trust: "registry_observation", Parameters: map[string]any{}, Registry: registry}
			now := time.Now().UTC()
			m := domain.Meta{ID: id(), ProductID: op.ProductID, Actor: op.RequestedBy, CreatedAt: now, UpdatedAt: now}
			child := domain.ExternalOperation{Meta: m, Kind: domain.RegistryObserveOperationKind, ParentID: op.ID, ApplicationID: c.ApplicationID, RepositoryID: c.RepositoryID, RequestedBy: op.RequestedBy, RegistryObservation: &inputs, Status: "PENDING", Phase: "EXTERNAL_QUEUED"}
			m.ID = id()
			job := domain.WorkerJob{Meta: m, OperationID: child.ID, RequestKey: "baseline-registry-" + workerconfig.Fingerprint(struct{ Capture, App string }{op.ID, c.ApplicationID})[7:], RequestHash: workerconfig.Fingerprint(inputs), Capability: "registry_observe", Inputs: inputs, InputHash: workerconfig.Fingerprint(inputs), Status: "QUEUED", Deadline: now.Add(time.Hour), MaxAttempts: 1}
			c.RegistryJobID = job.ID
			c.RegistryInputHash = job.InputHash
			c.RegistryRootSetHash = registry.RootSetHash
			p.Components[i] = c
			current.ChildIDs = append(current.ChildIDs, child.ID)
			st.Operations = append(st.Operations, child)
			st.WorkerJobs = append(st.WorkerJobs, job)
			workerAudit(st, &st.WorkerJobs[len(st.WorkerJobs)-1], op.RequestedBy, "baseline_registry_queued", now)
			current = operationByID(st, op.ID)
		}
		p.SnapshotHash = baselineProofHash(p)
		if !baselineSize(p, nil) {
			return invalid("baseline queue proof bound exceeded")
		}
		current.DevBaseline = &p
		current.Phase = "WAIT_REGISTRY"
		current.CurrentStep = current.Phase
		current.Detail = "Original baseline images protected; awaiting exact availability evidence"
		current.Status = "RUNNING"
		current.LeaseOwner = ""
		current.LeaseUntil = time.Time{}
		current.UpdatedAt = time.Now().UTC()
		current.NextAttemptAt = current.UpdatedAt
		return nil
	})
}
func baselineRuntimeProof(op domain.ExternalOperation) domain.DevRuntimeProof {
	p := domain.DevRuntimeProof{Request: domain.DevRuntimeRequest{ActivationOperationID: op.ID, RequestKey: op.RequestKey, ExpectedSelection: op.DevBaseline.Request.ExpectedSelection}, Generation: op.DevBaseline.Generation, ActivationHash: op.DevBaseline.SnapshotHash}
	for _, c := range op.DevBaseline.Components {
		p.Components = append(p.Components, domain.DevRuntimeComponent{AuthorityHash: c.AuthorityHash, ApplicationID: c.ApplicationID, ArtifactID: c.ArtifactID, ImageRepository: c.ImageRepository, Digest: c.Digest, GitOps: c.GitOps, GitOpsCommit: c.ObservedGitOps.HeadSHA, ConfigHash: c.ConfigHash, Scopes: c.Scopes})
	}
	p.SnapshotHash = devRuntimeProofHash(p)
	return p
}
func (s *Service) baselineConnections(st *domain.State, op domain.ExternalOperation) (map[string]delivery.Connection, error) {
	out := map[string]delivery.Connection{}
	for _, c := range op.DevBaseline.Components {
		connection, e := scopedConnection(st, c.ConnectionID, op.ProductID)
		if e != nil {
			return nil, e
		}
		out[c.ApplicationID] = connection.Config
	}
	return out, nil
}
func baselineRegistryContentHash(v domain.RegistryInventory, digest string, now time.Time) (string, error) {
	if domain.RegistryInventoryFreshness(v, now) != "FRESH" || v.Inputs.Registry == nil {
		return "", invalid("baseline fresh image content unavailable")
	}
	return domain.RegistryImageContentProofHash(v.Report.Graph, v.Inputs.Registry.Limits, v.Inputs.Registry.Roots, digest)
}
func (s *Service) baselineRegistryReceipt(st *domain.State, op domain.ExternalOperation, c domain.DevBaselineComponent, now time.Time) (*domain.DevBaselineRegistryReceipt, error) {
	j, e := s.baselineRegistryJob(st, op, c)
	if e != nil {
		return nil, e
	}
	if j.Status != "SUCCEEDED" {
		return nil, invalid("baseline registry observation incomplete")
	}
	child := operationByID(st, j.OperationID)
	if child == nil || child.Status != "SUCCEEDED" || child.Phase != "EXTERNAL_SUCCEEDED" || child.FinishedAt == nil {
		return nil, invalid("baseline registry operation completion unavailable")
	}

	for _, v := range st.RegistryInventories {
		if v.JobID != j.ID {
			continue
		}
		a := workerAttempt(st, v.AttemptID)
		if a == nil || a.Report == nil || a.Report.Registry == nil || !child.UpdatedAt.Equal(v.ReceivedAt) || !child.FinishedAt.Equal(v.ReceivedAt) || a.Status != "SUCCEEDED" || v.Fence != j.Fence || v.Fence != a.Fence || v.InputHash != j.InputHash || !reflect.DeepEqual(v.Inputs, j.Inputs) || !reflect.DeepEqual(v.Report, *a.Report.Registry) || domain.ValidateRegistryWorkerReport(*j, *a, *a.Report, v.ReceivedAt) != nil {
			return nil, invalid("baseline complete fresh causal registry closure unavailable")
		}
		contentHash, e := baselineRegistryContentHash(v, c.Digest, now)
		if e != nil {
			return nil, invalid("baseline complete image content unavailable")
		}
		closure := "UNKNOWN"
		blockers := append([]string(nil), v.Report.Graph.Blockers...)
		for _, root := range v.Report.Graph.Roots {
			if root.Digest == c.Digest {
				closure = root.Closure
				blockers = append(blockers, root.Blockers...)
			}
		}
		slices.Sort(blockers)
		blockers = slices.Compact(blockers)
		return &domain.DevBaselineRegistryReceipt{ClosureKind: "IMAGE_CONTENT", ContentProofHash: contentHash, MetadataRootClosure: closure, MetadataDiscovery: v.Report.Graph.Discovery, MetadataBlockers: blockers, GlobalInventoryComplete: v.Report.Graph.GlobalInventoryComplete, JobID: j.ID, OperationID: j.OperationID, AttemptID: a.ID, Fence: v.Fence, InputHash: j.InputHash, RootSetHash: c.RegistryRootSetHash, InventoryID: v.ID, GraphHash: v.Report.Graph.GraphHash, ObservedAt: v.Report.FinishedAt}, nil
	}
	return nil, invalid("baseline causal inventory absent")
}
func (s *Service) inspectBaselineArtifact(ctx context.Context, op domain.ExternalOperation, c domain.DevBaselineComponent) (delivery.ArtifactResult, error) {
	st, e := s.store.Read(ctx)
	if e != nil {
		return delivery.ArtifactResult{}, e
	}
	known, e := s.baselineArtifact(&st, op.ProductID, c)
	if e != nil {
		return delivery.ArtifactResult{}, e
	}
	if known.Native != nil {
		return s.inspectNativeArtifact(ctx, domain.ExternalOperation{Meta: domain.Meta{ProductID: op.ProductID}, ApplicationID: c.ApplicationID, ExistingArtifact: known, Snapshot: &domain.DeliverySnapshot{Revision: domain.IntegrationRevision{RepositoryID: known.RepositoryID, HeadCommit: known.SourceCommit}, Build: domain.ComponentBuild{ImageRepository: known.ImageRepository}}})
	}
	binding := repositoryBinding(&st, known.RepositoryID)
	if binding == nil {
		return delivery.ArtifactResult{}, invalid("baseline artifact source authority missing")
	}
	connection, e := scopedConnection(&st, binding.ConnectionID, op.ProductID)
	if e != nil {
		return delivery.ArtifactResult{}, e
	}
	return s.provider.InspectArtifact(ctx, connection.Config, delivery.ArtifactRequest{Repository: known.ImageRepository, Tag: known.Tag, ExpectedDigest: known.Digest, EvidenceRepository: known.BuildRequest.Repository, EvidenceRunID: known.BuildRunID, EvidenceOperationID: known.BuildRequest.OperationID, SourceSHA: known.SourceCommit})
}
func (s *Service) tickDevBaseline(ctx context.Context, op domain.ExternalOperation, token string) error {
	if e := s.validateBaselineIO(ctx, op); e != nil {
		return s.finishBaseline(ctx, op, token, nil, "AUTHORITY_CHANGED")
	}
	if op.Phase == "CAPTURE_GITOPS" {
		p, e := s.capturePhysicalBaseline(ctx, op)
		if e != nil {
			return s.finishBaseline(ctx, op, token, nil, runtimeFailureReason(e))
		}
		if e = s.stageBaseline(ctx, op, token, p); e != nil {
			return s.finishBaseline(ctx, op, token, nil, runtimeFailureReason(e))
		}
		return nil
	}
	if op.Phase != "WAIT_REGISTRY" {
		return s.finishBaseline(ctx, op, token, nil, "EVIDENCE_UNAVAILABLE")
	}
	st, e := s.store.Read(ctx)
	if e != nil {
		return e
	}
	for _, c := range op.DevBaseline.Components {
		if c.RegistryJobID != "" {
			j, e := s.baselineRegistryJob(&st, op, c)
			if e != nil {
				return s.finishBaseline(ctx, op, token, nil, "AUTHORITY_CHANGED")
			}
			if !workerTerminal(j.Status) {
				return s.retry(ctx, op, token, "waiting for causal baseline registry observation")
			}
			if j.Status != "SUCCEEDED" {
				return s.finishBaseline(ctx, op, token, nil, "EVIDENCE_UNAVAILABLE")
			}
		}
	}
	result := domain.DevBaselineResult{ExpectedHash: op.DevBaseline.SnapshotHash, Outcome: "CAPTURED", Reason: "EXACT_SHARED_BASELINE_MATCH"}
	for _, c := range op.DevBaseline.Components {
		cr := domain.DevBaselineComponentResult{ApplicationID: c.ApplicationID, ImageRepository: c.ImageRepository, Digest: c.Digest, GitOps: *c.ObservedGitOps, ArtifactID: c.ArtifactID, ArtifactHash: c.ArtifactHash}
		if c.ArtifactID != "" {
			if e := s.validateBaselineIO(ctx, op); e != nil {
				return s.finishBaseline(ctx, op, token, nil, "AUTHORITY_CHANGED")
			}
			availability, e := s.inspectBaselineArtifact(ctx, op, c)
			if e != nil || !availability.Available || availability.Digest != c.Digest || availability.ObservedAt.IsZero() {
				return s.finishBaseline(ctx, op, token, nil, "EVIDENCE_UNAVAILABLE")
			}
			cr.Availability = &availability
		} else {
			receipt, e := s.baselineRegistryReceipt(&st, op, c, time.Now().UTC())
			if e != nil {
				return s.finishBaseline(ctx, op, token, nil, "EVIDENCE_UNAVAILABLE")
			}
			cr.Registry = receipt
		}
		result.Components = append(result.Components, cr)
	}
	connections, e := s.baselineConnections(&st, op)
	if e != nil {
		return s.finishBaseline(ctx, op, token, nil, "AUTHORITY_CHANGED")
	}
	runtime, e := s.readRuntimeProof(ctx, baselineRuntimeProof(op), connections, func(ctx context.Context) error { return s.validateBaselineIO(ctx, op) })
	if e != nil {
		return s.finishBaseline(ctx, op, token, nil, runtimeFailureReason(e))
	}
	result.Runtime = &runtime
	result.ObservedAt = time.Now().UTC()
	result.Hash = baselineResultHash(result)
	return s.finishBaseline(ctx, op, token, &result, "")
}
func (s *Service) validateBaselineResult(st *domain.State, op domain.ExternalOperation, r domain.DevBaselineResult) error {
	p := op.DevBaseline
	if p == nil || r.Outcome != "CAPTURED" || r.Reason != "EXACT_SHARED_BASELINE_MATCH" || r.ExpectedHash != p.SnapshotHash || r.Hash != baselineResultHash(r) || r.Runtime == nil || r.ObservedAt.IsZero() || len(r.Components) != len(p.Components) || !baselineSize(*p, &r) {
		return invalid("baseline result proof mismatch")
	}
	for _, c := range p.Components {
		if !validArtifactDigest(c.Digest) || c.ObservedGitOps == nil || c.Revision == nil {
			return invalid("baseline complete physical proof unavailable")
		}
	}
	if r.ObservedAt.Before(op.CreatedAt) || r.Runtime.ObservedAt.Before(op.CreatedAt) {
		return invalid("baseline observation predates admission")
	}
	for _, component := range r.Runtime.Components {
		for _, role := range component.Roles {
			if role.ObservedAt.Before(op.CreatedAt) {
				return invalid("baseline role observation predates admission")
			}
		}
	}
	if r.Runtime.ObservedAt.IsZero() || r.Runtime.ObservedAt.After(r.ObservedAt) || r.ObservedAt.Sub(r.Runtime.ObservedAt) > 5*time.Minute {
		return invalid("nested baseline runtime observation time invalid")
	}
	if e := validateDevRuntimeResultAt(baselineRuntimeProof(op), *r.Runtime, r.ObservedAt); e != nil {
		return e
	}
	for i, c := range p.Components {
		cr := r.Components[i]
		if cr.ApplicationID != c.ApplicationID || cr.ImageRepository != c.ImageRepository || cr.Digest != c.Digest || cr.GitOps != *c.ObservedGitOps || cr.ArtifactID != c.ArtifactID || cr.ArtifactHash != c.ArtifactHash {
			return invalid("baseline component result changed")
		}
		actual := r.Runtime.Components[i]
		if actual.GitOps != *c.ObservedGitOps {
			return invalid("baseline initial/final GitOps drift")
		}
		if c.ArtifactID != "" {
			if cr.Registry != nil || cr.Availability == nil || !cr.Availability.Available || cr.Availability.Digest != c.Digest || cr.Availability.ObservedAt.IsZero() || cr.Availability.ObservedAt.Before(op.CreatedAt) || cr.Availability.ObservedAt.After(r.ObservedAt) || r.ObservedAt.Sub(cr.Availability.ObservedAt) > 5*time.Minute {
				return invalid("baseline artifact availability changed")
			}
			if _, e := s.baselineArtifact(st, op.ProductID, c); e != nil {
				return e
			}
		} else {
			receipt, e := s.baselineRegistryReceipt(st, op, c, r.ObservedAt)
			if e != nil || cr.Availability != nil || !reflect.DeepEqual(receipt, cr.Registry) {
				return invalid("baseline causal availability receipt changed")
			}
		}
	}
	return nil
}
func (s *Service) finishBaseline(ctx context.Context, op domain.ExternalOperation, token string, result *domain.DevBaselineResult, reason string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token || terminalOperation(current.Status) {
			return invalid("baseline capture lease lost")
		}
		now := time.Now().UTC()
		valid := ctx.Err() == nil && current.LeaseUntil.After(now) && s.baselineAllowed(st, op, true, true) == nil && result != nil && s.validateBaselineResult(st, op, *result) == nil && result.ObservedAt.Before(now.Add(time.Nanosecond)) && now.Sub(result.ObservedAt) <= 5*time.Minute
		if !valid {
			if !validRuntimeReason(reason) {
				reason = "AUTHORITY_CHANGED"
			}
			hash := ""
			if op.DevBaseline != nil {
				hash = op.DevBaseline.SnapshotHash
			}
			result = &domain.DevBaselineResult{ExpectedHash: hash, Outcome: "UNVERIFIED", Reason: reason, ObservedAt: now}
			result.Hash = baselineResultHash(*result)
		}
		if op.DevBaseline != nil && !baselineSize(*op.DevBaseline, result) {
			return invalid("baseline final proof bound exceeded")
		}
		current.DevBaselineResult = result
		current.Status = "FAILED"
		current.Phase = "BASELINE_UNVERIFIED"
		current.DeploymentState = current.Phase
		current.Detail = "Shared baseline unverified; candidate activation remains blocked (" + result.Reason + ")"
		current.Error = current.Detail
		if valid {
			current.Status = "SUCCEEDED"
			current.Phase = "BASELINE_CAPTURED"
			current.DeploymentState = current.Phase
			current.Detail = "Original shared baseline captured at observation; no candidate effects, restoration or unlock performed"
			current.Error = ""
		}
		current.CurrentStep = current.Phase
		current.FinishedAt = &now
		current.UpdatedAt = now
		current.LeaseOwner = ""
		current.LeaseUntil = time.Time{}
		st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, Actor: "system/baseline-capture", CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, Phase: current.Phase, Status: current.Status, Detail: current.Detail, Evidence: map[string]string{"result_hash": result.Hash}})
		return nil
	})
}
