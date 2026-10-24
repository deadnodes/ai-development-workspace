package application

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

var errRestorationUnapprovedMerge = errors.New("unapproved external restoration merge before attributed confirmation")

const devRestorationKind = "DEV_CANDIDATE_RESTORATION"
const devRestorationConfirmationKind = "DEV_RESTORATION_CONFIRMATION"
const devRestorationVerificationKind = "DEV_RESTORATION_VERIFICATION"
const devRestorationReleaseKind = "DEV_RESTORATION_RELEASE"

func restorationProofHash(p domain.DevRestorationProof) string {
	p.SnapshotHash = ""
	return workerconfig.Fingerprint(p)
}
func restorationPlanHash(p domain.DevRestorationPlan) string {
	p.Hash = ""
	p.Groups = append([]domain.DevRestorationGroup(nil), p.Groups...)
	for i := range p.Groups {
		p.Groups[i].Result = nil
		p.Groups[i].MergedAt = nil
	}
	return workerconfig.Fingerprint(p)
}
func restorationConfirmationHash(p domain.DevRestorationConfirmation) string {
	p.Hash = ""
	return workerconfig.Fingerprint(p)
}
func restorationVerificationHash(p domain.DevRestorationVerification) string {
	p.Hash = ""
	return workerconfig.Fingerprint(p)
}
func restorationReleaseHash(p domain.DevRestorationRelease) string {
	p.Hash = ""
	return workerconfig.Fingerprint(p)
}
func restorationFits(v any) bool { b, e := json.Marshal(v); return e == nil && len(b)+4096 <= 256*1024 }
func restorationAction(action string) bool {
	return slices.Contains([]string{"request_candidate_dev_restoration", "confirm_candidate_dev_restoration", "verify_candidate_dev_restoration", "release_candidate_dev_after_restoration"}, action)
}
func restorationKind(action string) string {
	switch action {
	case "request_candidate_dev_restoration":
		return devRestorationKind
	case "confirm_candidate_dev_restoration":
		return devRestorationConfirmationKind
	case "verify_candidate_dev_restoration":
		return devRestorationVerificationKind
	case "release_candidate_dev_after_restoration":
		return devRestorationReleaseKind
	}
	return ""
}
func restorationInput(c domain.Command) (domain.DevRestorationRequest, string, error) {
	var in domain.DevRestorationRequest
	if !restorationAction(c.Action) || c.ProductID == "" || strings.TrimSpace(c.Actor) == "" || c.ID != "" || c.FeatureID != "" || c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return in, "", invalid("restoration requires attributed actor and Product scope only")
	}
	allowed := map[string]bool{"request_key": true, "expected_selection": true}
	if c.Action == "request_candidate_dev_restoration" {
		allowed["reservation_operation_id"] = true
	} else {
		allowed["restoration_operation_id"] = true
	}
	if c.Action == "confirm_candidate_dev_restoration" {
		allowed["expected_plan_hash"] = true
	}
	if c.Action == "release_candidate_dev_after_restoration" {
		allowed["restoration_verification_operation_id"] = true
	}
	for k := range c.Data {
		if !allowed[k] {
			return in, "", invalid("unknown restoration field %s", k)
		}
	}
	if e := decode(c.Data, &in); e != nil {
		return in, "", e
	}
	if len(in.RequestKey) < 16 || len(in.RequestKey) > 128 || strings.TrimSpace(in.RequestKey) != in.RequestKey || strings.ContainsFunc(in.RequestKey, func(r rune) bool { return r < 32 || r == 127 }) || in.ExpectedSelection == nil {
		return in, "", invalid("exact request_key16..128 and selection required")
	}
	b, _ := json.Marshal(c.Data["expected_selection"])
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(b, &keys)
	if len(keys) != 3 {
		return in, "", invalid("exact selection fields required")
	}
	for _, k := range []string{"composition_id", "operation_id", "generation"} {
		if v, ok := keys[k]; !ok || string(v) == "null" {
			return in, "", invalid("exact selection fields required")
		}
	}
	for k := range allowed {
		if k == "request_key" || k == "expected_selection" {
			continue
		}
		v, ok := c.Data[k].(string)
		if !ok || v == "" || len(v) > 128 {
			return in, "", invalid("exact %s required", k)
		}
	}
	if in.ExpectedPlanHash != "" && !workerconfig.ValidFingerprint(in.ExpectedPlanHash) {
		return in, "", invalid("exact plan hash required")
	}
	return in, workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevRestorationRequest
	}{c.Action, c.ProductID, c.Actor, in}), nil
}
func restorationReplay(st *domain.State, c domain.Command, in domain.DevRestorationRequest, hash string) (*domain.ExternalOperation, error) {
	for i := range st.Operations {
		o := &st.Operations[i]
		if o.ProductID != c.ProductID || o.RequestKey != in.RequestKey {
			continue
		}
		if o.Kind != restorationKind(c.Action) || o.RequestedBy != c.Actor || o.RequestHash != hash {
			return nil, invalid("request_key already binds another Product receipt payload or actor")
		}
		return o, nil
	}
	return nil, nil
}
func latestRestoration(st *domain.State, hold string) (*domain.ExternalOperation, bool) {
	var out *domain.ExternalOperation
	valid := true
	for i := range st.Operations {
		o := &st.Operations[i]
		if o.Kind != devRestorationKind || o.ParentID != hold {
			continue
		}
		if o.DevRestoration == nil || o.DevRestoration.Generation == 0 {
			valid = false
			continue
		}
		if out == nil || o.DevRestoration.Generation > out.DevRestoration.Generation {
			out = o
		} else if o.DevRestoration.Generation == out.DevRestoration.Generation {
			valid = false
		}
	}
	return out, valid
}

