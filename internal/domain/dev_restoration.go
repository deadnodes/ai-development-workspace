package domain

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"releasecontrol/internal/delivery"
	"slices"
	"strings"
	"time"
)

// DevRestorationRequest deliberately carries no caller supplied physical proof.
type DevRestorationRequest struct {
	ReservationOperationID             string                `json:"reservation_operation_id,omitempty"`
	RestorationOperationID             string                `json:"restoration_operation_id,omitempty"`
	RestorationVerificationOperationID string                `json:"restoration_verification_operation_id,omitempty"`
	ExpectedPlanHash                   string                `json:"expected_plan_hash,omitempty"`
	RequestKey                         string                `json:"request_key"`
	ExpectedSelection                  *EnvironmentSelection `json:"expected_selection"`
}

// The original capture and hold remain immutable. This grants only one bounded
// recovery generation, never renewed candidate or production execution rights.
type DevRestorationProof struct {
	Request                DevRestorationRequest  `json:"request"`
	ReservationOperationID string                 `json:"reservation_operation_id"`
	ReservationHash        string                 `json:"reservation_hash"`
	BaselineCaptureID      string                 `json:"baseline_capture_id"`
	BaselineCaptureHash    string                 `json:"baseline_capture_hash"`
	Generation             uint64                 `json:"generation"`
	Epoch                  uint64                 `json:"epoch"`
	Selection              EnvironmentSelection   `json:"selection"`
	Deadline               time.Time              `json:"deadline"`
	Components             []DevBaselineComponent `json:"components"`
	Effects                []DevRestorationEffect `json:"effects"`
	SnapshotHash           string                 `json:"snapshot_hash"`
}
type DevRestorationEffect struct {
	OperationID string                      `json:"operation_id"`
	Connection  delivery.Connection         `json:"connection"`
	Batch       *delivery.GitOpsBatchApply  `json:"batch,omitempty"`
	PullRequest *delivery.GitOpsPullRequest `json:"pull_request,omitempty"`
	NoIntent    bool                        `json:"no_intent"`
}
type DevRestorationPlan struct {
	ExpectedHash   string                 `json:"expected_hash"`
	SettlementHash string                 `json:"settlement_hash"`
	Components     []DevRestorationMember `json:"components"`
	Groups         []DevRestorationGroup  `json:"groups"`
	Hash           string                 `json:"hash"`
}
type DevRestorationMember struct {
	ApplicationID string                      `json:"application_id"`
	GroupID       string                      `json:"group_id"`
	Original      DevRuntimeGitOpsObservation `json:"original"`
	Current       DevRuntimeGitOpsObservation `json:"current"`
	Selector      delivery.GitOpsRequest      `json:"selector"`
	NoWrite       bool                        `json:"no_write"`
}
type DevRestorationGroup struct {
	MergedAt    *time.Time                    `json:"merged_at,omitempty"`
	ID          string                        `json:"id"`
	LeaderID    string                        `json:"leader_id"`
	Connection  delivery.Connection           `json:"connection"`
	Repository  string                        `json:"repository"`
	Ref         string                        `json:"ref"`
	Batch       *delivery.GitOpsBatchApply    `json:"batch,omitempty"`
	IntentHash  string                        `json:"intent_hash,omitempty"`
	SendStarted bool                          `json:"send_started"`
	PullRequest *delivery.GitOpsPullRequest   `json:"pull_request,omitempty"`
	Result      []DevRuntimeGitOpsObservation `json:"result,omitempty"`
}

// Attribution is not an authenticated human text/voice decision.
type DevRestorationConfirmation struct {
	Request         DevRestorationRequest `json:"request"`
	RestorationHash string                `json:"restoration_hash"`
	Plan            DevRestorationPlan    `json:"plan"`
	TrustGrade      string                `json:"trust_grade"`
	ConfirmedAt     time.Time             `json:"confirmed_at"`
	Hash            string                `json:"hash"`
}
type DevRestorationVerification struct {
	Request          DevRestorationRequest        `json:"request"`
	RestorationHash  string                       `json:"restoration_hash"`
	PlanHash         string                       `json:"plan_hash"`
	ConfirmationID   string                       `json:"confirmation_id"`
	ConfirmationHash string                       `json:"confirmation_hash"`
	SettlementHash   string                       `json:"settlement_hash"`
	Generation       uint64                       `json:"generation"`
	Runtime          DevRuntimeProof              `json:"runtime"`
	Availability     []DevBaselineComponentResult `json:"availability"`
	Components       []DevBaselineComponent       `json:"components"`
	Hash             string                       `json:"hash"`
}
type DevRestorationRelease struct {
	Availability           []DevBaselineComponentResult `json:"availability"`
	AvailabilityHash       string                       `json:"availability_hash"`
	Request                DevRestorationRequest        `json:"request"`
	ReservationOperationID string                       `json:"reservation_operation_id"`
	ReservationHash        string                       `json:"reservation_hash"`
	RestorationHash        string                       `json:"restoration_hash"`
	PlanHash               string                       `json:"plan_hash"`
	ConfirmationID         string                       `json:"confirmation_id"`
	ConfirmationHash       string                       `json:"confirmation_hash"`
	VerificationHash       string                       `json:"verification_hash"`
	ResultHash             string                       `json:"result_hash"`
	SettlementHash         string                       `json:"settlement_hash"`
	RecoveryEpoch          uint64                       `json:"recovery_epoch"`
	RecoverySelection      EnvironmentSelection         `json:"recovery_selection"`
	ReleasedEpoch          uint64                       `json:"released_epoch"`
	ReleasedSelection      EnvironmentSelection         `json:"released_selection"`
	Hash                   string                       `json:"hash"`
}

func IsRestorationAction(action string) bool {
	switch action {
	case "request_candidate_dev_restoration", "confirm_candidate_dev_restoration", "verify_candidate_dev_restoration", "release_candidate_dev_after_restoration":
		return true
	}
	return false
}

