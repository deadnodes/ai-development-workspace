package application

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

func (s *Service) validateDevRuntimeIO(ctx context.Context, op domain.ExternalOperation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	current := operationByID(&st, op.ID)
	if current == nil || terminalOperation(current.Status) || current.LeaseOwner != op.LeaseOwner || !current.LeaseUntil.After(time.Now().UTC()) {
		return invalid("runtime verification lease unavailable")
	}
	return s.devRuntimeAllowed(&st, op, true)
}
func (s *Service) tickDevRuntime(ctx context.Context, op domain.ExternalOperation, token string) error {
	if err := s.validateDevRuntimeIO(ctx, op); err != nil {
		return s.finishDevRuntime(ctx, op, token, &domain.DevRuntimeResult{Reason: runtimeFailureReason(err)})
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	if err = s.devRuntimeAllowed(&st, op, true); err != nil {
		return s.finishDevRuntime(ctx, op, token, &domain.DevRuntimeResult{Reason: "AUTHORITY_CHANGED"})
	}
	activation := operationByID(&st, op.ParentID)
	if err = s.observeMainCandidate(ctx, operationByID(&st, activation.DevActivation.CandidateOperationID), false); err != nil {
		return s.finishDevRuntime(ctx, op, token, &domain.DevRuntimeResult{Reason: runtimeFailureReason(err)})
	}
	result, err := s.readDevRuntime(ctx, op, &st)
	if err != nil {
		return s.finishDevRuntime(ctx, op, token, &domain.DevRuntimeResult{Reason: runtimeFailureReason(err)})
	}
	return s.finishDevRuntime(ctx, op, token, &result)
}

type runtimeGitOpsGroup struct {
	Connection delivery.Connection
	Request    delivery.GitOpsRevisionRequest
	Proof      delivery.GitOpsRevisionProof
}

func runtimeGitOpsAuthority(c delivery.Connection) (string, error) {
	v, e := domain.GitOpsAuthorityFingerprint(c)
	if e != nil {
		return v, invalid("%v", e)
	}
	return v, nil
}

func runtimeGitOpsGroupKey(c delivery.Connection, r delivery.GitOpsRequest) string {
	authority, _ := runtimeGitOpsAuthority(c)
	return workerconfig.Fingerprint(struct{ Authority, Repository, Ref string }{authority, r.Repository, r.Ref})
}
func runtimeGitOpsObservation(v delivery.GitOpsSnapshot) domain.DevRuntimeGitOpsObservation {
	return domain.DevRuntimeGitOpsObservation{ImageRepository: v.ImageRepository, HeadSHA: v.HeadSHA, BlobSHA: v.BlobSHA, Digest: v.Digest, ContentHash: workerconfig.Fingerprint(v.Content)}
}
func validRevisionProof(r delivery.GitOpsRevisionRequest, p delivery.GitOpsRevisionProof) bool {
	return p.Repository == r.Repository && p.Ref == r.Ref && fullSHA.MatchString(p.HeadSHA) && (r.HeadSHA == "" || p.HeadSHA == r.HeadSHA) && reflect.DeepEqual(p.AncestorSHAs, r.AncestorSHAs)
}
func (s *Service) readDevRuntime(ctx context.Context, op domain.ExternalOperation, st *domain.State) (domain.DevRuntimeResult, error) {
	connections := map[string]delivery.Connection{}
	for _, c := range op.DevRuntime.Components {
		connections[c.ApplicationID] = operationByID(st, c.ChildID).Snapshot.GitOpsConnection
	}
	return s.readRuntimeProof(ctx, *op.DevRuntime, connections, func(ctx context.Context) error { return s.validateDevRuntimeIO(ctx, op) })
}
func (s *Service) readRuntimeProof(ctx context.Context, proof domain.DevRuntimeProof, connections map[string]delivery.Connection, guard func(context.Context) error) (domain.DevRuntimeResult, error) {
	out := domain.DevRuntimeResult{ExpectedHash: proof.SnapshotHash, Outcome: "VERIFIED", Reason: "EXACT_CONFIGURED_RUNTIME_MATCH"}
	groups := map[string]*runtimeGitOpsGroup{}
	keys := []string{}
	verifier, ok := s.provider.(delivery.GitOpsRevisionVerifier)
	if !ok {
		return out, invalid("runtime GitOps revision verifier unavailable")
	}
	for _, component := range proof.Components {
		connection := connections[component.ApplicationID]
		key := runtimeGitOpsGroupKey(connection, component.GitOps)
		group := groups[key]
		if group == nil {
			group = &runtimeGitOpsGroup{Connection: connection, Request: delivery.GitOpsRevisionRequest{Repository: component.GitOps.Repository, Ref: component.GitOps.Ref}}
			groups[key] = group
			keys = append(keys, key)
		}
		if !slices.Contains(group.Request.AncestorSHAs, component.GitOpsCommit) {
			group.Request.AncestorSHAs = append(group.Request.AncestorSHAs, component.GitOpsCommit)
		}
	}
	// Ordinary bounded reads may populate existing authentication caches. Only the
	// separate GET-only verifier establishes the common immutable revision.
	for _, key := range keys {
		group := groups[key]
		sort.Strings(group.Request.AncestorSHAs)
		var request delivery.GitOpsRequest
		for _, c := range proof.Components {
			if runtimeGitOpsGroupKey(connections[c.ApplicationID], c.GitOps) == key {
				request = c.GitOps
				break
			}
		}
		if err := guard(ctx); err != nil {
			return out, err
		}
		if _, err := s.provider.ReadGitOps(ctx, group.Connection, request); err != nil {
			return out, invalid("runtime GitOps read unavailable")
		}
		if err := guard(ctx); err != nil {
			return out, err
		}
		proof, err := verifier.VerifyGitOpsRevision(ctx, group.Connection, group.Request)
		if err != nil || !validRevisionProof(group.Request, proof) {
			return out, invalid("runtime common GitOps ancestry unavailable")
		}
		group.Proof = proof
	}
	pods := 0
	for _, component := range proof.Components {
		connection := connections[component.ApplicationID]
		group := groups[runtimeGitOpsGroupKey(connection, component.GitOps)]
		request := component.GitOps
		request.ExpectedHeadSHA = group.Proof.HeadSHA
		if err := guard(ctx); err != nil {
			return out, err
		}
		before, err := s.provider.ReadGitOps(ctx, connection, request)
		if err != nil {
			return out, invalid("runtime pinned GitOps read unavailable")
		}
		observed := runtimeGitOpsObservation(before)
		if err := validateRuntimeGitOps(component, observed, group.Proof); err != nil {
			return out, err
		}
		cr := domain.DevRuntimeComponentResult{ApplicationID: component.ApplicationID, GitOps: observed, Revision: group.Proof}
		for _, scope := range component.Scopes {
			var chosen delivery.RuntimeVerificationObserver
			for _, o := range s.runtimeObservers {
				v, ok := o.(delivery.RuntimeVerificationObserver)
				if !ok {
					continue
				}
				actual, e := v.RuntimeVerificationScope()
				if e == nil && reflect.DeepEqual(actual, scope) {
					if chosen != nil {
						return out, invalid("duplicate runtime binding")
					}
					chosen = v
				}
			}
			if chosen == nil {
				return out, invalid("frozen runtime role unavailable")
			}
			if err := guard(ctx); err != nil {
				return out, err
			}
			actual := chosen.Snapshot(ctx)
			if err := validateRuntimeRole(component, scope, actual, group.Proof.HeadSHA); err != nil {
				return out, err
			}
			pods += len(actual.Pods)
			if pods > 512 {
				return out, invalid("runtime pod proof bound exceeded")
			}
			// Successful snapshots contain only the typed operational fields and no errors.
			cr.Roles = append(cr.Roles, actual)
		}
		if err := guard(ctx); err != nil {
			return out, err
		}
		after, err := s.provider.ReadGitOps(ctx, connection, request)
		if err != nil || !reflect.DeepEqual(before, after) {
			return out, invalid("runtime target changed during observation")
		}
		out.Components = append(out.Components, cr)
	}
	// Check every target again after ALL role reads, including shared-file siblings.
	for _, component := range proof.Components {
		connection := connections[component.ApplicationID]
		group := groups[runtimeGitOpsGroupKey(connection, component.GitOps)]
		request := component.GitOps
		request.ExpectedHeadSHA = group.Proof.HeadSHA
		if err := guard(ctx); err != nil {
			return out, err
		}
		after, err := s.provider.ReadGitOps(ctx, connection, request)
		if err != nil {
			return out, invalid("runtime final target unavailable")
		}
		var expected domain.DevRuntimeGitOpsObservation
		for _, cr := range out.Components {
			if cr.ApplicationID == component.ApplicationID {
				expected = cr.GitOps
				break
			}
		}
		if runtimeGitOpsObservation(after) != expected {
			return out, invalid("runtime final target drift")
		}
	}
	for _, key := range keys {
		group := groups[key]
		request := group.Request
		request.HeadSHA = group.Proof.HeadSHA
		if err := guard(ctx); err != nil {
			return out, err
		}
		proof, err := verifier.VerifyGitOpsRevision(ctx, group.Connection, request)
		if err != nil || !validRevisionProof(request, proof) || !reflect.DeepEqual(group.Proof, proof) {
			return out, invalid("runtime final common GitOps revision drift")
		}
	}
	out.ObservedAt = time.Now().UTC()
	out.Hash = devRuntimeResultHash(out)
	if err := validateDevRuntimeResult(proof, out); err != nil {
		return out, err
	}
	return out, nil
}
func validateRuntimeGitOps(c domain.DevRuntimeComponent, g domain.DevRuntimeGitOpsObservation, p delivery.GitOpsRevisionProof) error {
	if e := domain.ValidateDevRuntimeGitOps(c, g, p); e != nil {
		return invalid("%v", e)
	}
	return nil
}

func validateRuntimeRole(c domain.DevRuntimeComponent, scope delivery.RuntimeVerificationScope, r delivery.RuntimeSnapshot, head string) error {
	return validateRuntimeRoleAt(c, scope, r, head, time.Now().UTC())
}
func validateRuntimeRoleAt(c domain.DevRuntimeComponent, scope delivery.RuntimeVerificationScope, r delivery.RuntimeSnapshot, head string, now time.Time) error {
	if e := domain.ValidateDevRuntimeRoleAt(c, scope, r, head, now); e != nil {
		return invalid("%v", e)
	}
	return nil
}

func validRuntimeImageID(v, repository, digest string) bool {
	return domain.ValidDevRuntimeImageID(v, repository, digest)
}

func validateDevRuntimeResult(p domain.DevRuntimeProof, r domain.DevRuntimeResult) error {
	return validateDevRuntimeResultAt(p, r, time.Now().UTC())
}
func validateDevRuntimeResultAt(p domain.DevRuntimeProof, r domain.DevRuntimeResult, now time.Time) error {
	if e := domain.ValidateDevRuntimeResultAt(p, r, now); e != nil {
		return invalid("%v", e)
	}
	return nil
}

func (s *Service) finishDevRuntime(ctx context.Context, op domain.ExternalOperation, token string, result *domain.DevRuntimeResult) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token || terminalOperation(current.Status) {
			return invalid("runtime observation lease lost")
		}
		expectedHash := ""
		if op.DevRuntime != nil {
			expectedHash = op.DevRuntime.SnapshotHash
		}
		now := time.Now().UTC()
		valid := op.DevRuntime != nil && ctx.Err() == nil && current.LeaseUntil.After(now) && s.devRuntimeAllowed(st, op, true) == nil && result != nil
		if valid {
			valid = validateDevRuntimeResult(*op.DevRuntime, *result) == nil && result.ObservedAt.Before(now.Add(time.Nanosecond)) && now.Sub(result.ObservedAt) <= 5*time.Minute
		}
		if !valid {
			reason := "EVIDENCE_UNAVAILABLE"
			if result != nil && validRuntimeReason(result.Reason) {
				reason = result.Reason
			}
			if ctx.Err() != nil || !current.LeaseUntil.After(now) || s.devRuntimeAllowed(st, op, true) != nil {
				reason = "AUTHORITY_CHANGED"
			}
			result = &domain.DevRuntimeResult{ExpectedHash: expectedHash, Outcome: "UNVERIFIED", ObservedAt: now, Reason: reason}
			result.Hash = devRuntimeResultHash(*result)
		}
		encoded, err := json.Marshal(struct {
			Proof  *domain.DevRuntimeProof
			Result *domain.DevRuntimeResult
		}{op.DevRuntime, result})
		if err != nil || len(encoded) > devRuntimeMaxBytes {
			return invalid("runtime proof/result bound exceeded")
		}
		current.DevRuntimeResult = result
		current.Status = "FAILED"
		current.Phase = "RUNTIME_UNVERIFIED"
		current.DeploymentState = "RUNTIME_UNVERIFIED"
		current.Detail = "Runtime unverified: incomplete, changed or unavailable exact read-only evidence; reservation retained"
		current.Detail += " (" + result.Reason + ")"
		current.Error = current.Detail
		if valid {
			current.Status = "SUCCEEDED"
			current.Phase = "RUNTIME_VERIFIED"
			current.DeploymentState = "RUNTIME_VERIFIED"
			current.Detail = "Configured candidate runtime matched at observation; functional acceptance and restoration remain unverified, reservation retained"
			current.Error = ""
		}
		current.CurrentStep = current.Phase
		current.UpdatedAt = now
		current.FinishedAt = &now
		current.LeaseOwner = ""
		current.LeaseUntil = time.Time{}
		st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, Actor: "system/runtime-verifier", CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, Phase: current.Phase, Status: current.Status, Detail: current.Detail, Evidence: map[string]string{"expected_hash": expectedHash, "result_hash": result.Hash}})
		return nil
	})
}

