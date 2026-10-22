package application

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

const devActivationKind = "DEV_CANDIDATE_DEPLOY"

func activationProofHash(p domain.DevActivationProof) string {
	p.SnapshotHash = ""
	return workerconfig.Fingerprint(p)
}

func activationPRHash(pr delivery.GitOpsPullRequest) string {
	return workerconfig.Fingerprint(struct {
		Number       int
		URL, HeadSHA string
	}{pr.Number, pr.URL, pr.HeadSHA})
}

func devActivationParent(st *domain.State, op domain.ExternalOperation) *domain.ExternalOperation {
	if op.Kind == devActivationKind {
		return operationByID(st, op.ID)
	}
	if op.Kind == "DEPLOY" && op.ParentID != "" {
		p := operationByID(st, op.ParentID)
		if p != nil && p.Kind == devActivationKind {
			return p
		}
	}
	return nil
}

// The capability is an exact persisted parent/child/proof relation, never a
// kind-only exemption from the reservation fence.
func devActivationGuard(st *domain.State, op domain.ExternalOperation) error {
	parent := devActivationParent(st, op)
	if parent == nil || parent.DevActivation == nil || parent.ProductID != op.ProductID || parent.EnvironmentID != op.EnvironmentID || parent.RequestedBy != op.RequestedBy || parent.ReservationQuarantined || parent.Status == "FAILED" || parent.Status == "CANCELLED" {
		return invalid("candidate Dev activation unavailable or stopped")
	}
	p := parent.DevActivation
	if op.Kind == devActivationKind && (!reflect.DeepEqual(op.DevActivation, parent.DevActivation) || !reflect.DeepEqual(op.ChildIDs, parent.ChildIDs) || !reflect.DeepEqual(op.PreparedDeployments, parent.PreparedDeployments) || op.RequestHash != parent.RequestHash || op.RequestKey != parent.RequestKey || op.ReservationEpoch != parent.ReservationEpoch || op.ParentID != "" || op.BuildOnly || op.Native != nil || op.Snapshot != nil || op.MainCandidate != nil || op.ReleaseCandidateID != "" || len(op.Sources) != 0) {
		return invalid("candidate Dev aggregate worker proof changed")
	}
	if err := validateActivationGroups(st, parent); err != nil {
		return err
	}
	if p.SnapshotHash == "" || p.SnapshotHash != activationProofHash(*p) || parent.RequestKey != p.Request.RequestKey || p.Request.ReservationOperationID != p.ReservationOperationID || parent.RequestHash != workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevReservationRequest
	}{"deploy_candidate_dev", parent.ProductID, parent.RequestedBy, p.Request}) {
		return invalid("candidate Dev activation proof changed")
	}
	hold := activeDevReservation(st, parent.EnvironmentID)
	if hold == nil || hold.ID != p.ReservationOperationID || hold.ProductID != parent.ProductID || hold.RequestedBy != parent.RequestedBy || hold.Status != "SUCCEEDED" || hold.Phase != "RESERVED" || hold.ReservationQuarantined || hold.DevReservation == nil || workerconfig.Fingerprint(hold.DevReservation) != p.ReservationHash {
		return invalid("candidate Dev reservation capability unavailable")
	}
	env := environmentByID(st, parent.EnvironmentID)
	if env == nil || env.ProductID != parent.ProductID || env.ReservationEpoch != parent.ReservationEpoch || env.ReservationEpoch != hold.DevReservation.Epoch || selectionToken(*env) != hold.DevReservation.ReservedSelection || p.Request.ExpectedSelection == nil || *p.Request.ExpectedSelection != selectionToken(*env) || env.Cluster != hold.DevReservation.Environment.Cluster || env.Namespace != hold.DevReservation.Environment.Namespace {
		return invalid("candidate Dev reservation selection or target changed")
	}
	candidate := operationByID(st, p.CandidateOperationID)
	if candidate == nil || candidate.ProductID != parent.ProductID || candidate.Status != "SUCCEEDED" || candidate.DeploymentState != "READY_FOR_VERIFICATION" || candidate.MainCandidate == nil || workerconfig.Fingerprint(candidate.MainCandidate) != p.CandidateHash || hold.DevReservation.CandidateOperationID != candidate.ID || hold.DevReservation.CandidateHash != p.CandidateHash {
		return invalid("original main candidate unavailable")
	}
	if err := mainCandidateGuard(st, candidate); err != nil {
		return err
	}
	if parent.ParentID != "" || parent.BuildOnly || parent.Native != nil || parent.Snapshot != nil || parent.MainCandidate != nil || parent.ReleaseCandidateID != "" || len(parent.Sources) != 0 || len(parent.ChildIDs) != len(p.Components) || len(p.Components) != len(candidate.ChildIDs) || len(parent.PreparedDeployments) != len(p.Components) {
		return invalid("candidate Dev aggregate shape changed")
	}
	for i, component := range p.Components {
		origin := operationByID(st, component.OriginalChildID)
		if origin == nil || !slices.Contains(candidate.ChildIDs, origin.ID) || origin.ParentID != candidate.ID || !origin.BuildOnly || origin.Status != "SUCCEEDED" || origin.DeploymentState != "ARTIFACT_READY" || origin.Snapshot == nil || origin.Snapshot.MainCandidate == nil || origin.Artifact == nil || origin.Artifact.Digest != component.Artifact.Digest {
			return invalid("original main build-only artifact unavailable")
		}
		if parent.ChildIDs[i] != component.ChildID || !reflect.DeepEqual(parent.PreparedDeployments[i], component.Snapshot) {
			return invalid("candidate Dev component list changed")
		}
		snap := component.Snapshot
		original := cloneDeliverySnapshot(*origin.Snapshot)
		// Only the Dev destination is substituted; the original Prod effective
		// build and its main-source proof remain byte-for-byte immutable.
		original.Target, original.Environment, original.GitOps, original.GitOpsConnection, original.GitOpsRequest = snap.Target, snap.Environment, snap.GitOps, snap.GitOpsConnection, snap.GitOpsRequest
		if !reflect.DeepEqual(original, snap) || snap.Target.EnvironmentID != env.ID || snap.Target.ApplicationID != origin.ApplicationID || snap.Target.ProductID != parent.ProductID || snap.Target.GitOpsMode != "PR" || !snap.Target.AllowDeploy || snap.Target.Purpose != "DEV" && snap.Target.Purpose != "TEST" || !slices.ContainsFunc(hold.DevReservation.Targets, func(t domain.EnvironmentBinding) bool { return reflect.DeepEqual(t, snap.Target) }) {
			return invalid("candidate Dev delivery scope changed")
		}
		target := environmentBinding(st, env.ID, origin.ApplicationID)
		gitops := repositoryBinding(st, snap.GitOps.RepositoryID)
		if target == nil || !reflect.DeepEqual(*target, snap.Target) || gitops == nil || !reflect.DeepEqual(*gitops, snap.GitOps) || snap.Environment.ID != env.ID || snap.Environment.Cluster != env.Cluster || snap.Environment.Namespace != env.Namespace {
			return invalid("candidate Dev destination changed")
		}
		conn, err := scopedConnection(st, snap.GitOps.ConnectionID, parent.ProductID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(conn.Config, snap.GitOpsConnection) || snap.GitOpsRequest.Repository != repositoryLocator(st, snap.GitOps) || snap.GitOpsRequest.Ref != snap.Target.Ref || snap.GitOpsRequest.Path != snap.Target.Path || snap.GitOpsRequest.ImageField != snap.Target.ImageField || snap.GitOpsRequest.DigestField != snap.Target.DigestField || snap.GitOpsRequest.ImageRepository != snap.Build.ImageRepository {
			return invalid("candidate Dev GitOps authority changed")
		}
		if physical, e := strconv.ParseInt(snap.GitOpsRequest.Repository, 10, 64); e != nil || physical <= 0 {
			return invalid("candidate Dev numeric GitOps repository required")
		}
		known, err := validatedExistingArtifact(st, component.Artifact.ID, parent.ProductID, origin.ApplicationID, snap.Revision.RepositoryID, snap.Revision.HeadCommit, snap.Build.ImageRepository)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(*known, component.Artifact) || known.Native == nil || known.Native.Profile != snap.Build.Profile {
			return invalid("candidate Dev original artifact identity changed")
		}
		child := operationByID(st, component.ChildID)
		if child == nil || child.Status == "FAILED" || child.Status == "CANCELLED" || child.Kind != "DEPLOY" || child.ParentID != parent.ID || child.ProductID != parent.ProductID || child.EnvironmentID != env.ID || child.RequestedBy != parent.RequestedBy || child.ApplicationID != origin.ApplicationID || child.ReservationEpoch != parent.ReservationEpoch || child.BuildOnly || child.Native != nil || child.MainCandidate != nil || child.DevActivation != nil || child.ReleaseCandidateID != "" || child.Run != nil || child.ExistingArtifact == nil || !reflect.DeepEqual(*child.ExistingArtifact, component.Artifact) || child.Snapshot == nil || !reflect.DeepEqual(*child.Snapshot, snap) || child.ExpectedDigest != component.Artifact.Digest || !slices.Contains([]string{"INSPECT", "APPLY", "APPLY_UNCERTAIN", "WAIT_GITOPS_PR"}, child.Phase) {
			return invalid("candidate Dev exact child capability changed")
		}
		if child.Artifact != nil && (!child.Artifact.Available || child.Artifact.Digest != component.Artifact.Digest || child.Artifact.ObservedAt.IsZero()) || child.Phase != "INSPECT" && child.Artifact == nil {
			return invalid("candidate Dev observed artifact changed")
		}
		if child.GitOpsPR != nil && (child.GitOpsPR.Number <= 0 || child.GitOpsPR.URL == "" || !fullSHA.MatchString(child.GitOpsPR.HeadSHA)) {
			return invalid("candidate Dev PR identity incomplete")
		}
		if child.GitOpsPR != nil && (child.DevActivationPRHash == "" || child.DevActivationPRHash != activationPRHash(*child.GitOpsPR)) {
			return invalid("candidate Dev frozen PR proof changed")
		}
		if child.GitOpsResult != nil && (child.GitOpsResult.CommitSHA == "" || child.GitOpsResult.BlobSHA == "" || child.DevActivationResultHash == "" || child.DevActivationResultHash != workerconfig.Fingerprint(child.GitOpsResult)) {
			return invalid("candidate Dev frozen GitOps result changed")
		}
		if child.Status == "SUCCEEDED" && (child.DeploymentState != "GITOPS_APPLIED" || child.GitOpsResult == nil) {
			return invalid("candidate Dev child desired-state result missing")
		}
		if child.Phase == "APPLY_UNCERTAIN" || child.Phase == "WAIT_GITOPS_PR" {
			if child.Artifact == nil || child.GitOpsBase == nil || child.DevActivationBatch == nil || child.DevActivationIntentHash != workerconfig.Fingerprint(child.DevActivationBatch) {
				return invalid("candidate Dev durable GitOps intent changed")
			}
		}
	}
	if op.Kind == "DEPLOY" {
		current := operationByID(st, op.ID)
		if current == nil || !slices.Contains(parent.ChildIDs, op.ID) || current.ParentID != op.ParentID || current.DevActivationGroupID != op.DevActivationGroupID || current.Snapshot == nil || !reflect.DeepEqual(current.Snapshot, op.Snapshot) || current.ExistingArtifact == nil || !reflect.DeepEqual(current.ExistingArtifact, op.ExistingArtifact) || op.BuildOnly || op.Native != nil || op.Run != nil || op.ExpectedDigest != current.ExpectedDigest || op.ReservationEpoch != current.ReservationEpoch {
			return invalid("candidate Dev worker capability changed")
		}
		if op.Artifact != nil && (!op.Artifact.Available || op.Artifact.Digest != current.ExpectedDigest || op.Artifact.ObservedAt.IsZero()) || current.Phase != "INSPECT" && op.Artifact == nil {
			return invalid("candidate Dev leased artifact changed")
		}
		if current.Phase == "APPLY_UNCERTAIN" || current.Phase == "WAIT_GITOPS_PR" {
			if current.GitOpsBase == nil || !reflect.DeepEqual(current.GitOpsBase, op.GitOpsBase) || current.DevActivationIntentHash != op.DevActivationIntentHash || !reflect.DeepEqual(current.DevActivationBatch, op.DevActivationBatch) {
				return invalid("candidate Dev durable GitOps base changed")
			}
		}
		if current.GitOpsPR != nil && (op.GitOpsPR == nil || current.GitOpsPR.Number != op.GitOpsPR.Number || current.GitOpsPR.HeadSHA != op.GitOpsPR.HeadSHA || current.GitOpsPR.URL != op.GitOpsPR.URL) {
			return invalid("candidate Dev frozen PR identity changed")
		}
		if current.GitOpsPR != nil && current.DevActivationPRHash != op.DevActivationPRHash {
			return invalid("candidate Dev leased PR proof changed")
		}
		if op.GitOpsPR != nil && (op.GitOpsPR.Number <= 0 || op.GitOpsPR.URL == "" || !fullSHA.MatchString(op.GitOpsPR.HeadSHA)) {
			return invalid("candidate Dev leased PR identity incomplete")
		}
		if current.GitOpsResult != nil && (!reflect.DeepEqual(current.GitOpsResult, op.GitOpsResult) || current.DevActivationResultHash != op.DevActivationResultHash) {
			return invalid("candidate Dev leased GitOps result changed")
		}
		if op.GitOpsResult != nil && (op.GitOpsResult.CommitSHA == "" || op.GitOpsResult.BlobSHA == "") {
			return invalid("candidate Dev GitOps result evidence incomplete")
		}
	}
	return nil
}