// Reconstruct only the historical fence in an in-memory copy to compare the
// current configuration with the original capture. No admitted record is edited.
func (s *Service) restorationOriginal(st *domain.State, hold *domain.ExternalOperation) (*domain.ExternalOperation, error) {
	if hold == nil || hold.Kind != "DEV_RESERVATION" || hold.DevReservation == nil || hold.Status != "SUCCEEDED" || hold.Phase != "RESERVED" || hold.ReservationQuarantined || nativeQuarantined(st, hold.ProductID) {
		return nil, invalid("active original reservation required")
	}
	capture, ok := latestBaseline(st, hold.ID)
	if !ok || capture == nil || capture.DevBaseline == nil || capture.DevBaselineResult == nil || capture.Status != "SUCCEEDED" || capture.Phase != "BASELINE_CAPTURED" || capture.ReservationQuarantined || capture.ProductID != hold.ProductID || capture.RequestedBy != hold.RequestedBy || capture.EnvironmentID != hold.EnvironmentID || capture.ReservationEpoch != hold.ReservationEpoch || capture.DevBaseline.SnapshotHash != baselineProofHash(*capture.DevBaseline) || capture.DevBaseline.ReservationHash != workerconfig.Fingerprint(hold.DevReservation) {
		return nil, invalid("immutable complete original baseline required")
	}
	clone := *st
	clone.Environments = append([]domain.Environment(nil), st.Environments...)
	env := environmentByID(&clone, hold.EnvironmentID)
	if env == nil {
		return nil, invalid("original environment missing")
	}
	env.ReservationEpoch = hold.ReservationEpoch
	env.SelectionGeneration = hold.DevReservation.ReservedSelection.Generation
	expected, e := s.baselineExpected(&clone, hold, capture.DevBaseline.Request, capture.DevBaseline.Generation)
	if e != nil {
		return nil, e
	}
	copy := *capture.DevBaseline
	copy.Components = append([]domain.DevBaselineComponent(nil), copy.Components...)
	for i, c := range copy.Components {
		copy.Components[i] = clearBaselinePhysical(c)
	}
	copy.SnapshotHash = baselineProofHash(copy)
	if !reflect.DeepEqual(copy, expected) {
		return nil, invalid("original configuration or complete observer coverage changed")
	}
	if e = s.validateBaselineResult(st, *capture, *capture.DevBaselineResult); e != nil {
		return nil, e
	}
	candidate := operationByID(st, hold.DevReservation.CandidateOperationID)
	if candidate == nil || candidate.MainCandidate == nil || workerconfig.Fingerprint(candidate.MainCandidate) != hold.DevReservation.CandidateHash {
		return nil, invalid("original candidate structural identity changed")
	}
	// Candidate liveness/checks/author-branch are intentionally not consulted.
	for _, a := range st.Operations {
		if a.DevActivation == nil || a.DevActivation.ReservationOperationID != hold.ID {
			continue
		}
		p := a.DevActivation
		if a.ProductID != hold.ProductID || a.RequestedBy != hold.RequestedBy || a.EnvironmentID != hold.EnvironmentID || p.SnapshotHash != activationProofHash(*p) || p.ReservationHash != workerconfig.Fingerprint(hold.DevReservation) || p.BaselineCaptureID != capture.ID || p.BaselineCaptureHash != baselineCaptureHash(*capture) || p.OriginalBaselineHash != workerconfig.Fingerprint(capture.DevBaselineResult.Components) || p.CandidateHash != hold.DevReservation.CandidateHash {
			return nil, invalid("original activation/capture relation changed")
		}

		if p.CandidateOperationID != candidate.ID || !reflect.DeepEqual(hold.DevReservation.CandidateProof, *candidate.MainCandidate) || len(p.Components) != len(candidate.ChildIDs) {
			return nil, invalid("original selected candidate membership changed")
		}
		for _, g := range p.Groups {
			for _, cid := range g.MemberIDs {
				child := operationByID(st, cid)
				if child == nil || cid != g.LeaderID && child.ReservationMutationIntent != "" {
					return nil, invalid("unknown original follower mutation intent")
				}
			}
		}
		for _, member := range p.Components {
			origin := operationByID(st, member.OriginalChildID)
			if origin == nil || origin.ParentID != candidate.ID || !slices.Contains(candidate.ChildIDs, origin.ID) || origin.ProductID != hold.ProductID || origin.ApplicationID != member.Artifact.ApplicationID || !origin.BuildOnly || origin.Status != "SUCCEEDED" || origin.DeploymentState != "ARTIFACT_READY" || origin.Snapshot == nil || origin.Artifact == nil || origin.Artifact.Digest != member.Artifact.Digest {
				return nil, invalid("original admitted native build-only artifact changed")
			}
			known, e := validatedExistingArtifact(st, member.Artifact.ID, hold.ProductID, origin.ApplicationID, origin.Snapshot.Revision.RepositoryID, origin.Snapshot.Revision.HeadCommit, origin.Snapshot.Build.ImageRepository)
			if e != nil || !reflect.DeepEqual(known, &member.Artifact) {
				return nil, invalid("original admitted artifact identity changed")
			}
		}
		if e = validateActivationGroups(st, &a); e != nil {
			return nil, e
		}
	}
	// Full baseline physical aliases must agree before any provider IO.
	physical := map[string]delivery.Connection{}
	selectors := map[string]bool{}
	for _, c := range capture.DevBaseline.Components {
		conn, e := scopedConnection(st, c.ConnectionID, hold.ProductID)
		if e != nil {
			return nil, e
		}
		key := activationPhysicalKey(conn.Config, c.GitOps.Repository, c.GitOps.Ref)
		if prior, ok := physical[key]; ok && !reflect.DeepEqual(prior, conn.Config) {
			return nil, invalid("physical restoration aliases disagree")
		}
		physical[key] = conn.Config
		selector := key + "|" + c.GitOps.Path + "|" + c.GitOps.ImageField + "|" + c.GitOps.DigestField + "|" + c.ImageRepository
		if selectors[selector] {
			return nil, invalid("overlapping original restoration selectors")
		}
		selectors[selector] = true
	}
	return capture, nil
}
func restorationEffects(st *domain.State, hold *domain.ExternalOperation) ([]domain.DevRestorationEffect, error) {
	effects := []domain.DevRestorationEffect{}
	attempts := 0
	seen := map[string]bool{}
	for _, o := range st.Operations {
		if o.EnvironmentID != hold.EnvironmentID || o.ProductID != hold.ProductID {
			continue
		}
		if o.DevActivation != nil && o.DevActivation.ReservationOperationID == hold.ID {
			for _, g := range o.DevActivation.Groups {
				leader := operationByID(st, g.LeaderID)
				if leader == nil || seen[leader.ID] {
					return nil, invalid("original effect leader missing or duplicate")
				}
				seen[leader.ID] = true
				no := leader.DevActivationBatch == nil && leader.ReservationMutationIntent == "" && leader.GitOpsPR == nil && leader.GitOpsResult == nil
				if !no && (leader.DevActivationBatch == nil || leader.DevActivationIntentHash != workerconfig.Fingerprint(leader.DevActivationBatch)) {
					return nil, invalid("unknown original durable mutation intent")
				}
				effects = append(effects, domain.DevRestorationEffect{OperationID: leader.ID, Connection: g.Connection, Batch: leader.DevActivationBatch, PullRequest: leader.GitOpsPR, NoIntent: no})
			}
		}
		if o.Kind == devRestorationKind && o.ParentID == hold.ID {
			attempts++
			if attempts >= 256 {
				return nil, invalid("recovery attempt history exhausted")
			}
		}
		if o.DevRestoration != nil && o.DevRestoration.ReservationOperationID == hold.ID && o.DevRestorationPlan != nil {
			for _, g := range o.DevRestorationPlan.Groups {
				if g.Batch == nil || !g.SendStarted {
					continue
				}
				if seen[g.LeaderID] {
					return nil, invalid("duplicate recovery effect")
				}
				seen[g.LeaderID] = true
				effects = append(effects, domain.DevRestorationEffect{OperationID: g.LeaderID, Connection: g.Connection, Batch: g.Batch, PullRequest: g.PullRequest})
			}
		}
		if len(effects) > 256 {
			return nil, invalid("restoration effect history exceeds256")
		}
	}
	if !restorationFits(effects) {
		return nil, invalid("restoration effect authority exceeds bound")
	}
	return effects, nil
}
func (s *Service) restorationAllowed(st *domain.State, op domain.ExternalOperation, write bool) error {
	current := operationByID(st, op.ID)
	p := op.DevRestoration
	if current == nil || p == nil || current.DevRestoration == nil || op.Kind != devRestorationKind || op.ParentID != p.ReservationOperationID || op.ProductID != current.ProductID || op.RequestedBy != current.RequestedBy || op.EnvironmentID != current.EnvironmentID || op.ReservationEpoch != current.ReservationEpoch || op.RequestHash != current.RequestHash || op.ReservationQuarantined || current.ReservationQuarantined || op.DevRestorationUnapproved || current.DevRestorationUnapproved || current.Status == "FAILED" || current.Status == "CANCELLED" || !reflect.DeepEqual(p, current.DevRestoration) || p.SnapshotHash != restorationProofHash(*p) || p.Epoch != op.ReservationEpoch || p.Generation == 0 || op.Native != nil || op.Snapshot != nil || op.MainCandidate != nil || op.DevActivation != nil || op.BuildOnly {
		return invalid("immutable recovery authority unavailable")
	}
	expectedHash := workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevRestorationRequest
	}{"request_candidate_dev_restoration", op.ProductID, op.RequestedBy, p.Request})
	if op.RequestHash != expectedHash || op.RequestKey != p.Request.RequestKey {
		return invalid("recovery request identity changed")
	}
	hold := activeDevReservation(st, op.EnvironmentID)
	if hold == nil || hold.ID != p.ReservationOperationID || hold.ProductID != op.ProductID || hold.RequestedBy != op.RequestedBy || workerconfig.Fingerprint(hold.DevReservation) != p.ReservationHash {
		return invalid("same active recovery hold required")
	}
	latest, ok := latestRestoration(st, hold.ID)
	if !ok || latest == nil || latest.ID != op.ID {
		return invalid("recovery generation superseded")
	}
	env := environmentByID(st, op.EnvironmentID)
	if env == nil || env.ReservationEpoch != p.Epoch || selectionToken(*env) != p.Selection {
		return invalid("recovery epoch or selection changed")
	}
	capture, e := s.restorationOriginal(st, hold)
	if e != nil {
		return e
	}
	if capture.ID != p.BaselineCaptureID || baselineCaptureHash(*capture) != p.BaselineCaptureHash || len(p.Components) != len(capture.DevBaseline.Components) {
		return invalid("original recovery capture changed")
	}
	for i, c := range p.Components {
		original := capture.DevBaseline.Components[i]
		c.RegistryJobID = ""
		c.RegistryInputHash = ""
		c.RegistryRootSetHash = ""
		original.RegistryJobID = ""
		original.RegistryInputHash = ""
		original.RegistryRootSetHash = ""
		if !reflect.DeepEqual(c, original) {
			return invalid("recovery original component changed")
		}
	}

	if len(p.Effects) > 256 {
		return invalid("recovery effect history exceeds256")
	}
	seenEffects := map[string]bool{}
	for _, effect := range p.Effects {
		if effect.OperationID == "" || seenEffects[effect.OperationID] || effect.NoIntent && (effect.Batch != nil || effect.PullRequest != nil) || !effect.NoIntent && effect.Batch == nil {
			return invalid("recovery effect set malformed or duplicate")
		}
		seenEffects[effect.OperationID] = true
	}
	if e := validateRestorationPlan(*current); e != nil {
		return e
	}
	if !restorationFits(struct {
		P            *domain.DevRestorationProof
		Plan         *domain.DevRestorationPlan
		Availability []domain.DevBaselineComponentResult
	}{p, current.DevRestorationPlan, current.DevRestorationAvailability}) {
		return invalid("recovery authority bound exceeded")
	}
	if write && (s.restorationWriteNow().After(p.Deadline) || p.Deadline.After(op.CreatedAt.Add(time.Hour)) || p.Deadline.Before(op.CreatedAt)) {
		return invalid("restoration-only write authority expired")
	}
	return nil
}
func (s *Service) validateRestorationIO(ctx context.Context, op domain.ExternalOperation, token string, write bool) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	st, e := s.store.Read(ctx)
	if e != nil {
		return e
	}
	current := operationByID(&st, op.ID)
	if current == nil || current.LeaseOwner != token || !current.LeaseUntil.After(time.Now().UTC()) {
		return invalid("recovery leader lease lost")
	}
	if e = s.restorationAllowed(&st, op, write); e != nil {
		return e
	}
	if !reflect.DeepEqual(current.DevRestorationPlan, op.DevRestorationPlan) {
		return invalid("recovery plan or intent changed")
	}
	return nil
}
func (s *Service) executeDevRestoration(ctx context.Context, c domain.Command) (any, error) {
	in, hash, e := restorationInput(c)
	if e != nil {
		return nil, e
	}
	st, e := s.store.Read(ctx)
	if e != nil {
		return nil, e
	}
	if old, e := restorationReplay(&st, c, in, hash); e != nil {
		return nil, e
	} else if old != nil {
		return *old, nil
	}
	if c.Action != "request_candidate_dev_restoration" {
		return s.executeRestorationSuccessor(ctx, c, in, hash)
	}
	hold := operationByID(&st, in.ReservationOperationID)
	if hold == nil || hold.ProductID != c.ProductID || hold.RequestedBy != c.Actor {
		return nil, invalid("same attributed reservation owner required")
	}
	// Renewal requires current, exact GET-only settlement first; replay never IO.
	if old, ok := latestRestoration(&st, hold.ID); !ok {
		return nil, invalid("damaged earlier recovery")
	} else if old != nil {
		if old.LeaseUntil.After(time.Now().UTC()) {
			return nil, invalid("earlier recovery in flight")
		}
		if _, e = s.reconcileRestorationEffects(delivery.WithCachedAuthentication(ctx), &st, *old, true); e != nil {
			return nil, e
		}
	}
	priorHash := ""
	if prior, _ := latestRestoration(&st, hold.ID); prior != nil {
		priorHash = workerconfig.Fingerprint(prior)
	}
	var out domain.ExternalOperation
	e = s.store.Update(ctx, func(st *domain.State) error {
		if old, e := restorationReplay(st, c, in, hash); e != nil {
			return e
		} else if old != nil {
			out = *old
			return nil
		}
		hold := operationByID(st, in.ReservationOperationID)
		if hold == nil || hold.ProductID != c.ProductID || hold.RequestedBy != c.Actor {
			return invalid("same attributed reservation owner required")
		}
		active := activeDevReservation(st, hold.EnvironmentID)
		if active == nil || active.ID != hold.ID {
			return invalid("exact active hold required")
		}
		env := environmentByID(st, hold.EnvironmentID)
		if e := checkSelection(env, in.ExpectedSelection); e != nil {
			return e
		}
		capture, e := s.restorationOriginal(st, hold)
		if e != nil {
			return e
		}
		generation := uint64(1)
		if prior, ok := latestRestoration(st, hold.ID); !ok {
			return invalid("damaged recovery history")
		} else if prior != nil {
			if prior.DevRestoration.Generation == math.MaxUint64 || prior.LeaseUntil.After(time.Now().UTC()) {
				return invalid("earlier recovery in flight or exhausted")
			}
			if workerconfig.Fingerprint(prior) != priorHash {
				return invalid("earlier recovery changed during settlement")
			}
			generation = prior.DevRestoration.Generation + 1
		}
		effects, e := restorationEffects(st, hold)
		if e != nil {
			return e
		}
		if len(effects) >= 256 {
			return invalid("recovery effect history exhausted")
		}
		if env.ReservationEpoch == math.MaxUint64 || env.SelectionGeneration == math.MaxUint64 {
			return invalid("recovery fence exhausted")
		}
		m := meta(c)
		p := &domain.DevRestorationProof{Request: in, ReservationOperationID: hold.ID, ReservationHash: workerconfig.Fingerprint(hold.DevReservation), BaselineCaptureID: capture.ID, BaselineCaptureHash: baselineCaptureHash(*capture), Generation: generation, Epoch: env.ReservationEpoch + 1, Selection: selectionToken(*env), Deadline: m.CreatedAt.Add(time.Hour), Components: append([]domain.DevBaselineComponent(nil), capture.DevBaseline.Components...), Effects: effects}
		p.Selection.Generation++
		for i := range p.Components {
			p.Components[i].RegistryJobID = ""
			p.Components[i].RegistryInputHash = ""
			p.Components[i].RegistryRootSetHash = ""
		}
		p.SnapshotHash = restorationProofHash(*p)
		if !restorationFits(p) {
			return invalid("recovery authority exceeds bound")
		}
		env.ReservationEpoch++
		env.SelectionGeneration++
		out = domain.ExternalOperation{Meta: m, Kind: devRestorationKind, ParentID: hold.ID, RequestedBy: c.Actor, EnvironmentID: hold.EnvironmentID, ReservationEpoch: p.Epoch, RequestKey: in.RequestKey, RequestHash: hash, DevRestoration: p, Status: "PENDING", Phase: "RECONCILE_EFFECTS", CurrentStep: "RECONCILE_EFFECTS", NextAttemptAt: m.CreatedAt, Detail: "Restoration generation fenced; original hold and roots retained"}
		st.Operations = append(st.Operations, out)
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: m.CreatedAt, EntityID: out.ID, ProductID: c.ProductID, Data: c})
		return s.queueRestorationRegistry(st, &out)
	})
	if e == nil {
		st, e = s.store.Read(ctx)
		if e == nil {
			out = *operationByID(&st, out.ID)
		}
	}
	return out, e
}

