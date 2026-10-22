package application

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strconv"
	"strings"
	"time"
)

const devBaselineKind = "DEV_CANDIDATE_BASELINE_CAPTURE"

func baselineProofHash(p domain.DevBaselineProof) string {
	p.SnapshotHash = ""
	return workerconfig.Fingerprint(p)
}
func baselineResultHash(r domain.DevBaselineResult) string {
	r.Hash = ""
	return workerconfig.Fingerprint(r)
}
func baselineInput(c domain.Command) (domain.DevBaselineRequest, string, error) {
	var in domain.DevBaselineRequest
	// Reuse the exact actor/Product/CAS parser, with no extra authority fields.
	copy := c
	copy.Action = "release_candidate_dev"
	request, _, err := devReservationInput(copy)
	if err != nil {
		return in, "", err
	}
	in = domain.DevBaselineRequest{ReservationOperationID: request.ReservationOperationID, RequestKey: request.RequestKey, ExpectedSelection: request.ExpectedSelection}
	if in.ReservationOperationID == "" {
		return in, "", invalid("reservation_operation_id required")
	}
	return in, workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevBaselineRequest
	}{c.Action, c.ProductID, c.Actor, in}), nil
}
func baselineReplay(st *domain.State, c domain.Command, in domain.DevBaselineRequest, hash string) (*domain.ExternalOperation, error) {
	for i := range st.Operations {
		v := &st.Operations[i]
		if v.RequestKey != in.RequestKey {
			continue
		}
		if v.Kind != devBaselineKind || v.ProductID != c.ProductID || v.RequestedBy != c.Actor || v.RequestHash != hash {
			return nil, invalid("request_key already binds another baseline scope, owner or payload")
		}
		return v, nil
	}
	return nil, nil
}
func latestBaseline(st *domain.State, hold string) (*domain.ExternalOperation, bool) {
	var out *domain.ExternalOperation
	valid := true
	for i := range st.Operations {
		v := &st.Operations[i]
		if v.Kind != devBaselineKind || v.ParentID != hold {
			continue
		}
		if v.DevBaseline == nil || v.DevBaseline.Generation == 0 {
			valid = false
			continue
		}
		if out == nil || v.DevBaseline.Generation > out.DevBaseline.Generation {
			out = v
		} else if v.DevBaseline.Generation == out.DevBaseline.Generation {
			valid = false
		}
	}
	return out, valid
}
func baselineHasActivation(st *domain.State, hold *domain.ExternalOperation) bool {
	for _, v := range st.Operations {
		if v.DevActivation != nil && v.DevActivation.ReservationOperationID == hold.ID || v.Kind == devActivationKind && v.EnvironmentID == hold.EnvironmentID && v.ReservationEpoch == hold.ReservationEpoch {
			return true
		}
	}
	return false
}
func (s *Service) baselineExpected(st *domain.State, hold *domain.ExternalOperation, in domain.DevBaselineRequest, generation uint64) (domain.DevBaselineProof, error) {
	p := domain.DevBaselineProof{Request: in, Generation: generation}
	if hold == nil || hold.DevReservation == nil || hold.Kind != "DEV_RESERVATION" || hold.Status != "SUCCEEDED" || hold.Phase != "RESERVED" || hold.ReservationQuarantined || nativeQuarantined(st, hold.ProductID) {
		return p, invalid("active nonquarantined reservation required")
	}
	active := activeDevReservation(st, hold.EnvironmentID)
	env := environmentByID(st, hold.EnvironmentID)
	if active == nil || active.ID != hold.ID || env == nil || env.ProductID != hold.ProductID || env.ReservationEpoch != hold.ReservationEpoch || hold.DevReservation.Epoch != hold.ReservationEpoch || selectionToken(*env) != hold.DevReservation.ReservedSelection || env.Cluster != hold.DevReservation.Environment.Cluster || env.Namespace != hold.DevReservation.Environment.Namespace || in.ExpectedSelection == nil || *in.ExpectedSelection != selectionToken(*env) {
		return p, invalid("baseline hold, epoch or exact selection changed")
	}
	comp := compositionByID(st, hold.DevReservation.BaselineSelection.CompositionID)
	if comp == nil || comp.ID != env.DesiredCompositionID || comp.ProductID != hold.ProductID || comp.EnvironmentID != env.ID || comp.EnvironmentSnapshot.Cluster != env.Cluster || comp.EnvironmentSnapshot.Namespace != env.Namespace {
		return p, invalid("reserved full shared baseline unavailable")
	}
	if _, ok := s.provider.(delivery.GitOpsRevisionVerifier); !ok {
		return p, invalid("read-only GitOps revision verifier required")
	}
	p.ReservationOperationID = hold.ID
	p.ReservationHash = workerconfig.Fingerprint(hold.DevReservation)
	p.CompositionID = comp.ID
	p.CompositionHash = workerconfig.Fingerprint(comp)
	roles := 0
	apps := map[string]bool{}
	targets := map[string]bool{}
	workloads := map[string]bool{}
	for _, part := range comp.Components {
		if apps[part.ApplicationID] {
			return p, invalid("duplicate baseline component")
		}
		apps[part.ApplicationID] = true
		app := applicationByID(st, part.ApplicationID)
		build := componentBuild(st, part.ApplicationID)
		target := environmentBinding(st, env.ID, part.ApplicationID)
		if app == nil || app.ProductID != hold.ProductID || domain.ComponentKind(*app) != "APPLICATION" || build == nil || target == nil || target.ProductID != hold.ProductID || target.GitOpsMode != "PR" || !target.AllowDeploy || target.Purpose != "DEV" && target.Purpose != "TEST" {
			return p, invalid("complete current baseline component profile and PR-only Dev target required")
		}
		image, err := domain.CanonicalImageRepository(build.ImageRepository)
		if err != nil || image != build.ImageRepository {
			return p, invalid("exact baseline Dev image selector required")
		}
		gitops := repositoryBinding(st, target.RepositoryID)
		if gitops == nil || gitops.ProductID != hold.ProductID || gitops.Role != "GITOPS" || gitops.ConnectionID != target.ConnectionID {
			return p, invalid("baseline GitOps authority unavailable")
		}
		connection, err := scopedConnection(st, target.ConnectionID, hold.ProductID)
		if err != nil {
			return p, err
		}
		repository := repositoryLocator(st, *gitops)
		if number, e := strconv.ParseInt(repository, 10, 64); e != nil || number <= 0 {
			return p, invalid("numeric baseline GitOps repository required")
		}
		authority, err := runtimeGitOpsAuthority(connection.Config)
		if err != nil {
			return p, err
		}
		request := delivery.GitOpsRequest{Repository: repository, Ref: target.Ref, Path: target.Path, ImageRepository: image, ImageField: target.ImageField, DigestField: target.DigestField}
		key := authority + "\x00" + repository + "\x00" + target.Ref + "\x00" + target.Path + "\x00" + target.ImageField + "\x00" + target.DigestField + "\x00" + image
		if targets[key] {
			return p, invalid("overlapping baseline target selector")
		}
		targets[key] = true
		scopes, err := s.runtimeCoverage(hold.ProductID, env.ID, app.ID, env.Cluster, env.Namespace)
		if err != nil {
			return p, err
		}
		roles += len(scopes)
		for _, scope := range scopes {
			key := runtimeRoleKey(scope)
			if workloads[key] {
				return p, invalid("overlapping baseline runtime roles")
			}
			workloads[key] = true
		}
		config := workerconfig.Fingerprint(struct {
			App                domain.Application
			Build              domain.ComponentBuild
			Target             domain.EnvironmentBinding
			Repository         domain.RepositoryBinding
			Connection         delivery.Connection
			Cluster, Namespace string
		}{*app, *build, *target, *gitops, connection.Config, env.Cluster, env.Namespace})
		p.Components = append(p.Components, domain.DevBaselineComponent{ApplicationID: app.ID, RepositoryID: app.RepositoryID, ConnectionID: connection.ID, ConfigHash: config, AuthorityHash: authority, ImageRepository: image, GitOps: request, Scopes: scopes})
	}
	if len(p.Components) == 0 || len(p.Components) > 16 || roles > 64 {
		return p, invalid("complete baseline coverage exceeds component/role bounds")
	}
	for _, target := range hold.DevReservation.Targets {
		if !apps[target.ApplicationID] {
			return p, invalid("candidate component missing from full baseline")
		}
		current := environmentBinding(st, env.ID, target.ApplicationID)
		if current == nil || !reflect.DeepEqual(*current, target) {
			return p, invalid("reserved candidate target changed")
		}
	}
	p.SnapshotHash = baselineProofHash(p)
	if !baselineSize(p, nil) {
		return p, invalid("baseline proof bound exceeded")
	}
	return p, nil
}
func baselineSize(p domain.DevBaselineProof, r *domain.DevBaselineResult) bool {
	if r == nil {
		r = &domain.DevBaselineResult{ExpectedHash: p.SnapshotHash, Outcome: "UNVERIFIED", Reason: strings.Repeat("x", 64), ObservedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), Hash: "sha256:" + strings.Repeat("0", 64)}
	}
	b, e := json.Marshal(struct {
		Proof  domain.DevBaselineProof
		Result *domain.DevBaselineResult
	}{p, r})
	return e == nil && len(b) <= devRuntimeMaxBytes
}
func clearBaselinePhysical(c domain.DevBaselineComponent) domain.DevBaselineComponent {
	c.Digest = ""
	c.ObservedGitOps = nil
	c.Revision = nil
	c.ArtifactID = ""
	c.ArtifactHash = ""
	c.RegistryJobID = ""
	c.RegistryInputHash = ""
	c.RegistryRootSetHash = ""
	return c
}
func (s *Service) baselineAllowed(st *domain.State, op domain.ExternalOperation, latest, beforeActivation bool) error {
	current := operationByID(st, op.ID)
	p := op.DevBaseline
	if current == nil || p == nil || current.DevBaseline == nil || op.Kind != devBaselineKind || current.Kind != op.Kind || current.ParentID != op.ParentID || current.ProductID != op.ProductID || current.RequestedBy != op.RequestedBy || current.EnvironmentID != op.EnvironmentID || current.ReservationEpoch != op.ReservationEpoch || !reflect.DeepEqual(current.ChildIDs, op.ChildIDs) || op.ReservationQuarantined || current.ReservationQuarantined || op.ParentID != p.ReservationOperationID || p.Request.ReservationOperationID != p.ReservationOperationID || op.RequestKey != p.Request.RequestKey || op.RequestHash != current.RequestHash || !reflect.DeepEqual(*current.DevBaseline, *p) || p.Generation == 0 || p.SnapshotHash == "" || p.SnapshotHash != baselineProofHash(*p) || op.Native != nil || op.DevActivation != nil || op.MainCandidate != nil || op.DevRuntime != nil || op.Snapshot != nil || op.BuildOnly || len(op.Sources) != 0 {
		return invalid("immutable baseline capture proof unavailable")
	}
	hash := workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevBaselineRequest
	}{"capture_candidate_dev_baseline", op.ProductID, op.RequestedBy, p.Request})
	if hash != op.RequestHash {
		return invalid("baseline request proof changed")
	}
	hold := operationByID(st, p.ReservationOperationID)
	if hold == nil || hold.ProductID != op.ProductID || hold.RequestedBy != op.RequestedBy || hold.EnvironmentID != op.EnvironmentID || hold.ReservationEpoch != op.ReservationEpoch {
		return invalid("baseline owner or reservation changed")
	}
	if beforeActivation {
		if baselineHasActivation(st, hold) {
			return invalid("baseline capture forbidden after activation admission")
		}
		if err := unresolvedReservationEffects(st, op.EnvironmentID); err != nil {
			return err
		}
	}
	expected, e := s.baselineExpected(st, hold, p.Request, p.Generation)
	if e != nil {
		return e
	}
	copy := *p
	copy.Components = append([]domain.DevBaselineComponent(nil), p.Components...)
	for i, c := range copy.Components {
		copy.Components[i] = clearBaselinePhysical(c)
	}
	copy.SnapshotHash = baselineProofHash(copy)
	if !reflect.DeepEqual(copy, expected) {
		return invalid("baseline configuration or coverage changed")
	}
	expectedChildren := []string{}
	for _, c := range p.Components {
		if c.Digest == "" {
			if c.ObservedGitOps != nil || c.Revision != nil || c.ArtifactID != "" || c.RegistryJobID != "" {
				return invalid("partial staged baseline proof")
			}
			continue
		}
		if !validArtifactDigest(c.Digest) || c.ObservedGitOps == nil || c.Revision == nil || c.ObservedGitOps.ImageRepository != c.ImageRepository || c.ObservedGitOps.Digest != c.Digest || !fullSHA.MatchString(c.ObservedGitOps.HeadSHA) || !fullSHA.MatchString(c.ObservedGitOps.BlobSHA) || !workerconfig.ValidFingerprint(c.ObservedGitOps.ContentHash) || !validRevisionProof(delivery.GitOpsRevisionRequest{Repository: c.GitOps.Repository, Ref: c.GitOps.Ref, HeadSHA: c.ObservedGitOps.HeadSHA, AncestorSHAs: []string{c.ObservedGitOps.HeadSHA}}, *c.Revision) {
			return invalid("baseline physical evidence changed")
		}
		if c.ArtifactID != "" {
			if _, err := s.baselineArtifact(st, op.ProductID, c); err != nil {
				return err
			}
			if c.RegistryJobID != "" || c.RegistryInputHash != "" || c.RegistryRootSetHash != "" {
				return invalid("mixed baseline availability authority")
			}
		} else if c.RegistryJobID != "" {
			job, err := s.baselineRegistryJob(st, op, c)
			if err != nil {
				return err
			}
			expectedChildren = append(expectedChildren, job.OperationID)
		}
	}
	if len(op.ChildIDs) != len(expectedChildren) {
		return invalid("baseline causal registry child set changed")
	}
	for i, id := range expectedChildren {
		if op.ChildIDs[i] != id {
			return invalid("baseline causal registry child order changed")
		}
	}
	if latest {
		v, ok := latestBaseline(st, hold.ID)
		if !ok || v == nil || v.ID != op.ID {
			return invalid("baseline capture superseded or damaged")
		}
	}
	return nil
}
func (s *Service) executeDevBaseline(ctx context.Context, c domain.Command) (any, error) {
	in, hash, e := baselineInput(c)
	if e != nil {
		return nil, e
	}
	var out domain.ExternalOperation
	e = s.store.Update(ctx, func(st *domain.State) error {
		if old, e := baselineReplay(st, c, in, hash); e != nil {
			return e
		} else if old != nil {
			out = *old
			return nil
		}
		hold := operationByID(st, in.ReservationOperationID)
		if hold == nil || hold.ProductID != c.ProductID || hold.RequestedBy != c.Actor || baselineHasActivation(st, hold) {
			return invalid("same active preactivation reservation owner required")
		}
		generation := uint64(1)
		for _, op := range st.Operations {
			if op.Kind == devBaselineKind && op.ParentID == hold.ID {
				if op.DevBaseline == nil || op.DevBaseline.Generation == 0 || op.DevBaseline.Generation == math.MaxUint64 {
					return invalid("baseline generation unavailable or exhausted")
				}
				if op.DevBaseline.Generation >= generation {
					generation = op.DevBaseline.Generation + 1
				}
			}
		}
		if err := unresolvedReservationEffects(st, hold.EnvironmentID); err != nil {
			return err
		}
		p, err := s.baselineExpected(st, hold, in, generation)
		if err != nil {
			return err
		}
		m := meta(c)
		out = domain.ExternalOperation{Meta: m, Kind: devBaselineKind, ParentID: hold.ID, EnvironmentID: hold.EnvironmentID, ReservationEpoch: hold.ReservationEpoch, RequestedBy: c.Actor, RequestKey: in.RequestKey, RequestHash: hash, DevBaseline: &p, Status: "PENDING", Phase: "CAPTURE_GITOPS", CurrentStep: "CAPTURE_GITOPS", NextAttemptAt: m.CreatedAt, Detail: "Read-only full shared baseline capture pending; no candidate effects permitted"}
		st.Operations = append(st.Operations, out)
		st.Events = append(st.Events, domain.Event{ID: id(), Actor: c.Actor, Action: c.Action, At: m.CreatedAt, ProductID: c.ProductID, EntityID: out.ID, Data: c})
		return nil
	})
	return out, e
}
func (s *Service) validateBaselineIO(ctx context.Context, op domain.ExternalOperation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	current := operationByID(&st, op.ID)
	if current == nil || terminalOperation(current.Status) || current.LeaseOwner != op.LeaseOwner || !current.LeaseUntil.After(time.Now().UTC()) {
		return invalid("baseline capture lease unavailable")
	}
	return s.baselineAllowed(&st, op, true, true)
}
func (s *Service) baselineQuery(st *domain.State, env string) map[string]any {
	out := map[string]any{"baseline_capture_current_valid": false}
	hold := activeDevReservation(st, env)
	if hold == nil {
		return out
	}
	op, valid := latestBaseline(st, hold.ID)
	if op == nil {
		return out
	}
	out["baseline_capture_id"] = op.ID
	out["baseline_capture_status"] = op.Status
	if op.DevBaselineResult != nil {
		out["baseline_capture_observed_at"] = op.DevBaselineResult.ObservedAt
	}
	if valid && s.baselineCurrent(st, *op, true) == nil {
		out["baseline_capture_current_valid"] = true
	}
	return out
}
func (s *Service) baselineCurrent(st *domain.State, op domain.ExternalOperation, beforeActivation bool) error {
	if op.DevBaseline == nil || op.Status != "SUCCEEDED" || op.Phase != "BASELINE_CAPTURED" || op.DeploymentState != "BASELINE_CAPTURED" || op.DevBaselineResult == nil || op.DevBaselineResult.Outcome != "CAPTURED" || op.DevBaselineResult.ExpectedHash != op.DevBaseline.SnapshotHash || op.DevBaselineResult.Hash != baselineResultHash(*op.DevBaselineResult) || op.DevBaselineResult.ObservedAt.IsZero() || op.DevBaselineResult.ObservedAt.After(time.Now().UTC()) || time.Since(op.DevBaselineResult.ObservedAt) > 5*time.Minute {
		return invalid("latest successful fresh baseline capture required")
	}
	if err := s.baselineAllowed(st, op, true, beforeActivation); err != nil {
		return err
	}
	return s.validateBaselineResult(st, op, *op.DevBaselineResult)
}