func (s *Service) devActivationAllowed(st *domain.State, op domain.ExternalOperation) error {
	if err := devActivationGuard(st, op); err != nil {
		return err
	}
	parent := devActivationParent(st, op)
	if err := s.activationBaselineAllowed(st, *parent); err != nil {
		return err
	}
	if nativeQuarantined(st, parent.ProductID) {
		return invalid("candidate Dev activation quarantined")
	}
	if err := s.mainCandidateAllowed(st, operationByID(st, parent.DevActivation.CandidateOperationID)); err != nil {
		return err
	}
	for _, component := range parent.DevActivation.Components {
		job := workerJob(st, component.Artifact.Native.PublicationJobID)
		if err := s.historicalPublicationEligible(st, job); err != nil {
			return err
		}
		if !s.workerCheckGate(st, workerJob(st, job.Inputs.Publication.Receipt.JobID)) {
			return invalid("candidate Dev native build check success missing")
		}
	}
	return nil
}

func activationReplay(st *domain.State, c domain.Command, in domain.DevReservationRequest, hash string) (*domain.ExternalOperation, error) {
	for i := range st.Operations {
		op := &st.Operations[i]
		if op.Kind == devBaselineKind && op.RequestKey == in.RequestKey {
			return nil, invalid("request_key already binds a baseline capture")
		}
		if op.Kind == devRuntimeKind && op.RequestKey == in.RequestKey {
			return nil, invalid("request_key already binds a runtime verification")
		}
		if op.ProductID == c.ProductID && op.RequestKey == in.RequestKey && (op.Kind == devActivationKind || op.Kind == "DEV_RESERVATION" || op.Kind == "DEV_RESERVATION_RELEASE") {
			if op.Kind != devActivationKind || op.RequestHash != hash || op.RequestedBy != c.Actor {
				return nil, invalid("request_key already binds another reservation/activation payload or actor")
			}
			return op, nil
		}
	}
	return nil, nil
}