func validateRestorationPlan(op domain.ExternalOperation) error {
	if e := domain.ValidateDevRestorationPlan(op); e != nil {
		return invalid("%v", e)
	}
	return nil
}

func (s *Service) markRestorationUnapproved(ctx context.Context, op domain.ExternalOperation, token string) error {
	allowed := func(st *domain.State, current *domain.ExternalOperation) error {
		if ctx.Err() != nil || current == nil || current.DevRestoration == nil || op.DevRestoration == nil || current.DevRestoration.SnapshotHash != op.DevRestoration.SnapshotHash {
			return invalid("recovery changed")
		}
		if token != "" && (op.LeaseOwner != token || current.LeaseOwner != token || !current.LeaseUntil.After(time.Now().UTC())) {
			return invalid("recovery classification lease lost")
		}
		return s.restorationAllowed(st, *current, false)
	}
	observed, e := s.store.Read(ctx)
	if e != nil {
		return e
	}
	current := operationByID(&observed, op.ID)
	if e := allowed(&observed, current); e != nil {
		return e
	}
	confirmation := restorationConfirmation(&observed, *current)
	confirmationHash := workerconfig.Fingerprint(confirmation)
	if confirmation != nil {
		// A losing observer may have read absence before a winner confirmed.
		// Reclassify against the current receipt and authoritative merge times;
		// approved, pending and unknown observations cannot poison that winner.
		if e := s.observeRestorationPlan(delivery.WithCachedAuthentication(ctx), *current, true); e != errRestorationUnapprovedMerge {
			return invalid("current confirmation does not prove an unapproved merge")
		}
	}
	before := workerconfig.Fingerprint(current)
	return s.store.Update(ctx, func(st *domain.State) error {
		cur := operationByID(st, op.ID)
		if e := allowed(st, cur); e != nil {
			return e
		}
		if cur == nil || workerconfig.Fingerprint(cur) != before || workerconfig.Fingerprint(restorationConfirmation(st, *cur)) != confirmationHash {
			return invalid("recovery or confirmation changed during merge classification")
		}
		cur.DevRestorationUnapproved = true
		cur.Status = "FAILED"
		cur.Phase = "UNAPPROVED_EFFECT"
		cur.Detail = "External merge observed before separate attributed confirmation; hold retained; no backdated approval or automatic rollback"
		cur.LeaseOwner = ""
		cur.LeaseUntil = time.Time{}
		now := time.Now().UTC()
		cur.FinishedAt = &now
		return nil
	})
}

// Recovery read reconciliation has no implicit TTL unlock or generic45minute
// cutoff. Retry changes only scheduling, never copies an old leased authority.
func (s *Service) retryRestoration(ctx context.Context, op domain.ExternalOperation, token, detail string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		cur := operationByID(st, op.ID)
		if cur == nil || cur.LeaseOwner != token || !cur.LeaseUntil.After(time.Now().UTC()) || ctx.Err() != nil {
			return invalid("recovery retry lease lost")
		}
		if cur.Kind == devRestorationVerificationKind {
			if e := s.restorationVerificationAllowed(st, *cur); e != nil {
				return e
			}
		} else {
			if e := s.restorationAllowed(st, *cur, false); e != nil {
				return e
			}
		}
		if len(detail) > 1000 {
			detail = detail[:1000]
		}
		cur.Detail = detail
		cur.Status = "RUNNING"
		cur.UpdatedAt = time.Now().UTC()
		cur.NextAttemptAt = cur.UpdatedAt.Add(2 * time.Second)
		cur.LeaseOwner = ""
		cur.LeaseUntil = time.Time{}
		return nil
	})
}