func exactRuntimeImage(v, repository, digest string) bool {
	return domain.ExactDevRuntimeImage(v, repository, digest)
}

// Stable actionable categories only; raw adapter errors and payloads never persist.
func runtimeFailureReason(err error) string {
	if err == nil {
		return "EVIDENCE_UNAVAILABLE"
	}
	v := err.Error()
	switch {
	case strings.Contains(v, "ancestry"), strings.Contains(v, "common GitOps revision"):
		return "GITOPS_ANCESTRY_UNVERIFIED"
	case strings.Contains(v, "drift"), strings.Contains(v, "target changed"):
		return "GITOPS_TARGET_DRIFT"
	case strings.Contains(v, "GitOps read"), strings.Contains(v, "target unavailable"):
		return "GITOPS_UNAVAILABLE"
	case strings.Contains(v, "role"), strings.Contains(v, "pods"):
		return "RUNTIME_ROLE_MISMATCH"
	case strings.Contains(v, "bound"):
		return "PROOF_BOUNDS_EXCEEDED"
	default:
		return "AUTHORITY_CHANGED"
	}
}
func validRuntimeReason(v string) bool {
	return slices.Contains([]string{"EVIDENCE_UNAVAILABLE", "GITOPS_ANCESTRY_UNVERIFIED", "GITOPS_TARGET_DRIFT", "GITOPS_UNAVAILABLE", "RUNTIME_ROLE_MISMATCH", "PROOF_BOUNDS_EXCEEDED", "AUTHORITY_CHANGED"}, v)
}