func (s *Service) executeDevActivation(ctx context.Context, c domain.Command) (any, error) {
	in, hash, err := devReservationInput(c)
	if err != nil {
		return nil, err
	}
	if in.ReservationOperationID == "" {
		return nil, invalid("reservation_operation_id required")
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	if old, e := activationReplay(&st, c, in, hash); e != nil {
		return nil, e
	} else if old != nil {
		return *old, nil
	}
	hold := operationByID(&st, in.ReservationOperationID)
	if hold == nil || hold.DevReservation == nil || hold.ProductID != c.ProductID || hold.RequestedBy != c.Actor {
		return nil, invalid("same attributed reservation owner required")
	}
	if err = checkSelection(environmentByID(&st, hold.EnvironmentID), in.ExpectedSelection); err != nil {
		return nil, err
	}
	if _, err = s.currentBaselineForHold(&st, hold); err != nil {
		return nil, err
	}
	candidate := operationByID(&st, hold.DevReservation.CandidateOperationID)
	if candidate == nil {
		return nil, invalid("original main candidate required")
	}
	if err = s.mainCandidateAllowed(&st, candidate); err != nil {
		return nil, err
	}
	if err = activationDestinationsAllowed(&st, candidate, hold.EnvironmentID, c.ProductID); err != nil {
		return nil, err
	}
	ioctx, cancel := context.WithTimeout(delivery.WithCachedAuthentication(ctx), 25*time.Second)
	defer cancel()
	if err = s.observeMainCandidate(ioctx, candidate, false); err != nil {
		return nil, err
	}
	var out domain.ExternalOperation
	err = s.store.Update(ctx, func(st *domain.State) error {
		if old, e := activationReplay(st, c, in, hash); e != nil {
			return e
		} else if old != nil {
			out = *old
			return nil
		}
		hold := operationByID(st, in.ReservationOperationID)
		if hold == nil || hold.DevReservation == nil || hold.ProductID != c.ProductID || hold.RequestedBy != c.Actor {
			return invalid("same attributed reservation owner required")
		}
		for _, op := range st.Operations {
			if op.DevActivation != nil && op.DevActivation.ReservationOperationID == hold.ID || op.Kind == devActivationKind && op.EnvironmentID == hold.EnvironmentID && op.ReservationEpoch == hold.ReservationEpoch {
				return invalid("reservation already has an activation; verified restoration required")
			}
		}
		candidate := operationByID(st, hold.DevReservation.CandidateOperationID)
		if candidate == nil {
			return invalid("original main candidate required")
		}
		if err := s.mainCandidateAllowed(st, candidate); err != nil {
			return err
		}
		env := environmentByID(st, hold.EnvironmentID)
		if env == nil {
			return invalid("reservation environment unavailable")
		}
		if err := checkSelection(env, in.ExpectedSelection); err != nil {
			return err
		}
		m := meta(c)
		baseline, err := s.currentBaselineForHold(st, hold)
		if err != nil {
			return err
		}
		proof := &domain.DevActivationProof{BaselineCaptureID: baseline.ID, BaselineCaptureHash: baselineCaptureHash(*baseline), OriginalBaselineHash: workerconfig.Fingerprint(baseline.DevBaselineResult.Components), Request: in, ReservationOperationID: hold.ID, ReservationHash: workerconfig.Fingerprint(hold.DevReservation), CandidateOperationID: candidate.ID, CandidateHash: workerconfig.Fingerprint(candidate.MainCandidate)}
		out = domain.ExternalOperation{Meta: m, Kind: devActivationKind, RequestedBy: c.Actor, EnvironmentID: env.ID, ReservationEpoch: hold.ReservationEpoch, RequestKey: in.RequestKey, RequestHash: hash, DevActivation: proof, Status: "PENDING", Phase: "WAIT_CHILDREN", CurrentStep: "WAIT_CHILDREN", Detail: "Candidate Dev activation admitted; PR preparation is not deployment or acceptance", NextAttemptAt: m.CreatedAt}
		children := []domain.ExternalOperation{}
		for _, cid := range candidate.ChildIDs {
			origin := operationByID(st, cid)
			if origin == nil || origin.Snapshot == nil || origin.Artifact == nil {
				return invalid("completed candidate artifact required")
			}
			snap := cloneDeliverySnapshot(*origin.Snapshot)
			var artifact *domain.DeliveryArtifact
			for i := range st.DeliveryArtifacts {
				a := &st.DeliveryArtifacts[i]
				if a.ApplicationID == origin.ApplicationID && a.SourceCommit == snap.Revision.HeadCommit && a.Digest == origin.Artifact.Digest && a.ProductID == c.ProductID && a.Native != nil && origin.Native != nil && a.Native.PublicationJobID == origin.Native.PublicationJobID {
					artifact = a
					break
				}
			}
			if artifact == nil {
				return invalid("server-admitted original native artifact required")
			}
			target := environmentBinding(st, env.ID, origin.ApplicationID)
			if target == nil {
				return invalid("reserved Dev target required")
			}
			gitops := repositoryBinding(st, target.RepositoryID)
			if gitops == nil || gitops.ProductID != c.ProductID || gitops.Role != "GITOPS" {
				return invalid("Dev GitOps repository required")
			}
			conn, e := scopedConnection(st, target.ConnectionID, c.ProductID)
			if e != nil {
				return e
			}
			if gitops.ConnectionID != conn.ID {
				return invalid("Dev GitOps connection mismatch")
			}
			snap.Target, snap.Environment, snap.GitOps, snap.GitOpsConnection = *target, *env, *gitops, conn.Config
			snap.GitOpsRequest = delivery.GitOpsRequest{Repository: repositoryLocator(st, *gitops), Ref: target.Ref, Path: target.Path, ImageField: target.ImageField, DigestField: target.DigestField, ImageRepository: snap.Build.ImageRepository}
			cm := m
			cm.ID = id()
			child := domain.ExternalOperation{Meta: cm, Kind: "DEPLOY", ParentID: out.ID, RequestedBy: c.Actor, EnvironmentID: env.ID, ApplicationID: origin.ApplicationID, ReservationEpoch: hold.ReservationEpoch, ExistingArtifact: artifact, Snapshot: &snap, ExpectedDigest: artifact.Digest, Status: "PENDING", Phase: "INSPECT", CurrentStep: "INSPECT", NextAttemptAt: m.CreatedAt}
			children = append(children, child)
			out.ChildIDs = append(out.ChildIDs, child.ID)
			out.PreparedDeployments = append(out.PreparedDeployments, snap)
			proof.Components = append(proof.Components, domain.DevActivationComponent{ChildID: child.ID, OriginalChildID: origin.ID, Artifact: *artifact, Snapshot: snap})
		}
		if err := freezeActivationGroups(proof, children, *baseline); err != nil {
			return err
		}
		proof.SnapshotHash = activationProofHash(*proof)
		st.Operations = append(st.Operations, out)
		st.Operations = append(st.Operations, children...)
		if err := s.devActivationAllowed(st, out); err != nil {
			return err
		}
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: m.CreatedAt, EntityID: out.ID, ProductID: c.ProductID, Data: c})
		return nil
	})
	return out, err
}

