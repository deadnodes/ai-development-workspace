package application

import (
	"context"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/workerconfig"
	"strings"
	"time"
)

func nativeQuarantined(st *domain.State, product string) bool {
	for _, q := range st.NativeQuarantines {
		if q.ProductID == product {
			return true
		}
	}
	return false
}
func nativeImage(r workerconfig.PublicationRecipe) string {
	return strings.TrimPrefix(strings.TrimPrefix(r.Origin, "https://"), "http://") + "/" + r.Repository
}
func readPublication(ctx context.Context, r workerconfig.PublicationRecipe, p domain.WorkerPublication) (domain.NativeObservation, error) {
	return ocipublisher.Read(ctx, ocipublisher.ReadConfig{Origin: r.Origin, Repository: r.Repository, CAFile: r.Pull.CAFile, UsernameEnv: r.Pull.UsernameEnv, PasswordEnv: r.Pull.PasswordEnv, BearerEnv: r.Pull.BearerEnv, MaxBytes: r.MaxBytes, TimeoutSeconds: r.TimeoutSeconds, FixtureLoopbackHTTP: r.Pull.FixtureLoopbackHTTP}, p.Receipt.ManifestDigest, p.Receipt.Platform)
}
func (s *Service) executeNativePublication(ctx context.Context, c domain.Command) (any, error) {
	if s.externalWorkers == nil || strings.TrimSpace(c.Actor) == "" {
		return nil, invalid("publication authority and actor required")
	}
	if c.Action == "reconcile_worker_publication" {
		return s.reconcilePublication(ctx, c)
	}
	var in struct {
		BuildJobID     string    `json:"build_job_id"`
		ComponentID    string    `json:"component_id"`
		RecipeID       string    `json:"recipe_id"`
		RecipeHash     string    `json:"recipe_hash"`
		RecipeRevision string    `json:"recipe_revision"`
		ConfigRevision string    `json:"config_revision"`
		RequestKey     string    `json:"request_key"`
		Deadline       time.Time `json:"deadline"`
	}
	if err := decode(c.Data, &in); err != nil {
		return nil, err
	}
	if c.ID != "" || c.IntegrationID != "" || c.FeatureID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" || (!workerRequestKey.MatchString(in.RequestKey) || strings.HasPrefix(in.RequestKey, "native-build:") || strings.HasPrefix(in.RequestKey, "native-publish:")) {
		return nil, invalid("invalid publication scope or request key")
	}
	var out domain.WorkerJob
	err := s.store.Update(ctx, func(st *domain.State) error {
		now := time.Now().UTC()
		if nativeQuarantined(st, c.ProductID) {
			return invalid("native product quarantined after restore")
		}
		requestHash := workerconfig.Fingerprint(struct {
			Product, Actor string
			Request        any
		}{c.ProductID, c.Actor, in})
		for _, j := range st.WorkerJobs {
			if j.ProductID == c.ProductID && j.RequestKey == in.RequestKey {
				if j.RequestHash != requestHash {
					return invalid("conflicting publication request replay")
				}
				out = j
				return nil
			}
		}
		r, ok := s.externalWorkers.Publication(in.RecipeID)
		if !ok || r.ProductID != c.ProductID || r.ComponentID != in.ComponentID || r.Hash != in.RecipeHash || r.Revision != in.RecipeRevision || s.externalWorkers.Revision() != in.ConfigRevision {
			return invalid("publication recipe identity mismatch")
		}
		b := workerJob(st, in.BuildJobID)
		if b != nil && b.Inputs.SourceProof != nil {
			return invalid("managed build publication is server-owned")
		}
		if b == nil || b.ProductID != c.ProductID || b.Status != "SUCCEEDED" || b.Capability != "local_oci_build" || b.Inputs.RepositoryID != r.RepositoryID || b.Inputs.RecipeHash != r.BuildRecipeHash {
			return invalid("successful exact build required")
		}
		a := currentWorkerAttempt(st, b)
		if a == nil || a.Status != "SUCCEEDED" || a.Report == nil || a.Report.Result != "PASSED" || validateWorkerLocalOCI(b, a, a.Report) != nil || a.Report.LocalOCI == nil || a.Report.LocalOCI.ArchiveBytes > r.MaxBytes {
			return invalid("accepted build receipt required")
		}
		app := applicationByID(st, r.ComponentID)
		build := componentBuild(st, r.ComponentID)
		integration := integration(st, b.IntegrationID)
		if app == nil || app.ProductID != c.ProductID || app.RepositoryID != r.RepositoryID || domain.ComponentKind(*app) != "APPLICATION" || integration == nil || integration.ProductID != c.ProductID || build == nil || build.Mode != "NATIVE" || build.Profile != r.Profile || build.ImageRepository != nativeImage(r) {
			return invalid("native component/profile configuration required")
		}
		if !in.Deadline.After(now) || in.Deadline.After(now.Add(time.Hour)) {
			return invalid("publication deadline outside bounds")
		}
		inputs := domain.WorkerInputs{RecipeID: r.ID, RecipeHash: r.Hash, RecipeRevision: r.Revision, ConfigRevision: in.ConfigRevision, RepositoryID: b.Inputs.RepositoryID, RepositoryURL: b.Inputs.RepositoryURL, RevisionID: b.Inputs.RevisionID, SourceSHA: b.Inputs.SourceSHA, Trust: "publication", Parameters: map[string]any{}, Publication: &domain.WorkerPublication{ComponentID: r.ComponentID, Profile: r.Profile, Origin: r.Origin, Repository: r.Repository, Receipt: *a.Report.LocalOCI}}
		m := domain.Meta{ID: id(), ProductID: c.ProductID, FeatureID: b.FeatureID, Actor: c.Actor, CreatedAt: now, UpdatedAt: now}
		op := domain.ExternalOperation{Meta: m, Kind: domain.WorkerOperationKind, IntegrationID: b.IntegrationID, RepositoryID: r.RepositoryID, RequestedBy: c.Actor, Status: "PENDING", Phase: "EXTERNAL_QUEUED"}
		m.ID = id()
		out = domain.WorkerJob{Meta: m, OperationID: op.ID, IntegrationID: b.IntegrationID, RequestKey: in.RequestKey, RequestHash: requestHash, Capability: "registry_publish", Inputs: inputs, InputHash: workerconfig.Fingerprint(inputs), Status: "QUEUED", Deadline: in.Deadline, MaxAttempts: 1}
		st.Operations = append(st.Operations, op)
		st.WorkerJobs = append(st.WorkerJobs, out)
		workerAudit(st, &st.WorkerJobs[len(st.WorkerJobs)-1], c.Actor, c.Action, now)
		return nil
	})
	return out, err
}
func (s *Service) nativeConfigMatches(j *domain.WorkerJob) bool {
	if s.externalWorkers == nil || j == nil {
		return false
	}
	r, ok := s.externalWorkers.Publication(j.Inputs.RecipeID)
	p := j.Inputs.Publication
	return ok && p != nil && j.Capability == "registry_publish" && r.ProductID == j.ProductID && r.RepositoryID == j.Inputs.RepositoryID && r.ComponentID == p.ComponentID && r.Profile == p.Profile && r.Origin == p.Origin && r.Repository == p.Repository && r.BuildRecipeHash == p.Receipt.RecipeHash && r.Hash == j.Inputs.RecipeHash && r.Revision == j.Inputs.RecipeRevision && s.externalWorkers.Revision() == j.Inputs.ConfigRevision && workerconfig.Fingerprint(j.Inputs) == j.InputHash
}
func (s *Service) publicationEligible(st *domain.State, j *domain.WorkerJob) error {
	return s.publicationEligibleWithParent(st, j, true)
}
func (s *Service) publicationEligibleWithParent(st *domain.State, j *domain.WorkerJob, live bool) error {
	if live && j != nil {
		if err := s.managedWorkerAllowed(st, j); err != nil {
			return err
		}
	}
	if j == nil || j.Capability != "registry_publish" || nativeQuarantined(st, j.ProductID) || !s.nativeConfigMatches(j) || j.CancellationRequested {
		return invalid("native publication quarantined, cancelled or configuration changed")
	}
	if err := nativeWorkerOrigin(st, j); err != nil {
		return err
	}
	p := j.Inputs.Publication
	b := workerJob(st, p.Receipt.JobID)
	a := workerAttempt(st, p.Receipt.AttemptID)
	if b == nil || a == nil || b.ProductID != j.ProductID || b.IntegrationID != j.IntegrationID || b.Inputs.SourceSHA != j.Inputs.SourceSHA || b.Inputs.RepositoryID != j.Inputs.RepositoryID || b.Status != "SUCCEEDED" || a.JobID != b.ID || a.Status != "SUCCEEDED" || a.Report == nil || a.Report.LocalOCI == nil || !reflect.DeepEqual(*a.Report.LocalOCI, p.Receipt) {
		return invalid("publication build provenance changed")
	}
	if err := nativeWorkerOrigin(st, b); err != nil {
		return err
	}
	build := componentBuild(st, p.ComponentID)
	if j.Inputs.SourceProof != nil {
		origin := operationByID(st, j.Inputs.SourceProof.DeliveryID)
		if origin != nil && origin.Snapshot != nil && origin.Snapshot.MainCandidate != nil {
			if build == nil || origin.Snapshot.BuildBaseline == nil || !reflect.DeepEqual(*build, *origin.Snapshot.BuildBaseline) {
				return invalid("candidate baseline profile changed")
			}
			build = &origin.Snapshot.Build
		}
	}
	if build == nil || build.Mode != "NATIVE" || build.Profile != p.Profile {
		return invalid("native component profile changed")
	}
	r, _ := s.externalWorkers.Publication(j.Inputs.RecipeID)
	if build.ImageRepository != nativeImage(r) {
		return invalid("native image target changed")
	}
	return nil
}
func (s *Service) publicationReportEligible(st *domain.State, p workerconfig.Principal, in domain.WorkerReport) (*domain.WorkerJob, *domain.WorkerAttempt, error) {
	now := time.Now().UTC()
	j := workerJob(st, in.JobID)
	a := workerAttempt(st, in.AttemptID)
	if j == nil || a == nil || a.JobID != j.ID || a.Fence != in.Fence || j.Fence != in.Fence || j.InputHash != in.InputHash || a.InputHash != in.InputHash || a.Principal != p.ID() || !s.externalWorkers.Current(p, now) || !p.Allows(j.ProductID, j.Inputs.RepositoryID, j.Capability, now) {
		return nil, nil, invalid("publication worker ownership or current authority denied")
	}
	if in.Result != "PASSED" || in.LocalOCI != nil || in.Publication == nil || in.Publication.ManifestDigest != j.Inputs.Publication.Receipt.ManifestDigest || in.EvidenceHash != "" {
		return nil, nil, invalid("publication result mismatch")
	}
	if a.Report != nil {
		if reflect.DeepEqual(*a.Report, in) && j.Status == "SUCCEEDED" {
			if err := s.historicalPublicationEligible(st, j); err != nil {
				return nil, nil, err
			}
			return j, a, nil
		}
		return nil, nil, invalid("conflicting publication report")
	}
	if err := s.publicationEligible(st, j); err != nil {
		return nil, nil, err
	}
	if j.Status != "RUNNING" || a.Status != "RUNNING" || !now.Before(a.LeaseUntil) || !now.Before(j.Deadline) {
		return nil, nil, invalid("publication lease expired or invalid")
	}
	return j, a, nil
}
func (s *Service) reportPublication(ctx context.Context, p workerconfig.Principal, in domain.WorkerReport) (domain.WorkerLease, error) {
	var out domain.WorkerLease
	st, err := s.store.Read(ctx)
	if err != nil {
		return out, err
	}
	j, a, err := s.publicationReportEligible(&st, p, in)
	if err != nil {
		return out, err
	}
	if a.Report != nil {
		return domain.WorkerLease{Job: *j, Attempt: *a}, nil
	}
	recipe, _ := s.externalWorkers.Publication(j.Inputs.RecipeID)
	if j.Inputs.SourceProof != nil {
		parent := operationByID(&st, j.Inputs.SourceProof.DeliveryID)
		if parent == nil {
			return out, invalid("native parent missing")
		}
		if err := s.verifyNativeSource(ctx, *parent); err != nil {
			return out, err
		}
	}
	observation, err := readPublication(ctx, recipe, *j.Inputs.Publication)
	if err != nil {
		return out, invalid("registry bytes unavailable or invalid")
	}
	if j.Inputs.SourceProof != nil {
		parent := operationByID(&st, j.Inputs.SourceProof.DeliveryID)
		if err := s.verifyNativeSource(ctx, *parent); err != nil {
			return out, err
		}
	}
	err = s.store.Update(ctx, func(st *domain.State) error {
		j, a, e := s.publicationReportEligible(st, p, in)
		if e != nil {
			return e
		}
		if a.Report == nil {
			copy := in
			a.Report = &copy
			j.Status = "SUCCEEDED"
			a.Status = "SUCCEEDED"
			a.UpdatedAt = time.Now().UTC()
			s.admitNative(st, j, a, observation, nil)
			workerAudit(st, j, p.ID(), "worker_report", time.Now().UTC())
		}
		out = domain.WorkerLease{Job: *j, Attempt: *a}
		return nil
	})
	return out, err
}
func (s *Service) admitNative(st *domain.State, j *domain.WorkerJob, a *domain.WorkerAttempt, o domain.NativeObservation, reconciliation *domain.NativeReconciliation) {
	for _, v := range st.DeliveryArtifacts {
		if v.Native != nil && v.Native.PublicationJobID == j.ID {
			return
		}
	}
	p := j.Inputs.Publication
	r, _ := s.externalWorkers.Publication(j.Inputs.RecipeID)
	now := time.Now().UTC()
	st.DeliveryArtifacts = append(st.DeliveryArtifacts, domain.DeliveryArtifact{Meta: domain.Meta{ID: id(), ProductID: j.ProductID, FeatureID: j.FeatureID, Actor: "system/native-admission", CreatedAt: now, UpdatedAt: now}, OperationID: j.OperationID, ApplicationID: p.ComponentID, RepositoryID: j.Inputs.RepositoryID, SourceCommit: j.Inputs.SourceSHA, ImageRepository: nativeImage(r), Digest: p.Receipt.ManifestDigest, Availability: "PRESENT", ObservedAt: o.ObservedAt, Provenance: "NATIVE_WORKER", Native: &domain.NativeArtifact{PublicationJobID: j.ID, PublicationAttemptID: a.ID, PublicationInputHash: j.InputHash, Profile: p.Profile, Receipt: p.Receipt, Observation: o, Reconciliation: reconciliation}})
}
func (s *Service) reconcilePublication(ctx context.Context, c domain.Command) (any, error) {
	var empty struct{}
	if decode(c.Data, &empty) != nil || c.ID == "" || c.FeatureID != "" || c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return nil, invalid("invalid reconciliation scope")
	}
	eligible := func(st *domain.State) (*domain.WorkerJob, *domain.WorkerAttempt, error) {
		j := workerJob(st, c.ID)
		if j == nil || j.ProductID != c.ProductID {
			return nil, nil, invalid("publication outside product")
		}

		a := currentWorkerAttempt(st, j)
		if j.Status == "SUCCEEDED" && a != nil && a.Status == "EXPIRED" && a.Report == nil {
			for _, artifact := range st.DeliveryArtifacts {
				if artifact.Native != nil && artifact.Native.PublicationJobID == j.ID && artifact.Native.PublicationAttemptID == a.ID && artifact.Native.Reconciliation != nil {
					if e := s.historicalPublicationEligible(st, j); e != nil {
						return nil, nil, e
					}
					return j, a, nil
				}
			}
		}
		if e := s.publicationEligible(st, j); e != nil {
			return nil, nil, e
		}
		now := time.Now().UTC()
		if a == nil || j.Status != "RECONCILIATION_REQUIRED" || a.Status != "EXPIRED" || a.Report != nil || !now.Before(j.Deadline) {
			return nil, nil, invalid("ordinary expired publication reconciliation required")
		}
		return j, a, nil
	}
	var frozen domain.WorkerJob
	err := s.store.Update(ctx, func(st *domain.State) error {
		j := workerJob(st, c.ID)
		if j != nil {
			recoverWorker(st, j, time.Now().UTC())
		}
		j, _, e := eligible(st)
		if e == nil {
			frozen = *j
		}
		return e
	})
	if err != nil {
		return nil, err
	}
	if frozen.Status == "SUCCEEDED" {
		return frozen, nil
	}
	checkManagedSource := func() error {
		if frozen.Inputs.SourceProof == nil {
			return nil
		}
		state, e := s.store.Read(ctx)
		if e != nil {
			return e
		}
		parent := operationByID(&state, frozen.Inputs.SourceProof.DeliveryID)
		if parent == nil {
			return invalid("native parent missing")
		}
		return s.verifyNativeSource(ctx, *parent)
	}
	if e := checkManagedSource(); e != nil {
		return nil, e
	}
	r, _ := s.externalWorkers.Publication(frozen.Inputs.RecipeID)
	o, err := readPublication(ctx, r, *frozen.Inputs.Publication)
	if err != nil {
		return nil, invalid("registry bytes unavailable or invalid")
	}
	if e := checkManagedSource(); e != nil {
		return nil, e
	}
	var out domain.WorkerJob
	err = s.store.Update(ctx, func(st *domain.State) error {
		j, a, e := eligible(st)
		if e != nil {
			return e
		}
		if j.Status == "SUCCEEDED" {
			out = *j
			return nil
		}
		if j.InputHash != frozen.InputHash || j.Fence != frozen.Fence {
			return invalid("publication fence changed")
		}
		now := time.Now().UTC()
		proof := &domain.NativeReconciliation{Actor: c.Actor, At: now, AttemptStatus: a.Status}
		s.admitNative(st, j, a, o, proof)
		j.Status = "SUCCEEDED"
		workerAudit(st, j, c.Actor, c.Action, now)
		out = *j
		return nil
	})
	return out, err
}