// ValidRestorationRelease recognizes an exact typed successor chain, never a
// status-shaped receipt. Imported/quarantined or malformed authority cannot settle.
func ValidRestorationRelease(st *State, receipt, hold ExternalOperation) bool {
	for _, q := range st.NativeQuarantines {
		if q.ProductID == hold.ProductID {
			return false
		}
	}
	p := receipt.DevRestorationRelease
	if p == nil || hold.DevReservation == nil || hold.ReservationQuarantined || receipt.ReservationQuarantined || receipt.Kind != "DEV_RESTORATION_RELEASE" || receipt.Status != "SUCCEEDED" || receipt.Phase != "RELEASED" || receipt.ProductID != hold.ProductID || receipt.EnvironmentID != hold.EnvironmentID || receipt.RequestedBy != hold.RequestedBy || p.ReservationOperationID != hold.ID || p.ReservationHash != checkHash(hold.DevReservation) || p.Hash == "" {
		return false
	}
	copy := *p
	copy.Hash = ""
	if p.Hash != checkHash(copy) || p.Request.RequestKey != receipt.RequestKey || p.Request.ExpectedSelection == nil || *p.Request.ExpectedSelection != p.RecoverySelection || p.RecoveryEpoch != receipt.ReservationEpoch || p.ReleasedEpoch != p.RecoveryEpoch+1 || p.ReleasedEpoch == 0 || p.ReleasedSelection.Generation != p.RecoverySelection.Generation+1 || p.ReleasedSelection.Generation == 0 || p.ReleasedSelection.CompositionID != p.RecoverySelection.CompositionID || p.ReleasedSelection.OperationID != p.RecoverySelection.OperationID {
		return false
	}
	requestHash := checkHash(struct {
		Action, Product, Actor string
		Input                  DevRestorationRequest
	}{"release_candidate_dev_after_restoration", receipt.ProductID, receipt.RequestedBy, p.Request})
	if receipt.RequestHash != requestHash {
		return false
	}
	find := func(id string) *ExternalOperation {
		var out *ExternalOperation
		for i := range st.Operations {
			if st.Operations[i].ID == id {
				if out != nil {
					return nil
				}
				out = &st.Operations[i]
			}
		}
		return out
	}
	recovery := find(p.Request.RestorationOperationID)
	confirmation := find(p.ConfirmationID)
	verification := find(p.Request.RestorationVerificationOperationID)
	if recovery == nil || confirmation == nil || verification == nil || recovery.DevRestoration == nil || recovery.DevRestorationPlan == nil || confirmation.DevRestorationConfirmation == nil || verification.DevRestorationVerification == nil || verification.DevRuntimeResult == nil {
		return false
	}
	for _, o := range []*ExternalOperation{recovery, confirmation, verification} {
		if o.ReservationQuarantined || o.ProductID != hold.ProductID || o.EnvironmentID != hold.EnvironmentID || o.RequestedBy != hold.RequestedBy || o.ReservationEpoch != p.RecoveryEpoch || o.Status != "SUCCEEDED" {
			return false
		}
	}
	r := recovery.DevRestoration
	rp := *r
	rp.SnapshotHash = ""
	plan := *recovery.DevRestorationPlan
	plan.Hash = ""
	plan.Groups = append([]DevRestorationGroup(nil), plan.Groups...)
	for i := range plan.Groups {
		plan.Groups[i].Result = nil
		plan.Groups[i].MergedAt = nil
	}
	if recovery.Kind != "DEV_CANDIDATE_RESTORATION" || recovery.ParentID != hold.ID || recovery.Phase != "GITOPS_APPLIED" || r.ReservationOperationID != hold.ID || r.ReservationHash != p.ReservationHash || r.SnapshotHash != p.RestorationHash || r.SnapshotHash != checkHash(rp) || r.Epoch != p.RecoveryEpoch || r.Selection != p.RecoverySelection || r.Generation == 0 || recovery.DevRestorationPlan.Hash != p.PlanHash || p.PlanHash != checkHash(plan) || receipt.ParentID != recovery.ID {
		return false
	}
	c := confirmation.DevRestorationConfirmation
	cp := *c
	cp.Hash = ""
	if confirmation.Kind != "DEV_RESTORATION_CONFIRMATION" || confirmation.ParentID != recovery.ID || confirmation.Phase != "ATTRIBUTED_CONTROL_CONFIRMATION" || c.TrustGrade != "ATTRIBUTED_CONTROL_CONFIRMATION" || c.Hash != p.ConfirmationHash || c.Hash != checkHash(cp) || c.RestorationHash != p.RestorationHash || c.Plan.Hash != p.PlanHash {
		return false
	}
	for _, g := range recovery.DevRestorationPlan.Groups {
		if g.Batch != nil && (g.MergedAt == nil || !g.MergedAt.After(c.ConfirmedAt) || verification.DevRuntimeResult != nil && g.MergedAt.After(verification.DevRuntimeResult.ObservedAt)) {
			return false
		}
	}
	v := verification.DevRestorationVerification
	vp := *v
	vp.Hash = ""
	result := *verification.DevRuntimeResult
	result.Hash = ""
	if verification.Kind != "DEV_RESTORATION_VERIFICATION" || verification.ParentID != recovery.ID || verification.Phase != "RESTORATION_VERIFIED" || v.Generation == 0 || v.Hash != p.VerificationHash || v.Hash != checkHash(vp) || v.RestorationHash != p.RestorationHash || v.PlanHash != p.PlanHash || v.ConfirmationID != confirmation.ID || v.ConfirmationHash != p.ConfirmationHash || v.SettlementHash == "" || v.SettlementHash != p.SettlementHash || verification.DevRuntimeResult.Outcome != "VERIFIED" || verification.DevRuntimeResult.Hash != p.ResultHash || p.ResultHash != checkHash(result) || receipt.FinishedAt == nil || verification.DevRuntimeResult.ObservedAt.After(*receipt.FinishedAt) || receipt.FinishedAt.Sub(verification.DevRuntimeResult.ObservedAt) > 5*time.Minute {
		return false
	}
	for _, o := range st.Operations {
		if o.Kind == "DEV_CANDIDATE_RESTORATION" && o.ParentID == hold.ID && (o.DevRestoration == nil || o.DevRestoration.Generation >= r.Generation && o.ID != recovery.ID) {
			return false
		}
		if o.Kind == "DEV_RESTORATION_VERIFICATION" && o.ParentID == recovery.ID && (o.DevRestorationVerification == nil || o.DevRestorationVerification.Generation >= v.Generation && o.ID != verification.ID) {
			return false
		}
	}
	return validRestorationSuccessorEvidence(st, receipt, hold, *recovery, *confirmation, *verification)
}

func IsRestorationOperation(o ExternalOperation) bool {
	return o.DevRestorationUnapproved || o.Kind == "DEV_CANDIDATE_RESTORATION" || o.Kind == "DEV_RESTORATION_CONFIRMATION" || o.Kind == "DEV_RESTORATION_VERIFICATION" || o.Kind == "DEV_RESTORATION_RELEASE" || o.DevRestoration != nil || o.DevRestorationPlan != nil || o.DevRestorationConfirmation != nil || o.DevRestorationVerification != nil || o.DevRestorationRelease != nil || len(o.DevRestorationAvailability) > 0
}

