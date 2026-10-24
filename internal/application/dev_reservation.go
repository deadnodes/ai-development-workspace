package application

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

func activeDevReservation(st *domain.State, environmentID string) *domain.ExternalOperation {
	for i := range st.Operations {
		hold := &st.Operations[i]
		if hold.Kind != "DEV_RESERVATION" || hold.EnvironmentID != environmentID {
			continue
		}
		released := false
		for _, receipt := range st.Operations {
			if domain.ValidRestorationRelease(st, receipt, *hold) {
				released = true
				break
			}
		}
		for _, receipt := range st.Operations {
			if receipt.Kind != "DEV_RESERVATION_RELEASE" || receipt.Status != "SUCCEEDED" || receipt.Phase != "RELEASED" || receipt.ProductID != hold.ProductID || receipt.EnvironmentID != hold.EnvironmentID || receipt.RequestedBy != hold.RequestedBy || receipt.DevReservation == nil || hold.DevReservation == nil {
				continue
			}
			p := receipt.DevReservation
			if p.ReservationOperationID != hold.ID || p.Request.ReservationOperationID != hold.ID || p.Request.RequestKey != receipt.RequestKey {
				continue
			}
			hash := workerconfig.Fingerprint(struct {
				Action, Product, Actor string
				Input                  domain.DevReservationRequest
			}{"release_candidate_dev", receipt.ProductID, receipt.RequestedBy, p.Request})
			if hash != receipt.RequestHash || p.Request.ExpectedSelection == nil || *p.Request.ExpectedSelection != hold.DevReservation.ReservedSelection || receipt.ReservationEpoch != hold.DevReservation.Epoch {
				continue
			}
			copy := *p
			copy.Request = hold.DevReservation.Request
			copy.ReservationOperationID = ""
			if reflect.DeepEqual(copy, *hold.DevReservation) {
				released = true
				break
			}
		}
		if !released {
			return hold
		}
	}
	return nil
}
func reservationFree(st *domain.State, environmentID string) error {
	if hold := activeDevReservation(st, environmentID); hold != nil {
		return invalid("environment has candidate Dev reservation %s; explicit owner release or reconciliation required", hold.ID)
	}
	return nil
}
func reservationOperationAllowed(st *domain.State, op domain.ExternalOperation) error {
	if (op.DevActivationGroupID != "" || op.DevActivationBatch != nil || op.DevActivationIntentHash != "" || op.DevActivationPRHash != "" || op.DevActivationResultHash != "") && devActivationParent(st, op) == nil {
		return invalid("orphaned candidate Dev group authority cannot resume")
	}
	if devActivationParent(st, op) != nil {
		return devActivationGuard(st, op)
	}
	switch op.Kind {
	case "DEPLOY", "COMPOSE", "RELEASE_CANDIDATE", "RELEASE_PROMOTION":
	default:
		return nil
	}
	if err := reservationFree(st, op.EnvironmentID); err != nil {
		return err
	}
	env := environmentByID(st, op.EnvironmentID)
	if env == nil || env.ReservationEpoch != op.ReservationEpoch {
		return invalid("environment reservation epoch changed; old operation authority cannot resume")
	}
	return nil
}
func reservationCommandAllowed(st *domain.State, c domain.Command) error {
	envID, _ := c.Data["environment_id"].(string)
	switch c.Action {
	case "delete_product":
		for _, e := range st.Environments {
			if e.ProductID == c.ProductID {
				if err := reservationFree(st, e.ID); err != nil {
					return err
				}
			}
		}
		return nil
	case "select_composition", "reconcile_composition", "promote_release_candidate":
		if op := operationByID(st, c.ID); op != nil {
			envID = op.EnvironmentID
		}
		if comp := compositionByID(st, c.ID); comp != nil {
			envID = comp.EnvironmentID
		}
	case "update_environment":
		envID = c.ID
	case "configure_environment", "assemble_environment", "prepare_release_candidate", "create_hotfix":
	default:
		return nil
	}
	return reservationFree(st, envID)
}
func devReservationInput(c domain.Command) (domain.DevReservationRequest, string, error) {
	var in domain.DevReservationRequest
	if strings.TrimSpace(c.Actor) == "" || c.ProductID == "" || c.ID != "" || c.FeatureID != "" || c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return in, "", invalid("reservation requires actor and product scope only")
	}
	allowed := map[string]bool{"request_key": true, "expected_selection": true}
	if c.Action == "reserve_candidate_dev" {
		allowed["candidate_operation_id"] = true
		allowed["environment_id"] = true
	} else {
		allowed["reservation_operation_id"] = true
	}
	for key := range c.Data {
		if !allowed[key] {
			return in, "", invalid("unknown reservation field %s", key)
		}
	}
	if err := decode(c.Data, &in); err != nil {
		return in, "", err
	}
	if len(in.RequestKey) < 16 || len(in.RequestKey) > 128 || strings.TrimSpace(in.RequestKey) != in.RequestKey || strings.ContainsFunc(in.RequestKey, func(r rune) bool { return r < 32 || r == 127 }) {
		return in, "", invalid("request_key must be 16..128 opaque bytes without control characters")
	}
	if in.ExpectedSelection == nil {
		return in, "", invalid("exact expected_selection required")
	}
	b, _ := json.Marshal(c.Data["expected_selection"])
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(b, &keys)
	if len(keys) != 3 {
		return in, "", invalid("exact expected_selection fields required")
	}
	for _, k := range []string{"composition_id", "operation_id", "generation"} {
		if v, ok := keys[k]; !ok || string(v) == "null" {
			return in, "", invalid("expected_selection.%s required", k)
		}
	}
	if c.Action == "reserve_candidate_dev" && (in.CandidateOperationID == "" || in.EnvironmentID == "") || c.Action == "release_candidate_dev" && in.ReservationOperationID == "" {
		return in, "", invalid("exact candidate/environment or reservation identity required")
	}
	return in, workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevReservationRequest
	}{c.Action, c.ProductID, c.Actor, in}), nil
}
func devReservationReplay(st *domain.State, c domain.Command, in domain.DevReservationRequest, hash string) (*domain.ExternalOperation, error) {
	for i := range st.Operations {
		op := &st.Operations[i]
		if op.ProductID == c.ProductID && op.RequestKey == in.RequestKey && domain.IsRestorationOperation(*op) {
			return nil, invalid("request_key already binds restoration receipt")
		}
		if op.Kind == devBaselineKind && op.RequestKey == in.RequestKey {
			return nil, invalid("request_key already binds a baseline capture")
		}
		if op.Kind == devRuntimeKind && op.RequestKey == in.RequestKey {
			return nil, invalid("request_key already binds a runtime verification")
		}
		if op.ProductID == c.ProductID && op.RequestKey == in.RequestKey && (op.Kind == "DEV_RESERVATION" || op.Kind == "DEV_RESERVATION_RELEASE" || op.Kind == devActivationKind) {
			if op.RequestHash != hash || op.RequestedBy != c.Actor {
				return nil, invalid("request_key already binds another reservation payload or actor")
			}
			return op, nil
		}
	}
	return nil, nil
}
func unresolvedReservationEffects(st *domain.State, environmentID string) error {
	for _, op := range st.Operations {
		if op.EnvironmentID != environmentID {
			continue
		}
		if op.ReservationMutationIntent != "" || op.Phase == "APPLY_UNCERTAIN" || op.GitOpsPR != nil && !op.GitOpsPR.Merged && !strings.EqualFold(op.GitOpsPR.State, "closed") {
			return invalid("unresolved GitOps effect requires reconciliation before reservation")
		}
		if op.Phase == "BUILD_LOOKUP" && (op.Run == nil || op.Run.Status != "completed") {
			return invalid("unresolved build dispatch requires reconciliation before reservation")
		}
		if op.Native != nil && op.Native.PublicationJobID != "" {
			if j := workerJob(st, op.Native.PublicationJobID); j != nil && (j.Status == "RUNNING" || j.Status == "RECONCILIATION_REQUIRED") {
				return invalid("publication in flight requires reconciliation before reservation")
			}
		}
	}
	return nil
}
func (s *Service) executeDevReservation(ctx context.Context, c domain.Command) (any, error) {
	in, hash, err := devReservationInput(c)
	if err != nil {
		return nil, err
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	if old, e := devReservationReplay(&st, c, in, hash); e != nil {
		return nil, e
	} else if old != nil {
		return *old, nil
	}
	// Observe immutable source identity outside the store transaction. Replay performs no IO.
	if c.Action == "reserve_candidate_dev" {
		candidate := operationByID(&st, in.CandidateOperationID)
		if candidate == nil {
			return nil, invalid("successful native main candidate required")
		}
		if candidate.ProductID != c.ProductID || candidate.Status != "SUCCEEDED" || candidate.DeploymentState != "READY_FOR_VERIFICATION" {
			return nil, invalid("successful same-product main candidate required")
		}
		if e := s.mainCandidateAllowed(&st, candidate); e != nil {
			return nil, e
		}
		copy := cloneFlowOperation(*candidate)
		if err = s.observeMainCandidate(ctx, &copy, false); err != nil {
			return nil, err
		}
	}
	var out domain.ExternalOperation
	err = s.store.Update(ctx, func(st *domain.State) error {
		if old, e := devReservationReplay(st, c, in, hash); e != nil {
			return e
		} else if old != nil {
			out = *old
			return nil
		}
		m := meta(c)
		now := m.CreatedAt
		if !productExists(st, c.ProductID) {
			return missing("product", c.ProductID)
		}
		if c.Action == "reserve_candidate_dev" {
			candidate := operationByID(st, in.CandidateOperationID)
			if candidate == nil || candidate.ProductID != c.ProductID || candidate.Kind != "RELEASE_CANDIDATE" || candidate.Status != "SUCCEEDED" || candidate.DeploymentState != "READY_FOR_VERIFICATION" || candidate.MainCandidate == nil {
				return invalid("successful native main candidate required")
			}
			if e := s.mainCandidateAllowed(st, candidate); e != nil {
				return e
			}
			environment := environmentByID(st, in.EnvironmentID)
			if environment == nil || environment.ProductID != c.ProductID {
				return invalid("reservation target outside product")
			}
			if e := checkSelection(environment, in.ExpectedSelection); e != nil {
				return e
			}
			if e := reservationFree(st, environment.ID); e != nil {
				return e
			}
			if e := unresolvedReservationEffects(st, environment.ID); e != nil {
				return e
			}
			baseline := compositionByID(st, environment.DesiredCompositionID)
			if baseline == nil || baseline.ProductID != c.ProductID || baseline.EnvironmentID != environment.ID || baseline.EnvironmentSnapshot.Cluster != environment.Cluster || baseline.EnvironmentSnapshot.Namespace != environment.Namespace {
				return invalid("exact current baseline and target required")
			}
			if environment.DesiredOperationID != "" {
				op := operationByID(st, environment.DesiredOperationID)
				if op == nil || op.ProductID != c.ProductID || op.EnvironmentID != environment.ID || op.CompositionSnapshot == nil || op.CompositionSnapshot.ID != baseline.ID {
					return invalid("baseline operation identity mismatch")
				}
			}
			targets := []domain.EnvironmentBinding{}
			baselineApps := map[string]bool{}
			for _, app := range baseline.ApplicationSnapshots {
				baselineApps[app.ID] = true
			}
			if len(candidate.ChildIDs) != len(candidate.PreparedDeployments) {
				return invalid("completed candidate components required")
			}
			for _, cid := range candidate.ChildIDs {
				child := operationByID(st, cid)
				if child == nil || child.ParentID != candidate.ID || child.Status != "SUCCEEDED" || child.DeploymentState != "ARTIFACT_READY" || !child.BuildOnly || child.Snapshot == nil || child.Snapshot.MainCandidate == nil {
					return invalid("completed native build-only children required")
				}
			}
			for _, snap := range candidate.PreparedDeployments {
				if !baselineApps[snap.Application.ID] {
					return invalid("candidate component absent from shared baseline")
				}
				target := environmentBinding(st, environment.ID, snap.Application.ID)
				if target == nil || target.ProductID != c.ProductID || !target.AllowDeploy || target.GitOpsMode != "PR" || target.Purpose != "DEV" && target.Purpose != "TEST" {
					return invalid("all candidate components require PR-only DEV/TEST target")
				}
				conn, e := scopedConnection(st, target.ConnectionID, c.ProductID)
				if e != nil {
					return e
				}
				binding := repositoryBinding(st, target.RepositoryID)
				if binding == nil || binding.ProductID != c.ProductID || binding.Role != "GITOPS" || binding.ConnectionID != conn.ID {
					return invalid("Dev target repository authority unavailable")
				}
				targets = append(targets, *target)
			}
			if environment.ReservationEpoch == math.MaxUint64 || environment.SelectionGeneration == math.MaxUint64 {
				return invalid("reservation epoch or selection generation exhausted")
			}
			before := *environment
			baselineToken := selectionToken(before)
			environment.ReservationEpoch++
			environment.SelectionGeneration++
			proof := &domain.DevReservationProof{Request: in, CandidateOperationID: candidate.ID, CandidateHash: workerconfig.Fingerprint(candidate.MainCandidate), CandidateProof: *candidate.MainCandidate, BaselineSelection: baselineToken, ReservedSelection: selectionToken(*environment), Epoch: environment.ReservationEpoch, Environment: before, Targets: targets}
			out = domain.ExternalOperation{Meta: m, Kind: "DEV_RESERVATION", RequestedBy: c.Actor, EnvironmentID: environment.ID, RequestKey: in.RequestKey, RequestHash: hash, DevReservation: proof, ReservationEpoch: environment.ReservationEpoch, Status: "SUCCEEDED", Phase: "RESERVED", CurrentStep: "RESERVED", Detail: "Reservation accepted; no candidate deployed or verified", FinishedAt: &now}
		} else {
			hold := operationByID(st, in.ReservationOperationID)
			if hold == nil || hold.ProductID != c.ProductID || hold.Kind != "DEV_RESERVATION" || hold.DevReservation == nil || hold.RequestedBy != c.Actor {
				return invalid("same attributed reservation owner required")
			}
			for _, activation := range st.Operations {
				if activation.DevActivation != nil && activation.DevActivation.ReservationOperationID == hold.ID || activation.Kind == devActivationKind && activation.EnvironmentID == hold.EnvironmentID && activation.ReservationEpoch == hold.ReservationEpoch {
					return invalid("candidate activation admitted; verified restoration required before release")
				}
			}
			environment := environmentByID(st, hold.EnvironmentID)
			if environment == nil || environment.ProductID != c.ProductID {
				return invalid("reservation environment unavailable")
			}
			if e := checkSelection(environment, in.ExpectedSelection); e != nil {
				return e
			}
			active := activeDevReservation(st, environment.ID)
			if active == nil || active.ID != hold.ID || hold.ReservationQuarantined || nativeQuarantined(st, c.ProductID) || hold.Status != "SUCCEEDED" || hold.Phase != "RESERVED" {
				return invalid("reservation requires explicit reconciliation; release unavailable")
			}
			proof := hold.DevReservation
			if environment.ReservationEpoch != proof.Epoch || selectionToken(*environment) != proof.ReservedSelection || environment.Cluster != proof.Environment.Cluster || environment.Namespace != proof.Environment.Namespace {
				return invalid("reservation identity or preserved baseline changed")
			}
			if e := unresolvedReservationEffects(st, environment.ID); e != nil {
				return e
			}
			if e := advanceSelection(environment); e != nil {
				return e
			}
			copied := *proof
			copied.Request = in
			copied.ReservationOperationID = hold.ID
			out = domain.ExternalOperation{Meta: m, Kind: "DEV_RESERVATION_RELEASE", RequestedBy: c.Actor, EnvironmentID: environment.ID, RequestKey: in.RequestKey, RequestHash: hash, DevReservation: &copied, ReservationEpoch: environment.ReservationEpoch, Status: "SUCCEEDED", Phase: "RELEASED", CurrentStep: "RELEASED", Detail: "Reservation released; baseline intent preserved, old execution authority remains fenced", FinishedAt: &now}
		}
		st.Operations = append(st.Operations, out)
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: now, EntityID: out.ID, ProductID: c.ProductID, Data: c})
		return nil
	})
	return out, err
}