func (s *Service) inspectNativeArtifact(ctx context.Context, op domain.ExternalOperation) (delivery.ArtifactResult, error) {
	var out delivery.ArtifactResult
	st, e := s.store.Read(ctx)
	if e != nil {
		return out, e
	}
	known := op.ExistingArtifact
	if known == nil || known.Native == nil || op.Snapshot == nil {
		return out, invalid("native artifact required")
	}
	if _, e = validatedExistingArtifact(&st, known.ID, op.ProductID, op.ApplicationID, op.Snapshot.Revision.RepositoryID, op.Snapshot.Revision.HeadCommit, op.Snapshot.Build.ImageRepository); e != nil {
		return out, e
	}
	j := workerJob(&st, known.Native.PublicationJobID)
	if e = s.historicalPublicationEligible(&st, j); e != nil {
		return out, e
	}
	r, _ := s.externalWorkers.Publication(j.Inputs.RecipeID)
	o, e := readPublication(ctx, r, *j.Inputs.Publication)
	if e != nil {
		return out, e
	}
	after, e := s.store.Read(ctx)
	if e != nil {
		return out, e
	}
	if e = s.historicalPublicationEligible(&after, workerJob(&after, j.ID)); e != nil {
		return out, e
	}
	return delivery.ArtifactResult{Available: true, Digest: o.ManifestDigest, ObservedAt: o.ObservedAt}, nil
}

func (s *Service) historicalPublicationEligible(st *domain.State, j *domain.WorkerJob) error {
	return s.publicationEligibleWithParent(st, j, false)
}