// Settlement suppresses only images bound to a unique, validated successor
// chain. Auxiliary or malformed nested references never borrow a parent's hold.
func restorationChangeApplication(st *State, product, id string) string {
	app, count := "", 0
	for _, a := range st.Applications {
		if a.ID == id && a.ProductID == product {
			app = a.ID
			count++
		}
	}
	for _, o := range st.Operations {
		if o.ID == id && o.ProductID == product && o.ApplicationID != "" {
			registered := 0
			for _, a := range st.Applications {
				if a.ID == o.ApplicationID && a.ProductID == product {
					registered++
				}
			}
			if registered == 1 {
				app = o.ApplicationID
				count++
			}
		}
	}
	if count != 1 {
		return ""
	}
	return app
}

func restorationEffectIdentity(a, b DevRestorationEffect) bool {
	if a.OperationID != b.OperationID || a.NoIntent != b.NoIntent || !reflect.DeepEqual(a.Connection, b.Connection) || !reflect.DeepEqual(a.Batch, b.Batch) {
		return false
	}
	if a.PullRequest == nil || b.PullRequest == nil {
		return a.PullRequest == nil && b.PullRequest == nil
	}
	return a.PullRequest.Number == b.PullRequest.Number && a.PullRequest.URL == b.PullRequest.URL && a.PullRequest.HeadSHA == b.PullRequest.HeadSHA
}

func restorationPriorAttemptBound(old, current, hold ExternalOperation) bool {
	p, r := old.DevRestoration, current.DevRestoration
	if p == nil || r == nil || old.ReservationQuarantined || old.Kind != "DEV_CANDIDATE_RESTORATION" || old.ProductID != current.ProductID || old.RequestedBy != current.RequestedBy || old.EnvironmentID != current.EnvironmentID || old.ParentID != hold.ID || p.ReservationOperationID != hold.ID || p.ReservationHash != r.ReservationHash || p.BaselineCaptureID != r.BaselineCaptureID || p.BaselineCaptureHash != r.BaselineCaptureHash || p.Generation == 0 || p.Generation >= r.Generation || p.Epoch != hold.ReservationEpoch+p.Generation || p.Epoch != old.ReservationEpoch || p.Selection.Generation != hold.DevReservation.ReservedSelection.Generation+p.Generation || p.Selection.CompositionID != r.Selection.CompositionID || p.Selection.OperationID != r.Selection.OperationID || !sameRestorationOriginalComponents(p.Components, r.Components) || !validRestorationRequestReceipt(old, "request_candidate_dev_restoration", p.Request) || old.ReservationMutationIntent != "" || old.GitOpsPR != nil || old.GitOpsResult != nil || old.DevActivationBatch != nil {
		return false
	}
	copy := *p
	copy.SnapshotHash = ""
	if p.SnapshotHash != checkHash(copy) || p.Request.ReservationOperationID != hold.ID || p.Request.ExpectedSelection.Generation+1 != p.Selection.Generation || p.Request.ExpectedSelection.CompositionID != p.Selection.CompositionID || p.Request.ExpectedSelection.OperationID != p.Selection.OperationID || !p.Deadline.Equal(old.CreatedAt.Add(time.Hour)) || ValidateDevRestorationPlan(old) != nil {
		return false
	}
	bound := func(effect DevRestorationEffect) bool {
		count := 0
		for _, admitted := range r.Effects {
			if restorationEffectIdentity(effect, admitted) {
				count++
			}
		}
		return count == 1
	}
	for _, effect := range p.Effects {
		if !bound(effect) {
			return false
		}
	}
	if old.DevRestorationPlan != nil {
		for _, group := range old.DevRestorationPlan.Groups {
			if !group.SendStarted {
				if group.PullRequest != nil || group.MergedAt != nil || group.Batch != nil && len(group.Result) != 0 {
					return false
				}
				continue // Exact prepared-but-unsent batches carry no mutation intent.
			}
			if group.Batch == nil || !bound(DevRestorationEffect{OperationID: group.LeaderID, Connection: group.Connection, Batch: group.Batch, PullRequest: group.PullRequest}) {
				return false
			}
		}
	}
	return true
}

