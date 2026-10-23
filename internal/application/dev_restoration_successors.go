package application

import (
	"context"
	"math"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"time"
)

func restorationConfirmation(st *domain.State, parent domain.ExternalOperation) *domain.ExternalOperation {
	var found *domain.ExternalOperation
	for i := range st.Operations {
		o := &st.Operations[i]
		if o.Kind != devRestorationConfirmationKind || o.ParentID != parent.ID {
			continue
		}
		p := o.DevRestorationConfirmation
		if found != nil || p == nil || o.ReservationQuarantined || o.Status != "SUCCEEDED" || o.Phase != "ATTRIBUTED_CONTROL_CONFIRMATION" || o.ProductID != parent.ProductID || o.RequestedBy != parent.RequestedBy || o.EnvironmentID != parent.EnvironmentID || o.ReservationEpoch != parent.ReservationEpoch || p.TrustGrade != "ATTRIBUTED_CONTROL_CONFIRMATION" || p.Hash != restorationConfirmationHash(*p) || p.RestorationHash != parent.DevRestoration.SnapshotHash || p.Plan.Hash == "" || p.Plan.Hash != restorationPlanHash(p.Plan) || parent.DevRestorationPlan == nil || p.Plan.Hash != parent.DevRestorationPlan.Hash || p.Request.RestorationOperationID != parent.ID || p.Request.ExpectedPlanHash != p.Plan.Hash || p.Request.ExpectedSelection == nil || *p.Request.ExpectedSelection != parent.DevRestoration.Selection || p.ConfirmedAt.Before(parent.CreatedAt) || p.ConfirmedAt.After(o.CreatedAt) {
			return nil
		}
		expected := workerconfig.Fingerprint(struct {
			Action, Product, Actor string
			Input                  domain.DevRestorationRequest
		}{"confirm_candidate_dev_restoration", o.ProductID, o.RequestedBy, p.Request})
		if o.RequestKey != p.Request.RequestKey || expected != o.RequestHash {
			return nil
		}
		found = o
	}
	return found
}
func latestRestorationVerification(st *domain.State, parent string) (*domain.ExternalOperation, bool) {
	var out *domain.ExternalOperation
	ok := true
	for i := range st.Operations {
		o := &st.Operations[i]
		if o.Kind != devRestorationVerificationKind || o.ParentID != parent {
			continue
		}
		p := o.DevRestorationVerification
		if p == nil || p.Generation == 0 {
			ok = false
			continue
		}
		if out == nil || p.Generation > out.DevRestorationVerification.Generation {
			out = o
		} else if p.Generation == out.DevRestorationVerification.Generation {
			ok = false
		}
	}
	return out, ok
}
func (s *Service) restorationRuntime(st *domain.State, parent domain.ExternalOperation, in domain.DevRestorationRequest, generation uint64) (domain.DevRuntimeProof, error) {
	p, e := domain.RestorationRuntimeExpected(parent, in, generation)
	if e != nil {
		return p, invalid("%v", e)
	}
	return p, nil
}

