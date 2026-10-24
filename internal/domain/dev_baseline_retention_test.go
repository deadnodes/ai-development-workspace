package domain

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func baselineRetentionFixture() State {
	st := retentionBase()
	st.Applications = append(st.Applications, Application{Meta: Meta{ID: "b", ProductID: "p"}, RepositoryID: "repo-b"})
	comp := Composition{Meta: Meta{ID: "baseline-composition", ProductID: "p"}, EnvironmentID: "dev", Components: []CompositionComponent{{ApplicationID: "app"}, {ApplicationID: "b"}}}
	st.Compositions = []Composition{comp}
	selection := EnvironmentSelection{CompositionID: comp.ID, OperationID: "original-dev", Generation: 7}
	reservation := DevReservationProof{Epoch: 3, ReservedSelection: selection, BaselineSelection: selection, Environment: Environment{Meta: Meta{ID: "dev", ProductID: "p"}}}
	hold := ExternalOperation{Meta: Meta{ID: "hold", ProductID: "p"}, Kind: "DEV_RESERVATION", EnvironmentID: "dev", RequestedBy: "operator", ReservationEpoch: 3, Status: "SUCCEEDED", Phase: "RESERVED", DevReservation: &reservation}
	p := DevBaselineProof{Request: DevBaselineRequest{ReservationOperationID: hold.ID, RequestKey: "capture-baseline-0001", ExpectedSelection: &selection}, ReservationOperationID: hold.ID, ReservationHash: checkHash(reservation), CompositionID: comp.ID, CompositionHash: checkHash(comp), Generation: 1, Components: []DevBaselineComponent{{ApplicationID: "app", RepositoryID: "repo", ImageRepository: "registry.example/team/app", Digest: retentionSHA(300)}, {ApplicationID: "b", RepositoryID: "repo-b", ImageRepository: "registry.example/team/b", Digest: retentionSHA(400)}}}
	p.SnapshotHash = checkHash(p)
	capture := ExternalOperation{Meta: Meta{ID: "capture", ProductID: "p"}, ParentID: hold.ID, Kind: "DEV_CANDIDATE_BASELINE_CAPTURE", EnvironmentID: "dev", RequestedBy: "operator", ReservationEpoch: 3, Status: "RUNNING", Phase: "WAIT_REGISTRY", DevBaseline: &p}
	st.Operations = []ExternalOperation{hold, capture}
	return st
}

func copyBaselineRetentionState(t *testing.T, st State) State {
	t.Helper()
	b, e := json.Marshal(st)
	if e != nil {
		t.Fatal(e)
	}
	var out State
	if e = json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func rehashBaselineRetention(p *DevBaselineProof) { p.SnapshotHash = ""; p.SnapshotHash = checkHash(p) }
func baselineRetained(t *testing.T, st State, at time.Time) RetentionReport {
	t.Helper()
	r := EvaluateRetention(st, "p", at, nil)
	for _, digest := range []string{retentionSHA(300), retentionSHA(400)} {
		v := retentionFind(t, r, digest)
		if !v.Protected || !retentionHas(v.Reasons, "dev_baseline:capture") {
			t.Fatal("baseline root unprotected", v)
		}
	}
	if r.DeletionAllowed || r.GlobalInventoryComplete {
		t.Fatal("baseline supplied deletion authority")
	}
	return r
}

func TestDevBaselineRetentionStagedFailedSupersededAndUnknown(t *testing.T) {
	for _, mode := range []string{"staged", "failed", "cancelled", "expired", "superseded", "quarantined", "native-quarantine", "missing-hold", "hold-proof-missing", "hold-damaged", "missing-proof", "proof-hash", "scope", "composition", "oversized-components", "uncertain-effect"} {
		t.Run(mode, func(t *testing.T) {
			st := baselineRetentionFixture()
			at := retentionNow
			switch mode {
			case "failed":
				st.Operations[1].Status = "FAILED"
			case "cancelled":
				st.Operations[1].Status = "CANCELLED"
			case "expired":
				at = at.Add(48 * time.Hour)
			case "superseded":
				newer := copyBaselineRetentionState(t, st).Operations[1]
				newer.ID = "new-capture"
				newer.Status = "FAILED"
				newer.DevBaseline.Generation = 2
				newer.DevBaseline.Components[0].Digest = retentionSHA(500)
				rehashBaselineRetention(newer.DevBaseline)
				st.Operations = append(st.Operations, newer)
			case "quarantined":
				st.Operations[0].ReservationQuarantined = true
			case "native-quarantine":
				st.NativeQuarantines = []NativeQuarantine{{Meta: Meta{ProductID: "p"}}}
			case "missing-hold":
				st.Operations = st.Operations[1:]
			case "hold-proof-missing":
				st.Operations[0].DevReservation = nil
			case "hold-damaged":
				st.Operations[0].DevReservation.Epoch++
			case "missing-proof":
				p := st.Operations[1].DevBaseline
				st.Operations[1].DevBaselineResult = &DevBaselineResult{Components: []DevBaselineComponentResult{{ApplicationID: "app", ImageRepository: p.Components[0].ImageRepository, Digest: p.Components[0].Digest}, {ApplicationID: "b", ImageRepository: p.Components[1].ImageRepository, Digest: p.Components[1].Digest}}}
				st.Operations[1].DevBaseline = nil
			case "proof-hash":
				st.Operations[1].DevBaseline.SnapshotHash = "damaged"
			case "scope":
				st.Operations[1].DevBaseline.Components[0].RepositoryID = "foreign-source"
				rehashBaselineRetention(st.Operations[1].DevBaseline)
			case "composition":
				st.Compositions[0].Components = st.Compositions[0].Components[:1]
			case "oversized-components":
				p := st.Operations[1].DevBaseline
				for len(p.Components) < 17 {
					p.Components = append(p.Components, p.Components[0])
				}
				rehashBaselineRetention(p)
			case "uncertain-effect":
				st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ID: "uncertain", ProductID: "p"}, EnvironmentID: "dev", Phase: "APPLY_UNCERTAIN"})
			}
			baselineRetained(t, st, at)
		})
	}
}

