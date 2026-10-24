package domain

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Baseline images are owned recovery roots, not caller-releasable pins. Failed,
// expired and superseded reads cannot erase a physical image already observed.
func (c *retentionCollector) baselineRoots() {
	for _, o := range c.st.Operations {
		if o.ProductID != c.product || (o.Kind != "DEV_CANDIDATE_BASELINE_CAPTURE" && o.DevBaseline == nil && o.DevBaselineResult == nil) {
			continue
		}
		c.evidence = append(c.evidence, c.baselineEvidence(o))
		valid := c.baselineCaptureScope(o)
		if valid && c.baselineHoldSettled(o) {
			continue
		}
		if o.Status != "SUCCEEDED" {
			c.block("baseline_capture_unverified")
		}
		reason := "dev_baseline:" + o.ID
		if len(o.ID) > 128 || strings.ContainsAny(o.ID, "\r\n\t") {
			reason = "dev_baseline:" + checkHash(o.ID)
		}
		add := func(app, repo, digest string) {
			if digest == "" {
				return
			}
			image := c.image(repo, digest, app, reason)
			if image != nil && !valid {
				image.Blockers = append(image.Blockers, "baseline_capture_authority_unknown")
			}
		}
		if p := o.DevBaseline; p != nil {
			for _, part := range p.Components {
				add(part.ApplicationID, part.ImageRepository, part.Digest)
				if part.ObservedGitOps != nil {
					add(part.ApplicationID, part.ObservedGitOps.ImageRepository, part.ObservedGitOps.Digest)
				}
			}
		}
		if r := o.DevBaselineResult; r != nil {
			for _, part := range r.Components {
				add(part.ApplicationID, part.ImageRepository, part.Digest)
				add(part.ApplicationID, part.GitOps.ImageRepository, part.GitOps.Digest)
			}
			if r.Runtime != nil {
				for _, part := range r.Runtime.Components {
					add(part.ApplicationID, part.GitOps.ImageRepository, part.GitOps.Digest)
				}
			}
		}
	}
}

func (c *retentionCollector) baselineHold(o ExternalOperation) (ExternalOperation, bool) {
	id := o.ParentID
	if o.DevBaseline != nil && o.DevBaseline.ReservationOperationID != "" {
		id = o.DevBaseline.ReservationOperationID
	}
	var hold ExternalOperation
	count := 0
	for _, v := range c.st.Operations {
		if v.ID == id {
			hold = v
			count++
		}
	}
	return hold, count == 1 && id != ""
}

func (c *retentionCollector) baselineEvidence(o ExternalOperation) string {
	hold, unique := c.baselineHold(o)
	causal := []string{}
	for _, effect := range c.st.Operations {
		if effect.ProductID != o.ProductID {
			continue
		}
		linked := (effect.DevReservation != nil && effect.DevReservation.ReservationOperationID == hold.ID && hold.ID != "") || (effect.DevActivation != nil && effect.DevActivation.ReservationOperationID == hold.ID && hold.ID != "") || (effect.EnvironmentID == o.EnvironmentID && effect.ReservationEpoch == o.ReservationEpoch && effect.Kind == "DEV_CANDIDATE_DEPLOY")
		uncertain := effect.EnvironmentID == o.EnvironmentID && (effect.ReservationMutationIntent != "" || effect.Phase == "APPLY_UNCERTAIN" || effect.GitOpsPR != nil)
		if linked || uncertain {
			causal = append(causal, checkHash(struct {
				ID, Kind, Status, Phase, Key, RequestHash, Owner, Intent string
				Epoch                                                    uint64
				Quarantined                                              bool
				Reservation                                              *DevReservationProof
				Activation                                               *DevActivationProof
				PR                                                       any
			}{effect.ID, effect.Kind, effect.Status, effect.Phase, effect.RequestKey, effect.RequestHash, effect.RequestedBy, effect.ReservationMutationIntent, effect.ReservationEpoch, effect.ReservationQuarantined, effect.DevReservation, effect.DevActivation, effect.GitOpsPR}))
		}
	}
	if p := o.DevBaseline; p != nil {
		for _, comp := range c.st.Compositions {
			if comp.ID == p.CompositionID {
				causal = append(causal, checkHash(comp))
			}
		}
		for _, part := range p.Components {
			for _, app := range c.st.Applications {
				if app.ID == part.ApplicationID {
					causal = append(causal, checkHash(struct{ ID, Product, Repository string }{app.ID, app.ProductID, app.RepositoryID}))
				}
			}
		}
	}
	return checkHash(struct {
		ID, Product, Environment, Parent, Kind, Status, Phase, RequestHash, Owner                         string
		Epoch                                                                                             uint64
		Proof                                                                                             *DevBaselineProof
		Result                                                                                            *DevBaselineResult
		UniqueHold                                                                                        bool
		HoldID, HoldProduct, HoldEnvironment, HoldKind, HoldStatus, HoldPhase, HoldOwner, HoldRequestHash string
		HoldEpoch                                                                                         uint64
		HoldQuarantined                                                                                   bool
		HoldProof                                                                                         *DevReservationProof
		Causal                                                                                            []string
	}{o.ID, o.ProductID, o.EnvironmentID, o.ParentID, o.Kind, o.Status, o.Phase, o.RequestHash, o.RequestedBy, o.ReservationEpoch, o.DevBaseline, o.DevBaselineResult, unique, hold.ID, hold.ProductID, hold.EnvironmentID, hold.Kind, hold.Status, hold.Phase, hold.RequestedBy, hold.RequestHash, hold.ReservationEpoch, hold.ReservationQuarantined, hold.DevReservation, sortedUnique(causal)})
}