func (c *retentionCollector) restorationSettledImages() map[string]map[string]bool {
	unique := map[string]*ExternalOperation{}
	duplicate := map[string]bool{}
	for i := range c.st.Operations {
		o := &c.st.Operations[i]
		if _, exists := unique[o.ID]; exists {
			duplicate[o.ID] = true
		}
		unique[o.ID] = o
	}
	settled := map[string]map[string]bool{}
	for _, receipt := range c.st.Operations {
		p := receipt.DevRestorationRelease
		if receipt.ProductID != c.product || p == nil {
			continue
		}
		ids := []string{p.ReservationOperationID, p.Request.RestorationOperationID, p.ConfirmationID, p.Request.RestorationVerificationOperationID, receipt.ID}
		valid := true
		seen := map[string]bool{}
		for _, id := range ids {
			o := unique[id]
			if id == "" || seen[id] || duplicate[id] || o == nil || o.ProductID != c.product {
				valid = false
				break
			}
			seen[id] = true
		}
		if !valid || !ValidRestorationRelease(&c.st, receipt, *unique[p.ReservationOperationID]) {
			continue
		}
		recovery := unique[p.Request.RestorationOperationID]
		images := map[string]bool{}
		add := func(app, repo, digest string) {
			canonical, err := CanonicalImageRepository(repo)
			if app != "" && err == nil && retentionDigest.MatchString(digest) {
				images[app+"\x00"+canonical+"@"+digest] = true
			}
		}
		// Only the immutable admitted proof and validated plan projection bind
		// settled images. Unchecked extra results/availability cannot add to it.
		for _, m := range recovery.DevRestoration.Components {
			add(m.ApplicationID, m.ImageRepository, m.Digest)
			if m.ObservedGitOps != nil {
				add(m.ApplicationID, m.ObservedGitOps.ImageRepository, m.ObservedGitOps.Digest)
			}
		}
		for _, effect := range recovery.DevRestoration.Effects {
			if effect.Batch != nil {
				for _, change := range effect.Batch.Changes {
					add(restorationChangeApplication(&c.st, c.product, change.ID), change.ImageRepository, change.Digest)
				}
			}
		}
		for _, m := range recovery.DevRestorationPlan.Components {
			add(m.ApplicationID, m.Original.ImageRepository, m.Original.Digest)
			add(m.ApplicationID, m.Current.ImageRepository, m.Current.Digest)
		}
		for _, group := range recovery.DevRestorationPlan.Groups {
			if group.Batch != nil {
				for _, change := range group.Batch.Changes {
					add(restorationChangeApplication(&c.st, c.product, change.ID), change.ImageRepository, change.Digest)
				}
			}
		}
		for _, id := range ids[1:] {
			settled[id] = images
		}
		// Explicit renewal fences earlier attempts, including nil plans and
		// prepared-but-unsent groups. Their exact admitted effects must all be
		// present in the validated current recovery, never inferred from ParentID.
		for _, old := range c.st.Operations {
			if duplicate[old.ID] || !restorationPriorAttemptBound(old, *recovery, *unique[p.ReservationOperationID]) {
				continue
			}
			generationUnique := true
			for _, other := range c.st.Operations {
				if other.ID != old.ID && other.DevRestoration != nil && other.DevRestoration.ReservationOperationID == old.ParentID && other.DevRestoration.Generation == old.DevRestoration.Generation {
					generationUnique = false
				}
			}
			if !generationUnique {
				continue
			}
			settled[old.ID] = images
			var confirmation *ExternalOperation
			ambiguous := false
			for i := range c.st.Operations {
				o := &c.st.Operations[i]
				if o.ParentID != old.ID || o.Kind != "DEV_RESTORATION_CONFIRMATION" {
					continue
				}
				if confirmation != nil {
					ambiguous = true
				}
				confirmation = o
			}
			if confirmation == nil || ambiguous || duplicate[confirmation.ID] {
				continue
			}
			cp := confirmation.DevRestorationConfirmation
			if cp == nil || confirmation.ReservationQuarantined || confirmation.ProductID != old.ProductID || confirmation.RequestedBy != old.RequestedBy || confirmation.EnvironmentID != old.EnvironmentID || confirmation.ReservationEpoch != old.ReservationEpoch || confirmation.Status != "SUCCEEDED" || confirmation.Phase != "ATTRIBUTED_CONTROL_CONFIRMATION" || !validRestorationRequestReceipt(*confirmation, "confirm_candidate_dev_restoration", cp.Request) || cp.Request.RestorationOperationID != old.ID || cp.Request.ExpectedSelection == nil || *cp.Request.ExpectedSelection != old.DevRestoration.Selection || cp.TrustGrade != "ATTRIBUTED_CONTROL_CONFIRMATION" || cp.RestorationHash != old.DevRestoration.SnapshotHash || old.DevRestorationPlan == nil || cp.Plan.Hash != old.DevRestorationPlan.Hash || cp.Request.ExpectedPlanHash != cp.Plan.Hash || cp.Plan.Hash != restorationCanonicalPlanHash(cp.Plan) || !reflect.DeepEqual(restorationFrozenPlan(cp.Plan), restorationFrozenPlan(*old.DevRestorationPlan)) || !cp.ConfirmedAt.Equal(confirmation.CreatedAt) || cp.ConfirmedAt.Before(old.CreatedAt) {
				continue
			}
			cc := *cp
			cc.Hash = ""
			if cp.Hash != checkHash(cc) {
				continue
			}
			settled[confirmation.ID] = images
			for _, verification := range c.st.Operations {
				v := verification.DevRestorationVerification
				if v == nil || duplicate[verification.ID] || verification.ReservationQuarantined || verification.Kind != "DEV_RESTORATION_VERIFICATION" || verification.ParentID != old.ID || verification.ProductID != old.ProductID || verification.RequestedBy != old.RequestedBy || verification.EnvironmentID != old.EnvironmentID || verification.ReservationEpoch != old.ReservationEpoch || !validRestorationRequestReceipt(verification, "verify_candidate_dev_restoration", v.Request) || v.Request.RestorationOperationID != old.ID || v.Request.ExpectedSelection == nil || *v.Request.ExpectedSelection != old.DevRestoration.Selection || v.RestorationHash != old.DevRestoration.SnapshotHash || v.PlanHash != old.DevRestorationPlan.Hash || v.ConfirmationID != confirmation.ID || v.ConfirmationHash != cp.Hash || v.Generation == 0 || !sameRestorationOriginalComponents(v.Components, old.DevRestoration.Components) {
					continue
				}
				vp := *v
				vp.Hash = ""
				expected, err := RestorationRuntimeExpected(old, v.Request, v.Generation)
				if err == nil && v.Hash == checkHash(vp) && reflect.DeepEqual(expected, v.Runtime) {
					settled[verification.ID] = images
				}
			}
		}
	}
	return settled
}

func (c *retentionCollector) restorationRoots() {
	settled := c.restorationSettledImages()
	for _, o := range c.st.Operations {
		if o.ProductID != c.product || !IsRestorationOperation(o) {
			continue
		}
		c.evidence = append(c.evidence, checkHash(o))
		reason := "dev_restoration:" + o.ID
		if len(o.ID) > 128 || strings.ContainsAny(o.ID, "\r\n\t") {
			reason = "dev_restoration:" + checkHash(o.ID)
		}
		add := func(app, repo, digest string) {
			if digest == "" {
				return
			}
			canonical, err := CanonicalImageRepository(repo)
			if app != "" && err == nil && settled[o.ID][app+"\x00"+canonical+"@"+digest] {
				return
			}
			image := c.image(repo, digest, app, reason)
			if image != nil && settled[o.ID] != nil {
				image.Blockers = append(image.Blockers, "restoration_unvalidated_nested_reference")
				c.block("restoration_unvalidated_nested_reference")
			}
		}
		artifact := func(id, app string) {
			if id == "" {
				return
			}
			for _, a := range c.st.DeliveryArtifacts {
				if a.ID == id && a.ProductID == c.product {
					add(app, c.artifactRepo(a), a.Digest)
				}
			}
		}
		ref := func(value, hint, app string) {
			value = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(value, "docker-pullable://"), "docker://"), "containerd://"), "cri-o://")
			if parts := strings.SplitN(value, "@", 2); len(parts) == 2 {
				repo := parts[0]
				if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
					repo = repo[:i]
				}
				add(app, repo, parts[1])
			} else if retentionDigest.MatchString(value) && hint != "" {
				add(app, hint, value)
			} else if value != "" {
				c.block("unresolved_image_reference")
			}
		}
		components := func(parts []DevBaselineComponent) {
			for _, m := range parts {
				add(m.ApplicationID, m.ImageRepository, m.Digest)
				artifact(m.ArtifactID, m.ApplicationID)
				if m.ObservedGitOps != nil {
					add(m.ApplicationID, m.ObservedGitOps.ImageRepository, m.ObservedGitOps.Digest)
				}
			}
		}
		batch := func(b *delivery.GitOpsBatchApply) {
			if b != nil {
				for _, change := range b.Changes {
					add(restorationChangeApplication(&c.st, c.product, change.ID), change.ImageRepository, change.Digest)
				}
			}
		}
		plan := func(p *DevRestorationPlan) {
			if p == nil {
				return
			}
			for _, m := range p.Components {
				add(m.ApplicationID, m.Original.ImageRepository, m.Original.Digest)
				add(m.ApplicationID, m.Current.ImageRepository, m.Current.Digest)
			}
			for _, g := range p.Groups {
				batch(g.Batch)
				members := []string{}
				seen := map[string]bool{}
				mapping := g.ID != ""
				groups := 0
				for _, other := range p.Groups {
					if other.ID == g.ID {
						groups++
					}
				}
				for _, m := range p.Components {
					if m.GroupID != g.ID {
						continue
					}
					if m.ApplicationID == "" || seen[m.ApplicationID] {
						mapping = false
					}
					seen[m.ApplicationID] = true
					members = append(members, m.ApplicationID)
				}
				mapping = mapping && groups == 1 && len(members) == len(g.Result)
				for i, result := range g.Result {
					app := ""
					if mapping {
						app = members[i]
					}
					add(app, result.ImageRepository, result.Digest)
				}
			}
		}
		availability := func(parts []DevBaselineComponentResult) {
			for _, m := range parts {
				add(m.ApplicationID, m.ImageRepository, m.Digest)
				add(m.ApplicationID, m.GitOps.ImageRepository, m.GitOps.Digest)
				artifact(m.ArtifactID, m.ApplicationID)
				if m.Availability != nil {
					add(m.ApplicationID, m.ImageRepository, m.Availability.Digest)
					if strings.Contains(m.Availability.URI, "@") {
						ref(m.Availability.URI, m.ImageRepository, m.ApplicationID)
					}
				}
			}
		}
		if p := o.DevRestoration; p != nil {
			components(p.Components)
			for _, effect := range p.Effects {
				batch(effect.Batch)
			}
		}
		plan(o.DevRestorationPlan)
		if p := o.DevRestorationConfirmation; p != nil {
			plan(&p.Plan)
		}
		if p := o.DevRestorationVerification; p != nil {
			components(p.Components)
			for _, m := range p.Runtime.Components {
				add(m.ApplicationID, m.ImageRepository, m.Digest)
				artifact(m.ArtifactID, m.ApplicationID)
			}
			availability(p.Availability)
		}
		if p := o.DevRestorationRelease; p != nil {
			availability(p.Availability)
		}
		availability(o.DevRestorationAvailability)
		if p := o.DevRuntimeResult; p != nil {
			for _, m := range p.Components {
				add(m.ApplicationID, m.GitOps.ImageRepository, m.GitOps.Digest)
				hint := m.GitOps.ImageRepository
				if o.DevRestorationVerification != nil {
					for _, expected := range o.DevRestorationVerification.Runtime.Components {
						if expected.ApplicationID == m.ApplicationID {
							hint = expected.ImageRepository
						}
					}
				}
				for _, role := range m.Roles {
					ref(role.WorkloadImage, hint, m.ApplicationID)
					for _, pod := range role.Pods {
						ref(pod.Image, hint, m.ApplicationID)
						ref(pod.ImageID, hint, m.ApplicationID)
					}
				}
			}
		}
	}
}