func (s *Service) tickDevActivation(ctx context.Context, op domain.ExternalOperation, token string) error {
	ctx = delivery.WithCachedAuthentication(ctx)
	st, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	if err = s.devActivationAllowed(&st, op); err != nil {
		return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
	}
	if !activationHasBaselineEffect(&st, op) {
		if err = s.observeMainCandidate(ctx, operationByID(&st, op.DevActivation.CandidateOperationID), false); err != nil {
			return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
		}
	}
	for _, cid := range op.ChildIDs {
		child := operationByID(&st, cid)
		if child == nil {
			return s.finish(ctx, op, token, "BLOCKED", "candidate Dev child missing", nil)
		}
		if child.Status == "FAILED" || child.Status == "CANCELLED" {
			return s.finish(ctx, op, token, "BLOCKED", "partial candidate Dev activation; reservation retained: "+child.Detail, nil)
		}
		if child.Status != "SUCCEEDED" || child.DeploymentState != "GITOPS_APPLIED" || child.GitOpsResult == nil {
			return s.retry(ctx, op, token, "waiting for exact candidate Dev GitOps results; reservation retained")
		}
	}
	return s.finish(ctx, op, token, "GITOPS_APPLIED", "PENDING_RECONCILIATION: candidate GitOps applied; runtime and acceptance remain unverified, reservation retained", nil)
}

// Final network-boundary revalidation cannot revoke an HTTP call already in
// flight. Failed late saves retain the durable uncertain intent and hold.
func (s *Service) validateDevActivationIO(ctx context.Context, op domain.ExternalOperation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	current := operationByID(&st, op.ID)
	if current == nil || terminalOperation(current.Status) || current.LeaseOwner != op.LeaseOwner || !current.LeaseUntil.After(time.Now().UTC()) {
		return invalid("candidate Dev lease unavailable")
	}
	return s.devActivationAllowed(&st, op)
}