func (c *retentionCollector) baselineCaptureScope(o ExternalOperation) bool {
	fail := func(code string) bool { c.block(code); return false }
	p := o.DevBaseline
	if p == nil || o.Kind != "DEV_CANDIDATE_BASELINE_CAPTURE" {
		return fail("baseline_capture_proof_missing_or_invalid")
	}
	copy := *p
	copy.SnapshotHash = ""
	if p.SnapshotHash == "" || p.SnapshotHash != checkHash(copy) || p.Generation == 0 || len(p.Components) == 0 || len(p.Components) > 16 {
		return fail("baseline_capture_proof_missing_or_invalid")
	}
	if o.Status == "SUCCEEDED" && o.DevBaselineResult == nil {
		return fail("baseline_capture_result_invalid")
	}
	encoded, err := json.Marshal(struct {
		Proof  *DevBaselineProof
		Result *DevBaselineResult
	}{p, o.DevBaselineResult})
	if err != nil || len(encoded) > 256<<10 {
		return fail("baseline_capture_bounds_exceeded")
	}
	hold, unique := c.baselineHold(o)
	if !unique || hold.Kind != "DEV_RESERVATION" || hold.ProductID != o.ProductID || hold.EnvironmentID != o.EnvironmentID || hold.RequestedBy != o.RequestedBy || hold.DevReservation == nil || hold.ReservationEpoch != o.ReservationEpoch || hold.DevReservation.Epoch != o.ReservationEpoch || p.Request.ReservationOperationID != hold.ID || p.ReservationOperationID != hold.ID || (o.ParentID != "" && o.ParentID != hold.ID) || checkHash(hold.DevReservation) != p.ReservationHash {
		return fail("baseline_capture_authority_unknown")
	}
	if hold.ReservationQuarantined || hold.Status != "SUCCEEDED" || hold.Phase != "RESERVED" {
		return fail("baseline_capture_hold_uncertain")
	}
	for _, q := range c.st.NativeQuarantines {
		if q.ProductID == o.ProductID {
			return fail("baseline_capture_hold_uncertain")
		}
	}
	seen := map[string]bool{}
	roles := 0
	pending := false
	for _, part := range p.Components {
		if part.ApplicationID == "" || seen[part.ApplicationID] {
			return fail("baseline_capture_component_scope_invalid")
		}
		seen[part.ApplicationID] = true
		matched := false
		for _, app := range c.st.Applications {
			if app.ID == part.ApplicationID && app.ProductID == o.ProductID && app.RepositoryID == part.RepositoryID {
				matched = true
			}
		}
		if !matched {
			return fail("baseline_capture_component_scope_invalid")
		}
		repo, e := CanonicalImageRepository(part.ImageRepository)
		if e != nil || repo != part.ImageRepository || (part.Digest != "" && !retentionDigest.MatchString(part.Digest)) {
			return fail("baseline_capture_image_scope_invalid")
		}
		if part.Digest == "" {
			pending = true
		}
		roles += len(part.Scopes)
	}
	if roles > 64 {
		return fail("baseline_capture_bounds_exceeded")
	}
	compositionCount := 0
	for _, composition := range c.st.Compositions {
		if composition.ID != p.CompositionID {
			continue
		}
		compositionCount++
		if composition.ProductID != o.ProductID || composition.EnvironmentID != o.EnvironmentID || checkHash(composition) != p.CompositionHash || len(composition.Components) != len(seen) {
			return fail("baseline_capture_composition_scope_invalid")
		}
		for _, part := range composition.Components {
			if !seen[part.ApplicationID] {
				return fail("baseline_capture_composition_scope_invalid")
			}
		}
	}
	if compositionCount != 1 {
		return fail("baseline_capture_composition_scope_invalid")
	}
	if pending {
		c.block("baseline_capture_roots_pending")
	}
	if r := o.DevBaselineResult; r != nil {
		result := *r
		result.Hash = ""
		if r.Hash == "" || r.Hash != checkHash(result) || r.ExpectedHash != p.SnapshotHash {
			return fail("baseline_capture_result_invalid")
		}
		if len(r.Components) > 16 {
			return fail("baseline_capture_bounds_exceeded")
		}
		if r.Runtime != nil {
			roles, pods := 0, 0
			for _, part := range r.Runtime.Components {
				roles += len(part.Roles)
				for _, role := range part.Roles {
					pods += len(role.Pods)
				}
			}
			if len(r.Runtime.Components) > 16 || roles > 64 || pods > 512 {
				return fail("baseline_capture_bounds_exceeded")
			}
		}
	}
	return true
}