func restorationFrozenPlan(p DevRestorationPlan) DevRestorationPlan {
	p.Hash = ""
	p.Groups = append([]DevRestorationGroup(nil), p.Groups...)
	for i := range p.Groups {
		p.Groups[i].Result = nil
		p.Groups[i].MergedAt = nil
	}
	return p
}
func restorationCanonicalPlanHash(p DevRestorationPlan) string {
	return checkHash(restorationFrozenPlan(p))
}
func RestorationRuntimeExpected(parent ExternalOperation, in DevRestorationRequest, generation uint64) (DevRuntimeProof, error) {
	if parent.DevRestoration == nil {
		return DevRuntimeProof{}, fmt.Errorf("immutable restoration proof required")
	}
	p := DevRuntimeProof{Request: DevRuntimeRequest{ActivationOperationID: parent.ID, RequestKey: in.RequestKey, ExpectedSelection: in.ExpectedSelection}, Generation: generation, ActivationHash: checkHash(parent.DevRestorationPlan)}
	if parent.DevRestorationPlan == nil || parent.DevRestorationPlan.Hash == "" || parent.DevRestorationPlan.Hash != restorationCanonicalPlanHash(*parent.DevRestorationPlan) || parent.Status != "SUCCEEDED" || parent.Phase != "GITOPS_APPLIED" {
		return p, fmt.Errorf("complete restored desired state required")
	}
	for _, c := range parent.DevRestoration.Components {
		commit := ""
		for _, g := range parent.DevRestorationPlan.Groups {
			n := 0
			for _, m := range parent.DevRestorationPlan.Components {
				if m.GroupID != g.ID {
					continue
				}
				if m.ApplicationID == c.ApplicationID {
					if n >= len(g.Result) {
						return p, fmt.Errorf("complete recovery group result missing")
					}
					r := g.Result[n]
					if r.ImageRepository != c.ImageRepository || r.Digest != c.Digest || !runtimeCommit.MatchString(r.HeadSHA) || !runtimeCommit.MatchString(r.BlobSHA) {
						return p, fmt.Errorf("original desired result changed")
					}
					commit = r.HeadSHA
				}
				n++
			}
		}
		if commit == "" {
			return p, fmt.Errorf("original component recovery result missing")
		}
		p.Components = append(p.Components, DevRuntimeComponent{AuthorityHash: c.AuthorityHash, ApplicationID: c.ApplicationID, ArtifactID: c.ArtifactID, ImageRepository: c.ImageRepository, Digest: c.Digest, GitOps: c.GitOps, GitOpsCommit: commit, ConfigHash: c.ConfigHash, Scopes: c.Scopes})
	}
	p.SnapshotHash = runtimeProofHash(p)
	return p, nil
}