func (s *Service) executeRestorationSuccessor(ctx context.Context, c domain.Command, in domain.DevRestorationRequest, hash string) (any, error) {
	st, e := s.store.Read(ctx)
	if e != nil {
		return nil, e
	}
	parent := operationByID(&st, in.RestorationOperationID)
	if parent == nil || parent.ProductID != c.ProductID || parent.RequestedBy != c.Actor || parent.DevRestoration == nil {
		return nil, invalid("same attributed restoration owner required")
	}
	if e = s.restorationAllowed(&st, *parent, false); e != nil {
		return nil, e
	}
	if e = checkSelection(environmentByID(&st, parent.EnvironmentID), in.ExpectedSelection); e != nil {
		return nil, e
	}
	ctx = delivery.WithCachedAuthentication(ctx)
	if c.Action == "release_candidate_dev_after_restoration" {
		return s.releaseAfterRestoration(ctx, c, in, hash, *parent)
	}
	if c.Action == "confirm_candidate_dev_restoration" {
		if parent.Phase != "WAIT_EXTERNAL_MERGE" || parent.DevRestorationPlan == nil || parent.DevRestorationPlan.Hash == "" || parent.DevRestorationPlan.Hash != in.ExpectedPlanHash || parent.DevRestorationPlan.Hash != restorationPlanHash(*parent.DevRestorationPlan) {
			return nil, invalid("exact complete stable recovery plan required")
		}
		for _, o := range st.Operations {
			if o.Kind == devRestorationConfirmationKind && o.ParentID == parent.ID {
				return nil, invalid("immutable recovery confirmation already exists")
			}
		}
		if e = s.observeRestorationPlan(ctx, *parent, false); e != nil {
			if e == errRestorationUnapprovedMerge {
				if current := s.markRestorationUnapproved(ctx, *parent, ""); current != nil {
					return nil, current
				}
			}
			return nil, e
		}
	} else if c.Action == "verify_candidate_dev_restoration" {
		if restorationConfirmation(&st, *parent) == nil {
			return nil, invalid("separate attributed confirmation required")
		}
		if _, e = s.restorationRuntime(&st, *parent, in, 1); e != nil {
			return nil, e
		}
	}
	before := workerconfig.Fingerprint(parent)
	var out domain.ExternalOperation
	e = s.store.Update(ctx, func(st *domain.State) error {
		if old, e := restorationReplay(st, c, in, hash); e != nil {
			return e
		} else if old != nil {
			out = *old
			return nil
		}
		current := operationByID(st, parent.ID)
		if current == nil || workerconfig.Fingerprint(current) != before {
			return invalid("recovery changed during confirmation/proof admission")
		}
		if e = s.restorationAllowed(st, *current, false); e != nil {
			return e
		}
		if e = checkSelection(environmentByID(st, current.EnvironmentID), in.ExpectedSelection); e != nil {
			return e
		}
		m := meta(c)
		out = domain.ExternalOperation{Meta: m, Kind: restorationKind(c.Action), ParentID: parent.ID, RequestedBy: c.Actor, EnvironmentID: parent.EnvironmentID, ReservationEpoch: parent.ReservationEpoch, RequestKey: in.RequestKey, RequestHash: hash, Status: "PENDING", Phase: "VERIFY_RESTORATION", NextAttemptAt: m.CreatedAt}
		if c.Action == "confirm_candidate_dev_restoration" {
			// Distinct request keys may observe the same plan concurrently. Admit
			// its one immutable confirmation under the same lock as the append;
			// identical-key replay has already returned above.
			for _, o := range st.Operations {
				if o.Kind == devRestorationConfirmationKind && o.ParentID == current.ID {
					return invalid("immutable recovery confirmation already exists")
				}
			}
			plan := *current.DevRestorationPlan
			p := &domain.DevRestorationConfirmation{Request: in, RestorationHash: current.DevRestoration.SnapshotHash, Plan: plan, TrustGrade: "ATTRIBUTED_CONTROL_CONFIRMATION", ConfirmedAt: m.CreatedAt}
			p.Hash = restorationConfirmationHash(*p)
			out.DevRestorationConfirmation = p
			out.Status = "SUCCEEDED"
			out.Phase = p.TrustGrade
			out.CurrentStep = out.Phase
			out.FinishedAt = &m.CreatedAt
			out.Detail = "Attributed control receipt only; external human-only reviewed Flux merge remains required"
		} else {
			confirmation := restorationConfirmation(st, *current)
			if confirmation == nil {
				return invalid("current immutable confirmation required")
			}
			generation := uint64(1)
			if prior, ok := latestRestorationVerification(st, parent.ID); !ok {
				return invalid("damaged verification history")
			} else if prior != nil {
				if prior.DevRestorationVerification.Generation == math.MaxUint64 {
					return invalid("verification generation exhausted")
				}
				generation = prior.DevRestorationVerification.Generation + 1
			}
			runtime, e := s.restorationRuntime(st, *current, in, generation)
			if e != nil {
				return e
			}
			p := &domain.DevRestorationVerification{Request: in, RestorationHash: current.DevRestoration.SnapshotHash, PlanHash: current.DevRestorationPlan.Hash, ConfirmationID: confirmation.ID, ConfirmationHash: confirmation.DevRestorationConfirmation.Hash, Generation: generation, Runtime: runtime, Components: append([]domain.DevBaselineComponent(nil), current.DevRestoration.Components...)}
			for i := range p.Components {
				p.Components[i].RegistryJobID = ""
				p.Components[i].RegistryInputHash = ""
				p.Components[i].RegistryRootSetHash = ""
			}
			out.DevRestorationVerification = p
		}
		if !restorationFits(out) {
			return invalid("restoration successor proof exceeds bound")
		}
		st.Operations = append(st.Operations, out)
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: m.CreatedAt, EntityID: out.ID, ProductID: c.ProductID, Data: c})
		if out.DevRestorationVerification != nil {
			temp := operationByID(st, out.ID)
			temp.DevRestoration = &domain.DevRestorationProof{Components: out.DevRestorationVerification.Components}
			if e = s.queueRestorationRegistry(st, temp); e != nil {
				return e
			}
			temp = operationByID(st, out.ID)
			temp.DevRestorationVerification.Components = temp.DevRestoration.Components
			temp.DevRestoration = nil
			temp.DevRestorationVerification.Hash = restorationVerificationHash(*temp.DevRestorationVerification)
			out = *temp
		}
		return nil
	})
	return out, e
}
func (s *Service) restorationVerificationAllowed(st *domain.State, op domain.ExternalOperation) error {
	cur := operationByID(st, op.ID)
	p := op.DevRestorationVerification
	if cur == nil || p == nil || op.ReservationQuarantined || cur.ReservationQuarantined || op.ProductID != cur.ProductID || op.RequestedBy != cur.RequestedBy || op.ParentID != cur.ParentID || op.EnvironmentID != cur.EnvironmentID || op.ReservationEpoch != cur.ReservationEpoch || !reflect.DeepEqual(p, cur.DevRestorationVerification) || p.Hash != restorationVerificationHash(*p) {
		return invalid("immutable restoration verification changed")
	}

	expectedHash := workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevRestorationRequest
	}{"verify_candidate_dev_restoration", op.ProductID, op.RequestedBy, p.Request})
	if op.Kind != devRestorationVerificationKind || op.ParentID != p.Request.RestorationOperationID || op.RequestKey != p.Request.RequestKey || op.RequestHash != expectedHash || p.Request.ExpectedSelection == nil {
		return invalid("restoration verification request identity changed")
	}
	parent := operationByID(st, op.ParentID)
	if parent == nil || parent.ProductID != op.ProductID || parent.RequestedBy != op.RequestedBy || parent.EnvironmentID != op.EnvironmentID || parent.ReservationEpoch != op.ReservationEpoch {
		return invalid("same recovery verification scope required")
	}
	if e := s.restorationAllowed(st, *parent, false); e != nil {
		return e
	}
	latest, ok := latestRestorationVerification(st, parent.ID)
	if !ok || latest == nil || latest.ID != op.ID {
		return invalid("restoration verification superseded")
	}
	confirmation := restorationConfirmation(st, *parent)
	if confirmation == nil || confirmation.ID != p.ConfirmationID || confirmation.DevRestorationConfirmation.Hash != p.ConfirmationHash {
		return invalid("restoration confirmation changed")
	}
	runtime, e := s.restorationRuntime(st, *parent, p.Request, p.Generation)
	if e != nil || !reflect.DeepEqual(runtime, p.Runtime) || parent.DevRestoration.SnapshotHash != p.RestorationHash || parent.DevRestorationPlan.Hash != p.PlanHash {
		return invalid("restoration proof input changed")
	}

	if len(p.Components) != len(parent.DevRestoration.Components) {
		return invalid("complete original verification availability membership required")
	}
	for i, c := range p.Components {
		original := parent.DevRestoration.Components[i]
		c.RegistryJobID = ""
		c.RegistryInputHash = ""
		c.RegistryRootSetHash = ""
		original.RegistryJobID = ""
		original.RegistryInputHash = ""
		original.RegistryRootSetHash = ""
		if !reflect.DeepEqual(c, original) {
			return invalid("original availability member changed")
		}
	}
	if op.Status == "SUCCEEDED" {
		if op.DevRuntimeResult == nil || s.validateRestorationAvailability(st, op, p.Components, p.Availability, op.DevRuntimeResult.ObservedAt) != nil {
			return invalid("complete successful restoration availability proof changed")
		}
	}
	return nil
}
func (s *Service) tickRestorationVerification(ctx context.Context, op domain.ExternalOperation, token string) error {
	ctx = delivery.WithCachedAuthentication(ctx)
	st, e := s.store.Read(ctx)
	if e != nil {
		return e
	}
	result := domain.DevRuntimeResult{ExpectedHash: op.DevRestorationVerification.Runtime.SnapshotHash, Outcome: "UNVERIFIED", Reason: "AUTHORITY_CHANGED", ObservedAt: time.Now().UTC()}
	guard := func(c context.Context) error {
		if e := c.Err(); e != nil {
			return e
		}
		current, e := s.store.Read(c)
		if e != nil {
			return e
		}
		v := operationByID(&current, op.ID)
		if v == nil || v.LeaseOwner != token || !v.LeaseUntil.After(time.Now().UTC()) {
			return invalid("restoration proof lease lost")
		}
		return s.restorationVerificationAllowed(&current, op)
	}
	if guard(ctx) == nil {
		parent := operationByID(&st, op.ParentID)
		settlement, e := s.reconcileRestorationEffects(ctx, &st, *parent, true)
		if e == nil {
			temp := op
			temp.DevRestoration = &domain.DevRestorationProof{Components: op.DevRestorationVerification.Components}
			availability, e := s.restorationAvailability(ctx, temp)
			if e != nil {
				return s.retryRestoration(ctx, op, token, "Waiting for fresh restoration availability")
			}
			connections := map[string]delivery.Connection{}
			for _, c := range parent.DevRestoration.Components {
				conn, e := scopedConnection(&st, c.ConnectionID, op.ProductID)
				if e != nil {
					return s.finishRestorationVerification(ctx, op, token, result)
				}
				connections[c.ApplicationID] = conn.Config
			}
			runtime, e := s.readRuntimeProof(ctx, op.DevRestorationVerification.Runtime, connections, guard)
			if e == nil {
				result = runtime
				op.DevRestorationVerification.Availability = availability
				op.DevRestorationVerification.SettlementHash = settlement
			}
		}
	}
	return s.finishRestorationVerification(ctx, op, token, result)
}
func (s *Service) finishRestorationVerification(ctx context.Context, op domain.ExternalOperation, token string, result domain.DevRuntimeResult) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		cur := operationByID(st, op.ID)
		if cur == nil || cur.LeaseOwner != token || !cur.LeaseUntil.After(time.Now().UTC()) || ctx.Err() != nil {
			return invalid("restoration verification final lease lost")
		}
		now := time.Now().UTC()
		valid := s.restorationVerificationAllowed(st, *cur) == nil && result.Outcome == "VERIFIED" && validateDevRuntimeResultAt(op.DevRestorationVerification.Runtime, result, now) == nil && now.Sub(result.ObservedAt) <= 5*time.Minute && len(op.DevRestorationVerification.Availability) == len(op.DevRestorationVerification.Components) && op.DevRestorationVerification.SettlementHash != "" && s.validateRestorationAvailability(st, op, op.DevRestorationVerification.Components, op.DevRestorationVerification.Availability, result.ObservedAt) == nil
		if !valid {
			result = domain.DevRuntimeResult{ExpectedHash: cur.DevRestorationVerification.Runtime.SnapshotHash, Outcome: "UNVERIFIED", Reason: "AUTHORITY_CHANGED", ObservedAt: now}
			result.Hash = devRuntimeResultHash(result)
		} else {
			cur.DevRestorationVerification = op.DevRestorationVerification
			cur.DevRestorationVerification.Hash = restorationVerificationHash(*cur.DevRestorationVerification)
		}
		cur.DevRuntimeResult = &result
		cur.Status = "FAILED"
		cur.Phase = "RESTORATION_UNVERIFIED"
		if valid {
			cur.Status = "SUCCEEDED"
			cur.Phase = "RESTORATION_VERIFIED"
		}
		cur.DeploymentState = cur.Phase
		cur.CurrentStep = cur.Phase
		cur.FinishedAt = &now
		cur.UpdatedAt = now
		cur.LeaseOwner = ""
		cur.LeaseUntil = time.Time{}
		if !restorationFits(cur) {
			return invalid("restoration verification result exceeds bound")
		}
		return nil
	})
}
func (s *Service) releaseAfterRestoration(ctx context.Context, c domain.Command, in domain.DevRestorationRequest, hash string, parent domain.ExternalOperation) (any, error) {
	st, e := s.store.Read(ctx)
	if e != nil {
		return nil, e
	}
	verification := operationByID(&st, in.RestorationVerificationOperationID)
	latest, ok := latestRestorationVerification(&st, parent.ID)
	if !ok || latest == nil || verification == nil || latest.ID != verification.ID || verification.ParentID != parent.ID || verification.Status != "SUCCEEDED" || verification.Phase != "RESTORATION_VERIFIED" || verification.DevRuntimeResult == nil || verification.DevRestorationVerification == nil || verification.ReservationQuarantined {
		return nil, invalid("latest complete successful separate restoration verification required")
	}
	if e = s.restorationVerificationAllowed(&st, *verification); e != nil {
		return nil, e
	}
	now := time.Now().UTC()
	r := verification.DevRuntimeResult
	if r.Outcome != "VERIFIED" || r.Hash != devRuntimeResultHash(*r) || r.ObservedAt.After(now) || now.Sub(r.ObservedAt) > 5*time.Minute || validateDevRuntimeResultAt(verification.DevRestorationVerification.Runtime, *r, now) != nil {
		return nil, invalid("fresh complete original runtime proof within5minutes required")
	}
	confirmation := restorationConfirmation(&st, parent)
	if confirmation == nil {
		return nil, invalid("exact separate attributed confirmation required")
	}
	// A persisted short-lived unlock lease makes final read/save fencing explicit.
	token := id()
	m := meta(c)
	out := domain.ExternalOperation{Meta: m, Kind: devRestorationReleaseKind, ParentID: parent.ID, RequestedBy: c.Actor, EnvironmentID: parent.EnvironmentID, ReservationEpoch: parent.ReservationEpoch, RequestKey: in.RequestKey, RequestHash: hash, Status: "RUNNING", Phase: "RELEASE_RECONCILIATION", LeaseOwner: token, LeaseUntil: now.Add(45 * time.Second)}
	// ProductID belongs to embedded Meta; assigned above via m already.
	parentHash := workerconfig.Fingerprint(parent)
	verificationHash := workerconfig.Fingerprint(verification)
	e = s.store.Update(ctx, func(st *domain.State) error {
		if old, e := restorationReplay(st, c, in, hash); e != nil {
			return e
		} else if old != nil {
			out = *old
			return nil
		}
		cur := operationByID(st, parent.ID)
		v := operationByID(st, verification.ID)
		if cur == nil || v == nil || workerconfig.Fingerprint(cur) != parentHash || workerconfig.Fingerprint(v) != verificationHash {
			return invalid("restoration/proof changed before unlock")
		}
		if e := s.restorationAllowed(st, *cur, false); e != nil {
			return e
		}
		st.Operations = append(st.Operations, out)
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: m.CreatedAt, EntityID: out.ID, ProductID: c.ProductID, Data: c})
		return nil
	})
	if e != nil {
		return nil, e
	}
	if out.LeaseOwner != token {
		return out, nil
	}
	fail := func(err error) (any, error) {
		_ = s.store.Update(ctx, func(st *domain.State) error {
			cur := operationByID(st, out.ID)
			if cur != nil && cur.LeaseOwner == token {
				cur.Status = "FAILED"
				cur.Phase = "RELEASE_BLOCKED"
				cur.LeaseOwner = ""
				cur.LeaseUntil = time.Time{}
				at := time.Now().UTC()
				cur.FinishedAt = &at
			}
			return nil
		})
		return nil, err
	}
	if e = s.observeRestorationPlan(ctx, parent, true); e != nil {
		return fail(e)
	}
	settlement, e := s.reconcileRestorationEffects(ctx, &st, parent, true)
	if e != nil {
		return fail(e)
	}
	// All original bytes must still exist; registry proof must remain fresh.
	temp := *verification
	temp.DevRestoration = &domain.DevRestorationProof{Components: verification.DevRestorationVerification.Components}
	currentAvailability, e := s.restorationAvailability(ctx, temp)
	if e != nil {
		return fail(e)
	}
	before := workerconfig.Fingerprint(parent.DevRestorationPlan)
	observed := parent
	observed.DevRestorationPlan = &domain.DevRestorationPlan{}
	*observed.DevRestorationPlan = *parent.DevRestorationPlan
	observed.DevRestorationPlan.Groups = append([]domain.DevRestorationGroup(nil), parent.DevRestorationPlan.Groups...)
	if e = s.collectRestorationResults(ctx, &observed); e != nil {
		return fail(e)
	}
	// Fresh proof remains tied to the exact stored desired head set.
	if workerconfig.Fingerprint(observed.DevRestorationPlan) != before {
		return fail(invalid("restored desired head changed since verification"))
	}
	finalSettlement, e := s.reconcileRestorationEffects(ctx, &st, parent, true)
	if e != nil || settlement != finalSettlement || finalSettlement != verification.DevRestorationVerification.SettlementHash {
		return fail(invalid("effect settlement changed during unlock"))
	}
	e = s.store.Update(ctx, func(st *domain.State) error {
		cur := operationByID(st, out.ID)
		p := operationByID(st, parent.ID)
		v := operationByID(st, verification.ID)
		now := time.Now().UTC()
		if cur == nil || cur.LeaseOwner != token || !cur.LeaseUntil.After(now) || ctx.Err() != nil || p == nil || v == nil || workerconfig.Fingerprint(p) != parentHash || workerconfig.Fingerprint(v) != verificationHash {
			return invalid("unlock lease or final proof changed")
		}
		if e = s.restorationAllowed(st, *p, false); e != nil {
			return e
		}
		if e = s.restorationVerificationAllowed(st, *v); e != nil {
			return e
		}
		latest, ok := latestRestorationVerification(st, p.ID)
		if !ok || latest == nil || latest.ID != v.ID || now.Sub(v.DevRuntimeResult.ObservedAt) > 5*time.Minute {
			return invalid("newer or stale restoration proof")
		}
		env := environmentByID(st, parent.EnvironmentID)
		if e = checkSelection(env, in.ExpectedSelection); e != nil {
			return e
		}
		if env.ReservationEpoch == math.MaxUint64 || env.SelectionGeneration == math.MaxUint64 {
			return invalid("unlock fence exhausted")
		}
		release := &domain.DevRestorationRelease{Availability: currentAvailability, AvailabilityHash: workerconfig.Fingerprint(currentAvailability), Request: in, ReservationOperationID: p.DevRestoration.ReservationOperationID, ReservationHash: p.DevRestoration.ReservationHash, RestorationHash: p.DevRestoration.SnapshotHash, PlanHash: p.DevRestorationPlan.Hash, ConfirmationID: confirmation.ID, ConfirmationHash: confirmation.DevRestorationConfirmation.Hash, VerificationHash: v.DevRestorationVerification.Hash, ResultHash: v.DevRuntimeResult.Hash, SettlementHash: finalSettlement, RecoveryEpoch: p.ReservationEpoch, RecoverySelection: p.DevRestoration.Selection, ReleasedEpoch: env.ReservationEpoch + 1, ReleasedSelection: selectionToken(*env)}
		release.ReleasedSelection.Generation++
		release.Hash = restorationReleaseHash(*release)
		env.ReservationEpoch++
		env.SelectionGeneration++
		cur.DevRestorationRelease = release
		cur.Status = "SUCCEEDED"
		cur.Phase = "RELEASED"
		cur.DeploymentState = "RELEASED"
		cur.CurrentStep = "RELEASED"
		cur.Detail = "Only this original hold settled; desired composition preserved; historical workers remain fenced; globalGC disabled"
		cur.FinishedAt = &now
		cur.LeaseOwner = ""
		cur.LeaseUntil = time.Time{}
		out = *cur
		hold := operationByID(st, release.ReservationOperationID)
		if hold == nil || !domain.ValidRestorationRelease(st, *cur, *hold) {
			return invalid("exact restoration release chain invalid")
		}
		return nil
	})
	if e != nil {
		return fail(e)
	}
	return out, nil
}