func appendBaselineRelease(st *State) {
	hold := st.Operations[0]
	p := *hold.DevReservation
	p.ReservationOperationID = hold.ID
	p.Request = DevReservationRequest{ReservationOperationID: hold.ID, RequestKey: "release-baseline-0001", ExpectedSelection: &hold.DevReservation.ReservedSelection}
	receipt := ExternalOperation{Meta: Meta{ID: "release", ProductID: hold.ProductID}, Kind: "DEV_RESERVATION_RELEASE", EnvironmentID: hold.EnvironmentID, RequestedBy: hold.RequestedBy, ReservationEpoch: hold.ReservationEpoch, Status: "SUCCEEDED", Phase: "RELEASED", RequestKey: p.Request.RequestKey, DevReservation: &p}
	receipt.RequestHash = checkHash(struct {
		Action, Product, Actor string
		Input                  DevReservationRequest
	}{"release_candidate_dev", receipt.ProductID, receipt.RequestedBy, p.Request})
	st.Operations = append(st.Operations, receipt)
}

func TestDevBaselineRetentionOnlyExactPreactivationReleaseSettles(t *testing.T) {
	for _, mode := range []string{"valid-release", "damaged-release", "activation", "unknown-activation", "quarantined-release", "manual-release", "invalid-proof-with-release"} {
		t.Run(mode, func(t *testing.T) {
			st := baselineRetentionFixture()
			appendBaselineRelease(&st)
			switch mode {
			case "damaged-release":
				st.Operations[2].RequestHash = "wrong"
			case "activation":
				st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ID: "activation", ProductID: "p"}, DevActivation: &DevActivationProof{ReservationOperationID: "hold"}})
			case "unknown-activation":
				st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ID: "activation", ProductID: "p"}, Kind: "DEV_CANDIDATE_DEPLOY", EnvironmentID: "dev", ReservationEpoch: 3, Status: "CANCELLED"})
			case "quarantined-release":
				st.Operations[2].ReservationQuarantined = true
			case "manual-release":
				st.Operations = st.Operations[:2]
				now := retentionNow
				st.ArtifactPins = []ArtifactPin{{Meta: Meta{ID: "caller-pin", ProductID: "p"}, ApplicationID: "app", ImageRepository: "registry.example/team/app", Digest: retentionSHA(300), ReleasedAt: &now}}
			case "invalid-proof-with-release":
				st.Operations[1].DevBaseline.SnapshotHash = "wrong"
			}
			if mode == "valid-release" {
				r := EvaluateRetention(st, "p", retentionNow, nil)
				for _, image := range r.Images {
					if retentionHas(image.Reasons, "dev_baseline:capture") {
						t.Fatal("settled preactivation hold retained owned reason")
					}
				}
			} else {
				baselineRetained(t, st, retentionNow)
			}
		})
	}
}