// ReservationState is safe query state, not deployment/runtime proof.
func ReservationState(st *domain.State, environmentID string) map[string]any {
	hold := activeDevReservation(st, environmentID)
	if hold == nil {
		return map[string]any{"active": false}
	}
	if hold.DevReservation == nil {
		return map[string]any{"active": true, "operation_id": hold.ID, "reconciliation_required": true, "candidate_deployed": false}
	}
	out := map[string]any{"active": true, "operation_id": hold.ID, "candidate_operation_id": hold.DevReservation.CandidateOperationID, "owner": hold.RequestedBy, "epoch": hold.DevReservation.Epoch, "selection": hold.DevReservation.ReservedSelection, "reconciliation_required": hold.ReservationQuarantined || hold.Status != "SUCCEEDED", "candidate_deployed": false, "release_action": "release_candidate_dev"}
	for _, activation := range st.Operations {
		if activation.Kind == devActivationKind && activation.DevActivation != nil && activation.DevActivation.ReservationOperationID == hold.ID {
			out["candidate_deployment_id"] = activation.ID
			out["candidate_deployment_status"] = activation.Status
			out["candidate_deployment_state"] = activation.DeploymentState
			groups := []map[string]any{}
			for _, g := range activation.DevActivation.Groups {
				v := map[string]any{"group_id": g.ID, "leader_id": g.LeaderID, "member_ids": append([]string(nil), g.MemberIDs...)}
				if leader := operationByID(st, g.LeaderID); leader != nil && leader.GitOpsPR != nil {
					v["pull_request_number"] = leader.GitOpsPR.Number
					v["pull_request_url"] = leader.GitOpsPR.URL
					v["head_sha"] = leader.GitOpsPR.HeadSHA
				}
				groups = append(groups, v)
			}
			out["candidate_gitops_groups"] = groups
			out["release_action"] = "verified_restoration_required"
		}
	}
	if recovery, ok := latestRestoration(st, hold.ID); recovery != nil {
		out["restoration_operation_id"] = recovery.ID
		out["restoration_status"] = recovery.Status
		out["restoration_phase"] = recovery.Phase
		out["restoration_current"] = ok
		out["release_action"] = "release_candidate_dev_after_restoration"
		if recovery.DevRestoration != nil {
			out["recovery_epoch"] = recovery.DevRestoration.Epoch
			out["recovery_selection"] = recovery.DevRestoration.Selection
			out["recovery_generation"] = recovery.DevRestoration.Generation
		}
		if recovery.DevRestorationPlan != nil {
			out["restoration_plan_hash"] = recovery.DevRestorationPlan.Hash
		}
	}
	return out
}