func validRestorationRequestReceipt(o ExternalOperation, action string, in DevRestorationRequest) bool {
	if o.ID == "" || o.ProductID == "" || strings.TrimSpace(o.RequestedBy) == "" || o.Actor != o.RequestedBy || o.FeatureID != "" || o.IntegrationID != "" || o.ApplicationID != "" || o.RepositoryID != "" || o.BuildOnly || o.Native != nil || o.MainCandidate != nil || o.Snapshot != nil || o.Run != nil || o.GitOpsPR != nil || o.GitOpsResult != nil || len(o.Sources) != 0 || len(o.PreparedDeployments) != 0 || o.CreatedAt.IsZero() || o.RequestKey != in.RequestKey || len(in.RequestKey) < 16 || len(in.RequestKey) > 128 || strings.TrimSpace(in.RequestKey) != in.RequestKey || strings.ContainsFunc(in.RequestKey, func(r rune) bool { return r < 32 || r == 127 }) || in.ExpectedSelection == nil {
		return false
	}
	switch action {
	case "request_candidate_dev_restoration":
		if in.ReservationOperationID == "" || in.RestorationOperationID != "" || in.RestorationVerificationOperationID != "" || in.ExpectedPlanHash != "" {
			return false
		}
	case "confirm_candidate_dev_restoration":
		if in.ReservationOperationID != "" || in.RestorationOperationID == "" || in.RestorationVerificationOperationID != "" || !retentionDigest.MatchString(in.ExpectedPlanHash) {
			return false
		}
	case "verify_candidate_dev_restoration":
		if in.ReservationOperationID != "" || in.RestorationOperationID == "" || in.RestorationVerificationOperationID != "" || in.ExpectedPlanHash != "" {
			return false
		}
	case "release_candidate_dev_after_restoration":
		if in.ReservationOperationID != "" || in.RestorationOperationID == "" || in.RestorationVerificationOperationID == "" || in.ExpectedPlanHash != "" {
			return false
		}
	default:
		return false
	}
	return o.RequestHash == checkHash(struct {
		Action, Product, Actor string
		Input                  DevRestorationRequest
	}{action, o.ProductID, o.RequestedBy, in})
}
func sameRestorationOriginalComponents(a, b []DevBaselineComponent) bool {
	if len(a) == 0 || len(a) != len(b) || len(a) > 16 {
		return false
	}
	for i, c := range a {
		d := b[i]
		c.RegistryJobID = ""
		c.RegistryInputHash = ""
		c.RegistryRootSetHash = ""
		d.RegistryJobID = ""
		d.RegistryInputHash = ""
		d.RegistryRootSetHash = ""
		if !reflect.DeepEqual(c, d) {
			return false
		}
	}
	return true
}
func validRestorationSuccessorEvidence(st *State, receipt, hold, recovery, confirmation, verification ExternalOperation) bool {
	r, c, v, p := recovery.DevRestoration, confirmation.DevRestorationConfirmation, verification.DevRestorationVerification, receipt.DevRestorationRelease
	if r == nil || c == nil || v == nil || p == nil || hold.DevReservation == nil || recovery.DevRestorationUnapproved || !validRestorationRequestReceipt(recovery, "request_candidate_dev_restoration", r.Request) || !validRestorationRequestReceipt(confirmation, "confirm_candidate_dev_restoration", c.Request) || !validRestorationRequestReceipt(verification, "verify_candidate_dev_restoration", v.Request) || !validRestorationRequestReceipt(receipt, "release_candidate_dev_after_restoration", p.Request) {
		return false
	}
	if recovery.DevRestorationConfirmation != nil || recovery.DevRestorationVerification != nil || recovery.DevRestorationRelease != nil || confirmation.DevRestoration != nil || confirmation.DevRestorationPlan != nil || confirmation.DevRestorationVerification != nil || confirmation.DevRestorationRelease != nil || verification.DevRestoration != nil || verification.DevRestorationPlan != nil || verification.DevRestorationConfirmation != nil || verification.DevRestorationRelease != nil || receipt.DevRestoration != nil || receipt.DevRestorationPlan != nil || receipt.DevRestorationConfirmation != nil || receipt.DevRestorationVerification != nil || len(confirmation.ChildIDs) != 0 || len(receipt.ChildIDs) != 0 {
		return false
	}
	if hold.Kind != "DEV_RESERVATION" || hold.Status != "SUCCEEDED" || hold.Phase != "RESERVED" || hold.RequestedBy != recovery.RequestedBy || hold.DevReservation.Epoch != hold.ReservationEpoch || hold.DevReservation.CandidateHash != checkHash(hold.DevReservation.CandidateProof) || r.Request.ReservationOperationID != hold.ID || r.Generation == 0 || r.Generation > 256 || r.Epoch != hold.ReservationEpoch+r.Generation || r.Epoch <= hold.ReservationEpoch || r.Selection.Generation != hold.DevReservation.ReservedSelection.Generation+r.Generation || r.Selection.Generation <= hold.DevReservation.ReservedSelection.Generation || r.Selection.CompositionID != hold.DevReservation.ReservedSelection.CompositionID || r.Selection.OperationID != hold.DevReservation.ReservedSelection.OperationID || r.Request.ExpectedSelection.Generation+1 != r.Selection.Generation || r.Request.ExpectedSelection.CompositionID != r.Selection.CompositionID || r.Request.ExpectedSelection.OperationID != r.Selection.OperationID || !r.Deadline.Equal(recovery.CreatedAt.Add(time.Hour)) {
		return false
	}
	for _, in := range []DevRestorationRequest{c.Request, v.Request, p.Request} {
		if in.RestorationOperationID != recovery.ID || in.ExpectedSelection == nil || *in.ExpectedSelection != r.Selection {
			return false
		}
	}
	if c.Request.ExpectedPlanHash != recovery.DevRestorationPlan.Hash || c.Plan.Hash != restorationCanonicalPlanHash(c.Plan) || !reflect.DeepEqual(restorationFrozenPlan(c.Plan), restorationFrozenPlan(*recovery.DevRestorationPlan)) || c.ConfirmedAt.Before(recovery.CreatedAt) || !c.ConfirmedAt.Equal(confirmation.CreatedAt) || confirmation.FinishedAt == nil || !confirmation.FinishedAt.Equal(c.ConfirmedAt) || verification.CreatedAt.Before(c.ConfirmedAt) || verification.FinishedAt == nil || verification.DevRuntimeResult == nil || verification.DevRuntimeResult.ObservedAt.Before(verification.CreatedAt) || verification.DevRuntimeResult.ObservedAt.After(*verification.FinishedAt) || receipt.CreatedAt.Before(*verification.FinishedAt) || receipt.FinishedAt == nil || receipt.FinishedAt.Before(receipt.CreatedAt) {
		return false
	}
	if ValidateDevRestorationPlan(recovery) != nil {
		return false
	}
	if recovery.FinishedAt == nil || recovery.FinishedAt.Before(c.ConfirmedAt) || verification.CreatedAt.Before(*recovery.FinishedAt) || p.Request.RestorationVerificationOperationID != verification.ID || recovery.DevRestorationPlan.ExpectedHash != r.SnapshotHash || !sameRestorationOriginalComponents(r.Components, v.Components) {
		return false
	}
	var capture *ExternalOperation
	for i := range st.Operations {
		if st.Operations[i].ID == r.BaselineCaptureID {
			if capture != nil {
				return false
			}
			capture = &st.Operations[i]
		}
	}
	if capture == nil || capture.DevBaseline == nil || capture.DevBaselineResult == nil || capture.Kind != "DEV_CANDIDATE_BASELINE_CAPTURE" || capture.ReservationQuarantined || capture.Status != "SUCCEEDED" || capture.Phase != "BASELINE_CAPTURED" || capture.ProductID != hold.ProductID || capture.RequestedBy != hold.RequestedBy || capture.EnvironmentID != hold.EnvironmentID || capture.ReservationEpoch != hold.ReservationEpoch || capture.ParentID != hold.ID || r.BaselineCaptureHash != checkHash(struct {
		Proof  *DevBaselineProof
		Result *DevBaselineResult
	}{capture.DevBaseline, capture.DevBaselineResult}) || !sameRestorationOriginalComponents(r.Components, capture.DevBaseline.Components) {
		return false
	}
	baseline := *capture.DevBaseline
	baseline.SnapshotHash = ""
	baselineResult := *capture.DevBaselineResult
	baselineResult.Hash = ""
	if capture.DevBaseline.SnapshotHash != checkHash(baseline) || capture.DevBaselineResult.Hash != checkHash(baselineResult) || capture.DevBaselineResult.ExpectedHash != capture.DevBaseline.SnapshotHash || capture.DevBaseline.ReservationHash != checkHash(hold.DevReservation) || capture.DevBaseline.ReservationOperationID != hold.ID || capture.DevBaselineResult.Outcome != "CAPTURED" {
		return false
	}
	expected, e := RestorationRuntimeExpected(recovery, v.Request, v.Generation)
	if e != nil || !reflect.DeepEqual(expected, v.Runtime) || v.Runtime.ActivationHash != checkHash(recovery.DevRestorationPlan) || v.Runtime.SnapshotHash != runtimeProofHash(v.Runtime) || ValidateDevRuntimeResultAt(v.Runtime, *verification.DevRuntimeResult, *receipt.FinishedAt) != nil {
		return false
	}
	roles, pods := 0, 0
	seen := map[string]bool{}
	for i, component := range v.Runtime.Components {
		if component.ApplicationID == "" || seen[component.ApplicationID] || len(component.Scopes) == 0 {
			return false
		}
		seen[component.ApplicationID] = true
		roles += len(component.Scopes)
		for j, scope := range component.Scopes {
			role := verification.DevRuntimeResult.Components[i].Roles[j]
			if scope.ProductID != hold.ProductID || scope.EnvironmentID != hold.EnvironmentID || scope.ApplicationID != component.ApplicationID || role.ObservedAt.Before(verification.CreatedAt) || role.ObservedAt.After(verification.DevRuntimeResult.ObservedAt) {
				return false
			}
			pods += len(role.Pods)
		}
	}
	if roles > 64 || pods > 512 {
		return false
	}
	if !validRestorationAvailabilityEvidence(st, verification, v.Components, v.Availability, verification.DevRuntimeResult.ObservedAt) {
		return false
	}
	if p.AvailabilityHash == "" || p.AvailabilityHash != checkHash(p.Availability) || !validRestorationAvailabilityEvidence(st, verification, v.Components, p.Availability, *receipt.FinishedAt) {
		return false
	}

	for _, o := range []ExternalOperation{recovery, confirmation, verification, receipt} {
		raw, e := json.Marshal(o)
		if e != nil || len(raw)+4096 > 256*1024 {
			return false
		}
	}
	return true
}
func validRestorationAvailabilityEvidence(st *State, op ExternalOperation, components []DevBaselineComponent, results []DevBaselineComponentResult, at time.Time) bool {
	if len(components) == 0 || len(components) != len(results) {
		return false
	}
	children := []string{}
	for i, component := range components {
		result := results[i]
		if component.ObservedGitOps == nil || result.ApplicationID != component.ApplicationID || result.ImageRepository != component.ImageRepository || result.Digest != component.Digest || result.ArtifactID != component.ArtifactID || result.ArtifactHash != component.ArtifactHash || result.GitOps != *component.ObservedGitOps {
			return false
		}
		if component.ArtifactID != "" {
			a := result.Availability
			if a == nil || result.Registry != nil || !a.Available || a.Digest != component.Digest || a.ObservedAt.Before(op.CreatedAt) || a.ObservedAt.After(at) || at.Sub(a.ObservedAt) > 5*time.Minute {
				return false
			}
			found := 0
			for _, artifact := range st.DeliveryArtifacts {
				if artifact.ID == component.ArtifactID {
					found++
					if artifact.ProductID != op.ProductID || artifact.ApplicationID != component.ApplicationID || artifact.RepositoryID != component.RepositoryID || artifact.ImageRepository != component.ImageRepository || artifact.Digest != component.Digest || checkHash(artifact) != component.ArtifactHash {
						return false
					}
				}
			}
			if found != 1 {
				return false
			}
		} else {
			if result.Availability != nil || result.Registry == nil {
				return false
			}
			receipt := result.Registry
			if receipt.ClosureKind != "IMAGE_CONTENT" || receipt.JobID != component.RegistryJobID || receipt.InputHash != component.RegistryInputHash || receipt.RootSetHash != component.RegistryRootSetHash || receipt.ObservedAt.Before(op.CreatedAt) || receipt.ObservedAt.After(at) || at.Sub(receipt.ObservedAt) > 5*time.Minute {
				return false
			}
			var job *WorkerJob
			for j := range st.WorkerJobs {
				if st.WorkerJobs[j].ID == receipt.JobID {
					if job != nil {
						return false
					}
					job = &st.WorkerJobs[j]
				}
			}
			if job == nil || job.Status != "SUCCEEDED" || job.CancellationRequested || job.ProductID != op.ProductID || job.Capability != "registry_observe" || job.CreatedAt.Before(op.CreatedAt) || job.Inputs.Registry == nil || job.InputHash != receipt.InputHash || job.InputHash != checkHash(job.Inputs) || job.Inputs.Registry.RootSetHash != receipt.RootSetHash || job.Inputs.Registry.Scope.ProductID != op.ProductID || job.Inputs.Registry.Scope.ComponentID != component.ApplicationID || job.Inputs.Registry.Scope.RepositoryID != component.RepositoryID || job.Inputs.Registry.Scope.ImageRepository != component.ImageRepository || !slices.Contains(job.Inputs.Registry.Roots, component.Digest) || job.OperationID != receipt.OperationID {
				return false
			}
			children = append(children, job.OperationID)
			var child *ExternalOperation
			for j := range st.Operations {
				if st.Operations[j].ID == job.OperationID {
					if child != nil {
						return false
					}
					child = &st.Operations[j]
				}
			}
			if child == nil || child.Kind != RegistryObserveOperationKind || child.ParentID != op.ID || child.ProductID != op.ProductID || child.RequestedBy != op.RequestedBy || child.ApplicationID != component.ApplicationID || child.RepositoryID != component.RepositoryID || child.RegistryObservation == nil || !reflect.DeepEqual(*child.RegistryObservation, job.Inputs) || child.Status != "SUCCEEDED" || child.Phase != "EXTERNAL_SUCCEEDED" {
				return false
			}
			var inventory *RegistryInventory
			for j := range st.RegistryInventories {
				if st.RegistryInventories[j].ID == receipt.InventoryID {
					if inventory != nil {
						return false
					}
					inventory = &st.RegistryInventories[j]
				}
			}
			if inventory == nil || inventory.JobID != job.ID || inventory.AttemptID != receipt.AttemptID || inventory.Fence != job.Fence || inventory.Fence != receipt.Fence || inventory.InputHash != job.InputHash || !reflect.DeepEqual(inventory.Inputs, job.Inputs) || RegistryInventoryFreshness(*inventory, at) != "FRESH" || inventory.Report.Graph.GraphHash != receipt.GraphHash || !inventory.Report.FinishedAt.Equal(receipt.ObservedAt) {
				return false
			}
			var attempt *WorkerAttempt
			for j := range st.WorkerAttempts {
				if st.WorkerAttempts[j].ID == receipt.AttemptID {
					if attempt != nil {
						return false
					}
					attempt = &st.WorkerAttempts[j]
				}
			}
			if attempt == nil || attempt.Status != "SUCCEEDED" || attempt.Fence != receipt.Fence || attempt.Report == nil || attempt.Report.Registry == nil || !reflect.DeepEqual(*attempt.Report.Registry, inventory.Report) || ValidateRegistryWorkerReport(*job, *attempt, *attempt.Report, inventory.ReceivedAt) != nil {
				return false
			}
			contentHash, e := RegistryImageContentProofHash(inventory.Report.Graph, job.Inputs.Registry.Limits, job.Inputs.Registry.Roots, component.Digest)
			if e != nil || contentHash != receipt.ContentProofHash {
				return false
			}
			closure := "UNKNOWN"
			blockers := append([]string(nil), inventory.Report.Graph.Blockers...)
			for _, root := range inventory.Report.Graph.Roots {
				if root.Digest == component.Digest {
					closure = root.Closure
					blockers = append(blockers, root.Blockers...)
				}
			}
			if receipt.MetadataRootClosure != closure || receipt.MetadataDiscovery != inventory.Report.Graph.Discovery || receipt.GlobalInventoryComplete != inventory.Report.Graph.GlobalInventoryComplete || !reflect.DeepEqual(sortedUnique(blockers), receipt.MetadataBlockers) {
				return false
			}
		}
	}
	return slices.Equal(children, op.ChildIDs)
}