func TestDevBaselineRetentionFingerprintAndPermutation(t *testing.T) {
	st := baselineRetentionFixture()
	base := baselineRetained(t, st, retentionNow)
	for _, mode := range []string{"capture-status", "capture-phase", "capture-generation", "capture-config", "capture-authority", "capture-request", "hold-epoch", "hold-status", "hold-quarantine", "hold-proof", "result"} {
		t.Run(mode, func(t *testing.T) {
			changed := copyBaselineRetentionState(t, st)
			p := changed.Operations[1].DevBaseline
			switch mode {
			case "capture-status":
				changed.Operations[1].Status = "FAILED"
			case "capture-phase":
				changed.Operations[1].Phase = "UNKNOWN"
			case "capture-generation":
				p.Generation++
			case "capture-config":
				p.Components[0].ConfigHash = "different"
			case "capture-authority":
				p.Components[0].AuthorityHash = "different"
			case "capture-request":
				changed.Operations[1].RequestHash = "different"
			case "hold-epoch":
				changed.Operations[0].ReservationEpoch++
			case "hold-status":
				changed.Operations[0].Status = "FAILED"
			case "hold-quarantine":
				changed.Operations[0].ReservationQuarantined = true
			case "hold-proof":
				changed.Operations[0].DevReservation.ReservedSelection.Generation++
			case "result":
				changed.Operations[1].DevBaselineResult = &DevBaselineResult{Outcome: "FAILED", Reason: "registry_missing"}
			}
			if baselineRetained(t, changed, retentionNow).InputHash == base.InputHash {
				t.Fatal("capture authority mutation absent from fingerprint")
			}
		})
	}
	st.Operations[0], st.Operations[1] = st.Operations[1], st.Operations[0]
	if got := EvaluateRetention(st, "p", retentionNow, nil); !reflect.DeepEqual(base, got) {
		t.Fatal("record permutation changed protected inventory")
	}
}

func TestDevBaselineRetentionOwnedRootFeedsExistingRegistryClosure(t *testing.T) {
	st, authority := registryDomainFixture(t)
	capture := baselineRetentionFixture()
	capture.Compositions[0].Components = capture.Compositions[0].Components[:1]
	p := capture.Operations[1].DevBaseline
	p.Components = p.Components[:1]
	p.CompositionHash = checkHash(capture.Compositions[0])
	rehashBaselineRetention(p)
	st.Compositions = capture.Compositions
	st.Operations = append(st.Operations, capture.Operations...)
	at := retentionNow
	st.ArtifactPins[0].ReleasedAt = &at
	r := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{authority})
	image := retentionFind(t, r, retentionSHA(300))
	if !image.Protected || !retentionHas(image.Reasons, "dev_baseline:capture") {
		t.Fatal("manual pin release removed owned root")
	}
	roots := RegistryKnownRoots(r, authority.Scope.ImageRepository)
	if len(roots) != 1 || roots[0] != retentionSHA(300) {
		t.Fatal("baseline did not feed existing graph root selector", roots)
	}
	summary := registrySummaryForTest(t, r)
	if summary.RequiredRoots != 1 || summary.CoveredRoots != 1 || len(r.ProtectedObjects) != 7 || len(r.ProtectedEdges) != 10 || r.DeletionAllowed || r.GlobalInventoryComplete {
		t.Fatal("owned baseline changed scoped closure/global authority", summary)
	}
}

func TestDevBaselineRetentionDamagedProofKeepsIndependentObservedRoots(t *testing.T) {
	st := baselineRetentionFixture()
	p := st.Operations[1].DevBaseline
	p.Components[0].ObservedGitOps = &DevRuntimeGitOpsObservation{ImageRepository: p.Components[0].ImageRepository, Digest: p.Components[0].Digest}
	p.Components[0].Digest = retentionSHA(900)
	// Deliberately don't fix the immutable hash: all independently known roots
	// remain conservative protection, never authority from a damaged record.
	r := baselineRetained(t, st, retentionNow)
	if !retentionFind(t, r, retentionSHA(900)).Protected || !retentionHas(r.Blockers, "baseline_capture_proof_missing_or_invalid") {
		t.Fatal("damaged root/proof lost conservative protection")
	}
}

func TestDevBaselineRetentionSharedRepositoryFingerprint(t *testing.T) {
	st := baselineRetentionFixture()
	st.Products = append(st.Products, Product{Meta: Meta{ID: "consumer"}})
	st.Applications = append(st.Applications, Application{Meta: Meta{ID: "consumer-app", ProductID: "consumer"}, RepositoryID: "consumer-repo"})
	st.ComponentBuilds = append(st.ComponentBuilds, ComponentBuild{Meta: Meta{ProductID: "consumer"}, ApplicationID: "consumer-app", ImageRepository: "registry.example/team/app"})
	before := EvaluateRetention(st, "consumer", retentionNow, nil)
	v := retentionFind(t, before, retentionSHA(300))
	if !v.Protected || !retentionHas(v.Reasons, "shared_product_root") {
		t.Fatal("foreign baseline root not shared")
	}
	changed := copyBaselineRetentionState(t, st)
	changed.Operations[0].DevReservation.ReservedSelection.Generation++
	after := EvaluateRetention(changed, "consumer", retentionNow, nil)
	if before.InputHash == after.InputHash {
		t.Fatal("foreign baseline authority change hidden by shared projection")
	}
	unrelated := copyBaselineRetentionState(t, st)
	unrelated.Operations = append(unrelated.Operations, ExternalOperation{Meta: Meta{ID: "unrelated", ProductID: "p"}, Kind: "DEPLOY", Status: "RUNNING", EnvironmentID: "other-env"})
	if got := EvaluateRetention(unrelated, "consumer", retentionNow, nil); got.InputHash != before.InputHash {
		t.Fatal("unrelated foreign operation leaked into shared baseline fingerprint")
	}
}