// beginReservationMutation serializes external mutation intent with reserve.
// Once intent exists, cancellation/lease expiry cannot prove that no write occurred.
func (s *Service) beginReservationMutation(ctx context.Context, op domain.ExternalOperation, token, kind string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := operationByID(st, op.ID)
		if current == nil || terminalOperation(current.Status) || current.LeaseOwner != token || !current.LeaseUntil.After(time.Now().UTC()) {
			return invalid("mutation lease unavailable")
		}
		if err := reservationOperationAllowed(st, *current); err != nil {
			return err
		}
		if current.Kind == "COMPOSE" || current.Kind == "RELEASE_CANDIDATE" || current.Kind == "RELEASE_PROMOTION" {
			if err := s.flowGuard(st, *current); err != nil {
				return err
			}
		}
		if current.ReservationMutationIntent != "" {
			return invalid("unresolved managed mutation intent requires read-only recovery")
		}
		current.ReservationMutationIntent = kind
		return nil
	})
}

func (s *Service) reservationMutationRecovery(ctx context.Context, op domain.ExternalOperation, token string) (bool, error) {
	st, err := s.store.Read(ctx)
	if err != nil {
		return false, err
	}
	current := operationByID(&st, op.ID)
	if current == nil || terminalOperation(current.Status) || current.LeaseOwner != token || !current.LeaseUntil.After(time.Now().UTC()) {
		return false, invalid("mutation recovery lease unavailable")
	}
	if err = reservationOperationAllowed(&st, *current); err != nil {
		return false, err
	}
	if current.ReservationMutationIntent != "" && current.ReservationMutationIntent != "SOURCE" {
		return false, invalid("different unresolved mutation intent")
	}
	return current.ReservationMutationIntent == "SOURCE", nil
}