func (s *Service) restorationQuery(st *domain.State, environment string) map[string]any {
	out := map[string]any{"restoration_verified": false}
	hold := activeDevReservation(st, environment)
	if hold == nil {
		return out
	}
	parent, ok := latestRestoration(st, hold.ID)
	if !ok || parent == nil {
		return out
	}
	out["restoration_operation_id"] = parent.ID
	proof, ok := latestRestorationVerification(st, parent.ID)
	if proof == nil {
		return out
	}
	out["restoration_verification_operation_id"] = proof.ID
	out["restoration_verification_status"] = proof.Status
	if !ok || proof.Status != "SUCCEEDED" || proof.Phase != "RESTORATION_VERIFIED" || proof.DevRuntimeResult == nil || proof.DevRestorationVerification == nil || s.restorationVerificationAllowed(st, *proof) != nil {
		return out
	}
	r := proof.DevRuntimeResult
	now := time.Now().UTC()
	if r.Outcome == "VERIFIED" && r.Hash == devRuntimeResultHash(*r) && !r.ObservedAt.After(now) && now.Sub(r.ObservedAt) <= 5*time.Minute && validateDevRuntimeResultAt(proof.DevRestorationVerification.Runtime, *r, now) == nil {
		out["restoration_verified"] = true
	}
	return out
}
