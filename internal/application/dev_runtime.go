package application

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

const devRuntimeKind = "DEV_CANDIDATE_RUNTIME_VERIFY"
const devRuntimeMaxBytes = 256 * 1024

func devRuntimeProofHash(p domain.DevRuntimeProof) string {
	p.SnapshotHash = ""
	return workerconfig.Fingerprint(p)
}
func devRuntimeResultHash(p domain.DevRuntimeResult) string {
	p.Hash = ""
	return workerconfig.Fingerprint(p)
}
func runtimeActivationHash(st *domain.State, activation domain.ExternalOperation) string {
	children := []struct{ ID, Proof, Result string }{}
	for _, cid := range activation.ChildIDs {
		child := operationByID(st, cid)
		if child == nil {
			return ""
		}
		children = append(children, struct{ ID, Proof, Result string }{cid, child.DevActivationPRHash, child.DevActivationResultHash})
	}
	return workerconfig.Fingerprint(struct {
		Proof    *domain.DevActivationProof
		Children any
	}{activation.DevActivation, children})
}
func devRuntimeInput(c domain.Command) (domain.DevRuntimeRequest, string, error) {
	var in domain.DevRuntimeRequest
	if c.ProductID == "" || strings.TrimSpace(c.Actor) == "" || c.ID != "" || c.FeatureID != "" || c.IntegrationID != "" || c.GateID != "" || c.CheckID != "" || c.ResultID != "" {
		return in, "", invalid("runtime verification requires attributed actor and Product scope only")
	}
	for k := range c.Data {
		if k != "activation_operation_id" && k != "request_key" && k != "expected_selection" {
			return in, "", invalid("unknown runtime verification field")
		}
	}
	if err := decode(c.Data, &in); err != nil {
		return in, "", err
	}
	if in.ActivationOperationID == "" || len(in.ActivationOperationID) > 128 || len(in.RequestKey) < 16 || len(in.RequestKey) > 128 || strings.TrimSpace(in.RequestKey) != in.RequestKey || strings.ContainsFunc(in.RequestKey, func(r rune) bool { return r < 32 || r == 127 }) || in.ExpectedSelection == nil {
		return in, "", invalid("exact activation, request_key16..128 and selection required")
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
	return in, workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevRuntimeRequest
	}{c.Action, c.ProductID, c.Actor, in}), nil
}
func devRuntimeReplay(st *domain.State, c domain.Command, in domain.DevRuntimeRequest, hash string) (*domain.ExternalOperation, error) {
	for i := range st.Operations {
		op := &st.Operations[i]
		if op.Kind == devBaselineKind && op.RequestKey == in.RequestKey {
			return nil, invalid("request_key already binds a baseline capture")
		}
		if op.RequestKey != in.RequestKey {
			continue
		}
		if op.Kind != devRuntimeKind || op.ProductID != c.ProductID || op.RequestedBy != c.Actor || op.RequestHash != hash {
			return nil, invalid("request_key already binds another runtime scope, payload or owner")
		}
		return op, nil
	}
	return nil, nil
}
func runtimeRoleKey(v delivery.RuntimeVerificationScope) string {
	return v.Context + "\x00" + v.Namespace + "\x00" + v.Deployment + "\x00" + v.Container
}
func (s *Service) runtimeCoverage(product, environment, application, cluster, namespace string) ([]delivery.RuntimeVerificationScope, error) {
	out := []delivery.RuntimeVerificationScope{}
	seen := map[string]bool{}
	group := ""
	for _, observer := range s.runtimeObservers {
		target := observer.TargetIdentity()
		if target.ProductID != product || target.EnvironmentID != environment || target.ApplicationID != application {
			continue
		}
		verifier, ok := observer.(delivery.RuntimeVerificationObserver)
		if !ok {
			return nil, invalid("all configured runtime roles require verification capability")
		}
		scope, err := verifier.RuntimeVerificationScope()
		if err != nil {
			return nil, invalid("runtime role binding unavailable")
		}
		if scope.ProductID != product || scope.EnvironmentID != environment || scope.ApplicationID != application || scope.Context != cluster || scope.Namespace != namespace || scope.Context == "" || scope.Namespace == "" || scope.Deployment == "" || scope.Container == "" || scope.FluxNamespace == "" || scope.Kustomization == "" || scope.VerificationGroup == "" || !validBindingHash(scope.BindingHash) {
			return nil, invalid("runtime role scope incomplete or target mismatch")
		}
		for _, v := range []string{scope.ProductID, scope.EnvironmentID, scope.ApplicationID, scope.Context, scope.Namespace, scope.Deployment, scope.Container, scope.FluxNamespace, scope.Kustomization, scope.VerificationGroup} {
			if len(v) > 1024 || strings.ContainsFunc(v, func(r rune) bool { return r < 32 || r == 127 }) {
				return nil, invalid("runtime role scope exceeds bounds")
			}
		}
		key := runtimeRoleKey(scope)
		if seen[key] || group != "" && group != scope.VerificationGroup {
			return nil, invalid("duplicate runtime role or mixed verification groups")
		}
		seen[key] = true
		group = scope.VerificationGroup
		out = append(out, scope)
		if len(out) > 64 {
			return nil, invalid("runtime role bound exceeded")
		}
	}
	if len(out) == 0 {
		return nil, invalid("complete operator runtime verification group required")
	}
	sort.Slice(out, func(i, j int) bool { return runtimeRoleKey(out[i]) < runtimeRoleKey(out[j]) })
	return out, nil
}
func validBindingHash(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, r := range v {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func (s *Service) runtimeExpectedProof(st *domain.State, activation *domain.ExternalOperation, in domain.DevRuntimeRequest, generation uint64) (domain.DevRuntimeProof, error) {
	out := domain.DevRuntimeProof{Request: in, Generation: generation}
	if activation == nil || activation.Kind != devActivationKind || activation.Status != "SUCCEEDED" || activation.DeploymentState != "GITOPS_APPLIED" || activation.DevActivation == nil {
		return out, invalid("successful exact candidate Dev activation required")
	}
	if err := s.devActivationAllowed(st, *activation); err != nil {
		return out, err
	}
	if err := checkSelection(environmentByID(st, activation.EnvironmentID), in.ExpectedSelection); err != nil {
		return out, err
	}
	if _, ok := s.provider.(delivery.GitOpsRevisionVerifier); !ok {
		return out, invalid("read-only GitOps revision verifier required")
	}
	out.ActivationHash = runtimeActivationHash(st, *activation)
	roles := 0
	seenApps := map[string]bool{}
	allRoles := map[string]bool{}
	for _, cid := range activation.ChildIDs {
		child := operationByID(st, cid)
		if child == nil || child.Status != "SUCCEEDED" || child.DeploymentState != "GITOPS_APPLIED" || child.Snapshot == nil || child.ExistingArtifact == nil || child.GitOpsResult == nil || !fullSHA.MatchString(child.GitOpsResult.CommitSHA) || !fullSHA.MatchString(child.GitOpsResult.BlobSHA) || seenApps[child.ApplicationID] {
			return out, invalid("complete immutable activated component required")
		}
		seenApps[child.ApplicationID] = true
		snap := child.Snapshot
		scopes, err := s.runtimeCoverage(activation.ProductID, activation.EnvironmentID, child.ApplicationID, snap.Environment.Cluster, snap.Environment.Namespace)
		if err != nil {
			return out, err
		}
		roles += len(scopes)
		for _, scope := range scopes {
			key := runtimeRoleKey(scope)
			if allRoles[key] {
				return out, invalid("runtime workload claimed by multiple components")
			}
			allRoles[key] = true
		}
		authority, e := runtimeGitOpsAuthority(snap.GitOpsConnection)
		if e != nil {
			return out, e
		}
		out.Components = append(out.Components, domain.DevRuntimeComponent{AuthorityHash: authority, ApplicationID: child.ApplicationID, ChildID: cid, ArtifactID: child.ExistingArtifact.ID, ImageRepository: child.ExistingArtifact.ImageRepository, Digest: child.ExistingArtifact.Digest, GitOps: snap.GitOpsRequest, GitOpsCommit: child.GitOpsResult.CommitSHA, ConfigHash: workerconfig.Fingerprint(snap), Scopes: scopes})
	}
	if len(out.Components) == 0 || len(out.Components) > 16 || roles > 64 {
		return out, invalid("runtime verification component/role bounds exceeded")
	}
	out.SnapshotHash = devRuntimeProofHash(out)
	if !devRuntimeProofFits(out) {
		return out, invalid("runtime expected proof bound exceeded")
	}
	return out, nil
}

func devRuntimeProofFits(proof domain.DevRuntimeProof) bool {
	failure := domain.DevRuntimeResult{ExpectedHash: proof.SnapshotHash, Outcome: "UNVERIFIED", Reason: strings.Repeat("x", 64), ObservedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), Hash: "sha256:" + strings.Repeat("0", 64)}
	b, err := json.Marshal(struct {
		Proof  domain.DevRuntimeProof
		Result domain.DevRuntimeResult
	}{proof, failure})
	return err == nil && len(b) <= devRuntimeMaxBytes
}
func latestDevRuntime(st *domain.State, activation string) (*domain.ExternalOperation, bool) {
	var latest *domain.ExternalOperation
	valid := true
	for i := range st.Operations {
		v := &st.Operations[i]
		if v.Kind != devRuntimeKind || v.ParentID != activation {
			continue
		}
		if v.DevRuntime == nil || v.DevRuntime.Generation == 0 {
			valid = false
			continue
		}
		if latest == nil || v.DevRuntime.Generation > latest.DevRuntime.Generation {
			latest = v
		} else if v.DevRuntime.Generation == latest.DevRuntime.Generation {
			valid = false
		}
	}
	return latest, valid
}
func (s *Service) devRuntimeAllowed(st *domain.State, op domain.ExternalOperation, latest bool) error {
	current := operationByID(st, op.ID)
	p := op.DevRuntime
	if p == nil || current == nil || current.DevRuntime == nil || op.Kind != devRuntimeKind || op.ReservationQuarantined || current.ReservationQuarantined || op.ParentID != p.Request.ActivationOperationID || op.ProductID != current.ProductID || current.Kind != op.Kind || current.ParentID != op.ParentID || current.EnvironmentID != op.EnvironmentID || current.ReservationEpoch != op.ReservationEpoch || op.RequestedBy != current.RequestedBy || op.RequestKey != p.Request.RequestKey || op.RequestHash != current.RequestHash || !reflect.DeepEqual(p, current.DevRuntime) || p.Generation == 0 || p.SnapshotHash == "" || p.SnapshotHash != devRuntimeProofHash(*p) || op.Native != nil || op.Snapshot != nil || op.DevActivation != nil || op.MainCandidate != nil || len(op.ChildIDs) != 0 || op.BuildOnly {
		return invalid("runtime verification immutable proof unavailable")
	}
	hash := workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevRuntimeRequest
	}{"verify_candidate_dev_runtime", op.ProductID, op.RequestedBy, p.Request})
	if hash != op.RequestHash {
		return invalid("runtime verification request proof changed")
	}
	activation := operationByID(st, p.Request.ActivationOperationID)
	if activation == nil || activation.ProductID != op.ProductID || activation.RequestedBy != op.RequestedBy || activation.EnvironmentID != op.EnvironmentID || activation.ReservationEpoch != op.ReservationEpoch {
		return invalid("same candidate reservation owner and epoch required")
	}
	expected, err := s.runtimeExpectedProof(st, activation, p.Request, p.Generation)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, *p) {
		return invalid("runtime activation, config or coverage changed")
	}
	if latest {
		v, ok := latestDevRuntime(st, activation.ID)
		if !ok || v == nil || v.ID != op.ID {
			return invalid("runtime verification superseded")
		}
	}
	return nil
}
func (s *Service) executeDevRuntime(ctx context.Context, c domain.Command) (any, error) {
	in, hash, err := devRuntimeInput(c)
	if err != nil {
		return nil, err
	}
	var out domain.ExternalOperation
	err = s.store.Update(ctx, func(st *domain.State) error {
		if old, e := devRuntimeReplay(st, c, in, hash); e != nil {
			return e
		} else if old != nil {
			out = *old
			return nil
		}
		activation := operationByID(st, in.ActivationOperationID)
		if activation == nil || activation.ProductID != c.ProductID || activation.RequestedBy != c.Actor {
			return invalid("same attributed candidate Dev owner required")
		}
		generation := uint64(1)
		for _, v := range st.Operations {
			if v.Kind == devRuntimeKind && v.ParentID == activation.ID {
				if v.DevRuntime == nil || v.DevRuntime.Generation == 0 || v.DevRuntime.Generation == math.MaxUint64 {
					return invalid("runtime generation unavailable or exhausted")
				}
				if v.DevRuntime.Generation >= generation {
					generation = v.DevRuntime.Generation + 1
				}
			}
		}
		proof, e := s.runtimeExpectedProof(st, activation, in, generation)
		if e != nil {
			return e
		}
		m := meta(c)
		out = domain.ExternalOperation{Meta: m, Kind: devRuntimeKind, ParentID: activation.ID, RequestedBy: c.Actor, EnvironmentID: activation.EnvironmentID, ReservationEpoch: activation.ReservationEpoch, RequestKey: in.RequestKey, RequestHash: hash, DevRuntime: &proof, Status: "PENDING", Phase: "VERIFY_RUNTIME", CurrentStep: "VERIFY_RUNTIME", Detail: "Read-only candidate runtime verification pending; reservation retained", NextAttemptAt: m.CreatedAt}
		st.Operations = append(st.Operations, out)
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: m.CreatedAt, EntityID: out.ID, ProductID: c.ProductID, Data: c})
		return nil
	})
	return out, err
}