// Only the existing exact pre-activation release receipt can settle these roots.
// A future restoration protocol must explicitly extend this condition; neither
// cancelled operations nor a release-shaped record after activation suffice.
func (c *retentionCollector) baselineHoldSettled(o ExternalOperation) bool {
	hold, ok := c.baselineHold(o)
	if !ok || hold.DevReservation == nil {
		return false
	}
	for _, receipt := range c.st.Operations {
		if ValidRestorationRelease(&c.st, receipt, hold) {
			return true
		}
	}
	for _, effect := range c.st.Operations {
		if effect.ProductID != hold.ProductID {
			continue
		}
		if (effect.DevActivation != nil && effect.DevActivation.ReservationOperationID == hold.ID) || (effect.Kind == "DEV_CANDIDATE_DEPLOY" && effect.EnvironmentID == hold.EnvironmentID && effect.ReservationEpoch == hold.ReservationEpoch) {
			c.block("baseline_restore_unverified")
			return false
		}
		if effect.EnvironmentID == hold.EnvironmentID && (effect.ReservationMutationIntent != "" || effect.Phase == "APPLY_UNCERTAIN" || (effect.GitOpsPR != nil && !effect.GitOpsPR.Merged && !strings.EqualFold(effect.GitOpsPR.State, "closed"))) {
			c.block("baseline_hold_effect_uncertain")
			return false
		}
	}
	for _, receipt := range c.st.Operations {
		p := receipt.DevReservation
		if receipt.Kind != "DEV_RESERVATION_RELEASE" || receipt.Status != "SUCCEEDED" || receipt.Phase != "RELEASED" || receipt.ProductID != hold.ProductID || receipt.EnvironmentID != hold.EnvironmentID || receipt.RequestedBy != hold.RequestedBy || receipt.ReservationQuarantined || p == nil || p.ReservationOperationID != hold.ID || p.Request.ReservationOperationID != hold.ID || p.Request.RequestKey != receipt.RequestKey || p.Request.ExpectedSelection == nil || *p.Request.ExpectedSelection != hold.DevReservation.ReservedSelection || receipt.ReservationEpoch != hold.DevReservation.Epoch {
			continue
		}
		expected := checkHash(struct {
			Action, Product, Actor string
			Input                  DevReservationRequest
		}{"release_candidate_dev", receipt.ProductID, receipt.RequestedBy, p.Request})
		if expected != receipt.RequestHash {
			continue
		}
		copy := *p
		copy.Request = hold.DevReservation.Request
		copy.ReservationOperationID = ""
		if reflect.DeepEqual(copy, *hold.DevReservation) {
			return true
		}
	}
	return false
}

func baselineInScope(o ExternalOperation, inScope func(string) bool) bool {
	if p := o.DevBaseline; p != nil {
		for _, part := range p.Components {
			if inScope(part.ImageRepository) || (part.ObservedGitOps != nil && inScope(part.ObservedGitOps.ImageRepository)) {
				return true
			}
		}
	}
	if r := o.DevBaselineResult; r != nil {
		for _, part := range r.Components {
			if inScope(part.ImageRepository) || inScope(part.GitOps.ImageRepository) {
				return true
			}
		}
		if r.Runtime != nil {
			for _, part := range r.Runtime.Components {
				if inScope(part.GitOps.ImageRepository) {
					return true
				}
			}
		}
	}
	return false
}