func GitOpsAuthorityFingerprint(c delivery.Connection) (string, error) {
	raw := c.APIURL
	if raw == "" {
		raw = "https://api.github.com"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawPath != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("GitOps API authority unavailable")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		host := u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		u.Host = host
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return checkHash(u.String()), nil
}
func restorationPhysicalKey(c delivery.Connection, repository, ref string) string {
	authority, _ := GitOpsAuthorityFingerprint(c)
	return authority + "|" + repository + "|" + ref
}
func ValidateDevRestorationPlan(op ExternalOperation) error {
	p := op.DevRestorationPlan
	if p == nil {
		return nil
	}
	proof := op.DevRestoration
	if proof == nil || p.ExpectedHash != proof.SnapshotHash || p.SettlementHash == "" || len(p.Components) != len(proof.Components) || len(p.Groups) == 0 || len(p.Groups) > 16 {
		return fmt.Errorf("recovery plan membership changed")
	}
	seen := map[string]bool{}
	groups := map[string]DevRestorationGroup{}
	for _, g := range p.Groups {
		if _, e := GitOpsAuthorityFingerprint(g.Connection); e != nil {
			return e
		}
		key := restorationPhysicalKey(g.Connection, g.Repository, g.Ref)
		if g.ID != checkHash(key) || g.LeaderID != op.ID+"-"+checkHash(key)[7:23] || seen[g.ID] {
			return fmt.Errorf("recovery group authority changed")
		}
		seen[g.ID] = true
		groups[g.ID] = g
	}
	changes := map[string][]delivery.GitOpsImageChange{}
	heads := map[string]string{}
	for i, m := range p.Components {
		c := proof.Components[i]
		g, ok := groups[m.GroupID]
		selector := c.GitOps
		selector.ImageRepository = m.Current.ImageRepository
		selector.ExpectedHeadSHA = ""
		noWrite := m.Current.ImageRepository == m.Original.ImageRepository && m.Current.Digest == m.Original.Digest
		authority, _ := GitOpsAuthorityFingerprint(g.Connection)
		if authority != c.AuthorityHash {
			return fmt.Errorf("recovery group differs from original physical authority")
		}
		if !ok || m.ApplicationID != c.ApplicationID || c.ObservedGitOps == nil || m.Original != *c.ObservedGitOps || !reflect.DeepEqual(m.Selector, selector) || g.Repository != selector.Repository || g.Ref != selector.Ref || m.NoWrite != noWrite || !runtimeCommit.MatchString(m.Current.HeadSHA) || !runtimeCommit.MatchString(m.Current.BlobSHA) || !retentionDigest.MatchString(m.Current.ContentHash) || !retentionDigest.MatchString(m.Current.Digest) {
			return fmt.Errorf("recovery original/current member changed")
		}
		if head := heads[g.ID]; head != "" && head != m.Current.HeadSHA {
			return fmt.Errorf("recovery group prepared at different heads")
		}
		heads[g.ID] = m.Current.HeadSHA
		if !noWrite {
			changes[g.ID] = append(changes[g.ID], delivery.GitOpsImageChange{ID: c.ApplicationID, Selector: selector, ExpectedBlobSHA: m.Current.BlobSHA, ImageRepository: c.ImageRepository, Digest: c.Digest})
		}
	}
	for _, g := range p.Groups {
		list := changes[g.ID]
		if len(list) == 0 {
			if g.Batch != nil || g.SendStarted || g.PullRequest != nil || g.IntentHash != "" || g.MergedAt != nil {
				return fmt.Errorf("no-write recovery group gained mutation authority")
			}
			continue
		}
		expected := delivery.GitOpsBatchApply{Repository: g.Repository, Ref: g.Ref, ExpectedHeadSHA: heads[g.ID], OperationID: g.LeaderID, Message: "release-control: " + g.LeaderID, Changes: list}
		if g.Batch == nil || !reflect.DeepEqual(*g.Batch, expected) || g.IntentHash != checkHash(expected) {
			return fmt.Errorf("complete recovery intent changed")
		}
		raw, _ := json.Marshal(g.Batch)
		if len(raw) > 64*1024 {
			return fmt.Errorf("recovery batch bound exceeded")
		}
		if g.PullRequest != nil && (!g.SendStarted || g.PullRequest.Number <= 0 || g.PullRequest.URL == "" || !runtimeCommit.MatchString(g.PullRequest.HeadSHA)) {
			return fmt.Errorf("recovery proposal identity changed")
		}
	}
	if p.Hash != "" && p.Hash != restorationCanonicalPlanHash(*p) {
		return fmt.Errorf("final recovery plan hash changed")
	}
	return nil
}