// Current truth is separate from the durable historical receipt and baseline maps.
func (s *Service) candidateRuntimeState(st *domain.State, environment, activationID string) map[string]any {
	out := map[string]any{"candidate_runtime_verified": false}
	if activationID == "" {
		hold := activeDevReservation(st, environment)
		if hold == nil || hold.DevReservation == nil {
			return out
		}
		for _, op := range st.Operations {
			if op.Kind == devActivationKind && op.DevActivation != nil && op.DevActivation.ReservationOperationID == hold.ID {
				activationID = op.ID
				break
			}
		}
	}
	op, ok := latestDevRuntime(st, activationID)
	if op == nil {
		return out
	}
	out["candidate_runtime_verification_id"] = op.ID
	out["candidate_runtime_verification_status"] = op.Status
	if op.DevRuntimeResult != nil {
		out["candidate_runtime_observed_at"] = op.DevRuntimeResult.ObservedAt
	}
	if !ok || op.Status != "SUCCEEDED" || op.Phase != "RUNTIME_VERIFIED" || op.DeploymentState != "RUNTIME_VERIFIED" || op.DevRuntimeResult == nil || op.DevRuntimeResult.Outcome != "VERIFIED" || op.DevRuntimeResult.ExpectedHash != op.DevRuntime.SnapshotHash || op.DevRuntimeResult.Hash == "" || op.DevRuntimeResult.Hash != devRuntimeResultHash(*op.DevRuntimeResult) {
		return out
	}
	now := time.Now().UTC()
	at := op.DevRuntimeResult.ObservedAt
	if at.IsZero() || at.After(now) || now.Sub(at) > 5*time.Minute {
		return out
	}
	if s.devRuntimeAllowed(st, *op, true) != nil || validateDevRuntimeResult(*op.DevRuntime, *op.DevRuntimeResult) != nil {
		return out
	}
	out["candidate_runtime_verified"] = true
	return out
}
