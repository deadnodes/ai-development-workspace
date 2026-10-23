package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func restorationCommand(t *testing.T, f *nativeFixture, action, parent, key string) domain.Command {
	t.Helper()
	st, e := f.store.Read(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	data := map[string]any{"request_key": key, "expected_selection": selectionToken(*environmentByID(&st, "dev"))}
	field := "restoration_operation_id"
	if action == "request_candidate_dev_restoration" {
		field = "reservation_operation_id"
	}
	data[field] = parent
	return domain.Command{Action: action, ProductID: "p", Actor: "operator", Data: data}
}
func completeRestorationRegistry(t *testing.T, f *nativeFixture) {
	t.Helper()
	ctx := context.Background()
	for {
		principal := registryPrincipal(t, f.s)
		lease, e := f.s.ClaimWorker(ctx, principal, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_observe"})
		if e != nil {
			t.Fatal(e)
		}
		if lease == nil {
			return
		}
		started, e := f.s.StartWorker(ctx, principal, workerRequest(lease))
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.s.ReportWorker(ctx, principal, baselineRegistryFixtureReport(t, &started)); e != nil {
			t.Fatal(e)
		}
	}
}
func TestDevRestorationNoEffectExpiryReplayAndScopedRelease(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, activation := devActivationMultiFixture(t, kind)
			before, _ := f.store.Read(ctx)
			candidate := operationByID(&before, hold.DevReservation.CandidateOperationID)
			originalHash := workerconfig.Fingerprint(candidate.MainCandidate)
			baseline, _ := latestBaseline(&before, hold.ID)
			baselineHash := baselineCaptureHash(*baseline)
			// Actual candidate/baseline records remain immutable. Only the private native
			// deadline decision seam advances; provider/registry/runtime clocks stay real.
			f.s.nativeAuthorityNow = func() time.Time { return candidate.CreatedAt.Add(2 * time.Hour) }
			if f.s.mainCandidateAllowed(&before, candidate) == nil {
				t.Fatal("expired candidate execution allowed")
			}
			if _, e := f.s.Execute(ctx, activation); e == nil {
				t.Fatal("expired candidate activation renewed")
			}
			command := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-request-no-effect-v1")
			var wg sync.WaitGroup
			results := make(chan domain.ExternalOperation, 2)
			errs := make(chan error, 2)
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, e := f.s.Execute(ctx, command)
					if e != nil {
						errs <- e
						return
					}
					results <- v.(domain.ExternalOperation)
				}()
			}
			wg.Wait()
			close(results)
			close(errs)
			for e := range errs {
				t.Fatal(e)
			}
			var parent domain.ExternalOperation
			for result := range results {
				if parent.ID != "" && result.ID != parent.ID {
					t.Fatal("concurrent identical key minted different attempts")
				}
				parent = result
			}
			var e error
			completeRestorationRegistry(t, f)
			for range 3 {
				forceActivationTick(t, f, parent.ID)
			}
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			if parent.Phase != "WAIT_EXTERNAL_MERGE" || parent.DevRestorationPlan == nil || parent.DevRestorationPlan.Hash == "" {
				t.Fatalf("plan not ready: %+v", parent)
			}
			if len(parent.DevRestorationPlan.Components) != 2 || p.proposals != 0 {
				t.Fatal("no-effect full baseline fabricated proposal")
			}
			for _, m := range parent.DevRestorationPlan.Components {
				if !m.NoWrite {
					t.Fatal("original member missing no-write proof")
				}
			}
			if p.sourceReads.Load() != 0 {
				t.Log("fixture initialization source reads", p.sourceReads.Load())
			}

			// Admitted current authority must reject material drift and corrupted plans.
			for name, mutate := range map[string]func(*domain.State){
				"epoch":      func(st *domain.State) { environmentByID(st, "dev").ReservationEpoch++ },
				"selection":  func(st *domain.State) { environmentByID(st, "dev").SelectionGeneration++ },
				"config":     func(st *domain.State) { environmentBinding(st, "dev", "component").Path = "foreign.yaml" },
				"grant":      func(st *domain.State) { st.ConnectionGrants = nil },
				"quarantine": func(st *domain.State) { operationByID(st, parent.ID).ReservationQuarantined = true },
				"duplicate-member": func(st *domain.State) {
					o := operationByID(st, parent.ID)
					o.DevRestorationPlan.Components[1] = o.DevRestorationPlan.Components[0]
					o.DevRestorationPlan.Hash = restorationPlanHash(*o.DevRestorationPlan)
				},
				"forged-target": func(st *domain.State) {
					o := operationByID(st, parent.ID)
					o.DevRestorationPlan.Components[0].Selector.Path = "other.yaml"
					o.DevRestorationPlan.Hash = restorationPlanHash(*o.DevRestorationPlan)
				},
				"duplicate-generation": func(st *domain.State) {
					o := *operationByID(st, parent.ID)
					o.ID = "duplicate-recovery"
					st.Operations = append(st.Operations, o)
				},
				"oversized": func(st *domain.State) {
					o := operationByID(st, parent.ID)
					o.DevRestorationPlan.SettlementHash = string(make([]byte, 256*1024))
				},
			} {
				t.Run(name, func(t *testing.T) {
					copy := cloneStateForActivation(t, st)
					mutate(&copy)
					if f.s.restorationAllowed(&copy, *operationByID(&copy, parent.ID), true) == nil {
						t.Fatal("changed recovery authority accepted")
					}
				})
			}
			bad := command
			bad.Actor = "foreign"
			if _, e = f.s.Execute(ctx, bad); e == nil {
				t.Fatal("request key reused by foreign actor")
			}
			if _, e = f.s.Execute(ctx, domain.Command{Action: "release_candidate_dev", ProductID: "p", Actor: "operator", Data: map[string]any{"reservation_operation_id": hold.ID, "request_key": command.Data["request_key"], "expected_selection": parent.DevRestoration.Selection}}); e == nil {
				t.Fatal("Product receipt key reused across actions")
			}
			confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "restore-confirm-no-effect-v1")
			confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			cv, e := f.s.Execute(ctx, confirm)
			if e != nil {
				t.Fatal(e)
			}
			if cv.(domain.ExternalOperation).DevRestorationConfirmation.TrustGrade != "ATTRIBUTED_CONTROL_CONFIRMATION" {
				t.Fatal("spoofed human grade")
			}
			forceActivationTick(t, f, parent.ID)
			st, _ = f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			if parent.Phase != "GITOPS_APPLIED" {
				t.Fatal("no-effect original desired state not proven", parent.Phase)
			}
			verify := restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "restore-verify-no-effect-v1")
			vv, e := f.s.Execute(ctx, verify)
			if e != nil {
				t.Fatal(e)
			}
			proof := vv.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, proof.ID)
			st, _ = f.store.Read(ctx)
			proof = *operationByID(&st, proof.ID)
			if proof.Status != "SUCCEEDED" || proof.Phase != "RESTORATION_VERIFIED" {
				t.Fatalf("runtime proof failed %+v", proof)
			}

			beforeRetention := f.s.evaluateRetention(st, "p", time.Now().UTC())
			for _, c := range parent.DevRestoration.Components {
				protected := false
				for _, image := range beforeRetention.Images {
					if image.ImageRepository == c.ImageRepository && image.Digest == c.Digest && image.Protected {
						protected = true
					}
				}
				if !protected {
					t.Fatal("active recovery original root unprotected")
				}
			}
			release := restorationCommand(t, f, "release_candidate_dev_after_restoration", parent.ID, "restore-release-no-effect-v1")
			release.Data["restoration_verification_operation_id"] = proof.ID
			rv, e := f.s.Execute(ctx, release)
			if e != nil {
				t.Fatal(e)
			}
			receipt := rv.(domain.ExternalOperation)
			st, _ = f.store.Read(ctx)
			if activeDevReservation(&st, "dev") != nil || !domain.ValidRestorationRelease(&st, receipt, *operationByID(&st, hold.ID)) {
				t.Fatal("exact restoration release not recognized")
			}

			afterRetention := f.s.evaluateRetention(st, "p", time.Now().UTC())
			if afterRetention.DeletionAllowed || afterRetention.GlobalInventoryComplete {
				t.Fatal("scoped release granted global GC")
			}
			for _, image := range afterRetention.Images {
				for _, reason := range image.Reasons {
					if reason == "dev_baseline:"+baseline.ID || reason == "dev_restoration:"+parent.ID {
						t.Fatal("exact settled successor retained active owned roots")
					}
				}
			}
			if beforeRetention.InputHash == afterRetention.InputHash {
				t.Fatal("retention fingerprint ignored release authority")
			}
			if workerconfig.Fingerprint(operationByID(&st, candidate.ID).MainCandidate) != originalHash {
				t.Fatal("candidate mutated/resealed")
			}
			afterBaseline, _ := latestBaseline(&st, hold.ID)
			if baselineCaptureHash(*afterBaseline) != baselineHash {
				t.Fatal("baseline mutated/resealed")
			}
			if environmentByID(&st, "dev").DesiredCompositionID != hold.DevReservation.ReservedSelection.CompositionID {
				t.Fatal("unlock replaced baseline selection")
			}
			for _, replay := range []domain.Command{command, confirm, verify, release} {
				if _, e = f.s.Execute(ctx, replay); e != nil {
					t.Fatal("historical replay after release", e)
				}
			}

			if e = f.store.Update(ctx, func(st *domain.State) error {
				st.Operations = append(st.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "malformed-recovery-orphan", ProductID: "p"}, Kind: "DEPLOY", ParentID: "missing-recovery-parent", RequestedBy: "operator", Status: "SUCCEEDED", DevRestorationRelease: &domain.DevRestorationRelease{}})
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			archive, e := f.s.CreateBackup(ctx, "operator")
			if e != nil {
				t.Fatal(e)
			}
			destStore, _ := openWorkerTestStore(t, kind)
			defer destStore.Close()
			dest := NewWithProvider(destStore, p)
			dest.ConfigureWorkers(f.s.externalWorkers)
			dest.SetRuntimeObservers(f.s.runtimeObservers)
			if _, e = dest.RestoreBackup(ctx, "restore-owner", archive.ArchiveBase64, archive.SHA256); e != nil {
				t.Fatal(e)
			}
			imported, _ := destStore.Read(ctx)
			if orphan := operationByID(&imported, "malformed-recovery-orphan"); orphan == nil || !orphan.ReservationQuarantined {
				t.Fatal("malformed imported recovery orphan not quarantined")
			}
			if activeDevReservation(&imported, "dev") == nil || !operationByID(&imported, receipt.ID).ReservationQuarantined || !operationByID(&imported, parent.ID).ReservationQuarantined || !operationByID(&imported, proof.ID).ReservationQuarantined {
				t.Fatal("imported successor revived/unlocked recovery authority")
			}
			for _, replay := range []domain.Command{command, confirm, verify, release} {
				if _, e = dest.Execute(ctx, replay); e != nil {
					t.Fatal("quarantined historical replay", e)
				}
			}
			portable, e := f.s.ExportWorkspace(ctx, "p")
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(portable)
			if strings.Contains(string(raw), "dev_restoration") || strings.Contains(string(raw), parent.ID) {
				t.Fatal("portable export leaked recovery authority")
			}
			for _, mutate := range []func(*domain.State){func(st *domain.State) { operationByID(st, proof.ID).DevRuntimeResult.Hash = "bad" }, func(st *domain.State) { operationByID(st, receipt.ID).DevRestorationRelease.PlanHash = "bad" }, func(st *domain.State) { operationByID(st, parent.ID).ReservationQuarantined = true }} {
				copy := cloneStateForActivation(t, st)
				mutate(&copy)
				if activeDevReservation(&copy, "dev") == nil {
					t.Fatal("forged/quarantined release unlocked hold")
				}
			}
			t.Log("focused expiry decision seam; actual native build/publisher fixture and isolated store; registry/Kube/GitHub attestation peers simulated; no one-hour wait")
		})
	}
}
func TestDevRestorationStrictRawCommand(t *testing.T) {
	for _, raw := range []string{`{"action":"request_candidate_dev_restoration","product_id":"p","actor":"operator","id":"","data":{}}`, `{"action":"confirm_candidate_dev_restoration","product_id":"p","actor":"operator","data":{"request_key":"a","request_key":"b"}}`, `{"action":"verify_candidate_dev_restoration","product_id":"p","actor":"operator","data":{"Expected_selection":{}}}`} {
		var c domain.Command
		if json.Unmarshal([]byte(raw), &c) == nil {
			t.Fatal("noncanonical raw restoration admitted", raw)
		}
	}
}
func TestDevRestorationUnknownOriginalEffectFencesAndBlocksRenewal(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, activation := devActivationFixture(t, kind)
			v, e := f.s.Execute(ctx, activation)
			if e != nil {
				t.Fatal(e)
			}
			old := v.(domain.ExternalOperation)
			p.lost = true
			p.absent = true
			activationTicks(t, f, 10)
			st, _ := f.store.Read(ctx)
			child := operationByID(&st, old.ChildIDs[0])
			if child.DevActivationBatch == nil || child.Phase != "APPLY_UNCERTAIN" {
				t.Fatal("fixture did not create real ambiguous intent")
			}
			p.sourceReads.Store(0)
			command := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-unknown-original-v1")
			v, e = f.s.Execute(ctx, command)
			if e != nil {
				t.Fatal(e)
			}
			restore := v.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, restore.ID)
			st, _ = f.store.Read(ctx)
			if operationByID(&st, restore.ID).Phase != "RECONCILE_EFFECTS" || p.proposals != 1 || activeDevReservation(&st, "dev") == nil {
				t.Fatal("unknown effect omitted or hold released")
			}
			forceActivationTick(t, f, old.ChildIDs[0])
			if p.proposals != 1 || p.sourceReads.Load() != 0 {
				t.Fatal("old fenced job performed source IO/write")
			}
			renew := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-unknown-renewal-v1")
			if _, e = f.s.Execute(ctx, renew); e == nil {
				t.Fatal("unknown original intent renewed")
			}
			if p.sourceReads.Load() != 0 {
				t.Fatal("recovery performed candidate-main/source preflight")
			}
		})
	}
}
func TestDevRestorationCompleteThreeComponentBaselineIncludesUntouchedC(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, reserve := devReservationFixture(t, kind)
			source := f.s.provider.(*mainCandidateProvider)
			build, _ := f.s.externalWorkers.Recipe("build")
			pubs := []workerconfig.PublicationRecipe{f.recipe}
			for _, id := range []string{"untouched-b", "untouched-c"} {
				r := f.recipe
				r.ID = "publish-" + id
				r.ComponentID = id
				r.Repository += "-" + id
				r.Hash = workerconfig.PublicationHash(r)
				pubs = append(pubs, r)
			}
			cfg := workerconfig.Config{Revision: f.s.externalWorkers.Revision(), LeaseSeconds: 5, Recipes: []workerconfig.Recipe{build}, Publications: pubs, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}
			authority, e := workerconfig.New(cfg, "native-control-fixture", os.Getenv)
			if e != nil {
				t.Fatal(e)
			}
			f.s.ConfigureWorkers(authority)
			for _, r := range pubs[1:] {
				exec(t, f.s, domain.Command{Action: "create_application", ID: r.ComponentID, ProductID: "p", Data: map[string]any{"name": r.ComponentID, "repository_id": "source"}})
				exec(t, f.s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": r.ComponentID, "connection_id": "conn", "mode": "NATIVE", "profile": r.Profile, "image_repository": nativeImage(r), "native": domain.NativeBinding{BuildRecipeID: build.ID, PublicationRecipeID: r.ID, Parameters: map[string]any{}}}})
				exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": r.ComponentID, "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": r.ComponentID + ".yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
			}
			exec(t, f.s, domain.Command{Action: "plan_composition", ID: "three-originals", ProductID: "p", Data: map[string]any{"name": "Complete A+B+C", "environment_id": "dev", "components": []domain.CompositionComponent{component("component", "rev"), component("untouched-b", "rev"), component("untouched-c", "rev")}}})
			exec(t, f.s, domain.Command{Action: "select_composition", ID: "three-originals"})
			state, _ := f.store.Read(ctx)
			reserve.Data["expected_selection"] = selectionToken(*environmentByID(&state, "dev"))
			v, e := f.s.Execute(ctx, reserve)
			if e != nil {
				t.Fatal(e)
			}
			hold := v.(domain.ExternalOperation)
			p := &devActivationProvider{mainCandidateProvider: source}
			f.s.provider = p
			prepareActivationFixtureBaseline(t, f, p, hold)
			request := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-three-originals-v1")
			v, e = f.s.Execute(ctx, request)
			if e != nil {
				t.Fatal(e)
			}
			parent := v.(domain.ExternalOperation)
			if len(parent.DevRestoration.Components) != 3 {
				t.Fatal("untouched baseline omitted")
			}
			completeRestorationRegistry(t, f)
			for range 3 {
				forceActivationTick(t, f, parent.ID)
			}
			state, _ = f.store.Read(ctx)
			parent = *operationByID(&state, parent.ID)
			if parent.Phase != "WAIT_EXTERNAL_MERGE" || len(parent.DevRestorationPlan.Components) != 3 || p.proposals != 0 {
				t.Fatal("complete three-component original plan missing")
			}
			confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "restore-three-confirm-v1")
			confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			if _, e = f.s.Execute(ctx, confirm); e != nil {
				t.Fatal(e)
			}
			forceActivationTick(t, f, parent.ID)
			verify := restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "restore-three-verify-v1")
			v, e = f.s.Execute(ctx, verify)
			if e != nil {
				t.Fatal(e)
			}
			proof := v.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, proof.ID)
			state, _ = f.store.Read(ctx)
			proof = *operationByID(&state, proof.ID)
			if proof.Status != "SUCCEEDED" || len(proof.DevRuntimeResult.Components) != 3 {
				t.Fatal("untouched baseline runtime omitted")
			}
			roles := 0
			for _, r := range proof.DevRuntimeResult.Components {
				roles += len(r.Roles)
			}
			if roles != 6 {
				t.Fatal("complete configured role coverage missing")
			}
			// A pending newer proof immediately invalidates the old PASS and scoped unlock.
			newer := restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "restore-three-newproof-v1")
			if _, e = f.s.Execute(ctx, newer); e != nil {
				t.Fatal(e)
			}
			state, _ = f.store.Read(ctx)
			if f.s.restorationQuery(&state, "dev")["restoration_verified"] != false {
				t.Fatal("pending new proof retained oldPASS")
			}
			release := restorationCommand(t, f, "release_candidate_dev_after_restoration", parent.ID, "restore-three-stalerelease-v1")
			release.Data["restoration_verification_operation_id"] = proof.ID
			if _, e = f.s.Execute(ctx, release); e == nil {
				t.Fatal("olderPASS released after newer pending proof")
			}
		})
	}
}
func TestDevRestorationMissingFreshOriginalImageContentBlocks(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, _ := devActivationFixture(t, kind)
			request := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-missing-current-bytes-v1")
			v, e := f.s.Execute(ctx, request)
			if e != nil {
				t.Fatal(e)
			}
			parent := v.(domain.ExternalOperation)
			principal := registryPrincipal(t, f.s)
			lease, e := f.s.ClaimWorker(ctx, principal, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_observe"})
			if e != nil || lease == nil {
				t.Fatal("current registry job missing", e)
			}
			started, e := f.s.StartWorker(ctx, principal, workerRequest(lease))
			if e != nil {
				t.Fatal(e)
			}
			// Generic registry metadata without independently observed config/layer content
			// must not inherit the historical baseline's prior IMAGE_CONTENT proof.
			report := registryReport(t, &started, true)
			if _, e = f.s.ReportWorker(ctx, principal, report); e != nil {
				t.Fatal(e)
			}
			forceActivationTick(t, f, parent.ID)
			st, _ := f.store.Read(ctx)
			current := operationByID(&st, parent.ID)
			if current.DevRestorationPlan != nil || current.Phase != "RECONCILE_EFFECTS" || p.proposals != 0 || activeDevReservation(&st, "dev") == nil {
				t.Fatal("missing fresh original content authorized restoration or unlock")
			}
		})
	}
}

// Regression for successor-only corruption: immutable hold, baseline and recovery plan stay unchanged.
func TestDevRestorationSuccessorNestedEvidenceTampering(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, hold, _ := devActivationMultiFixture(t, kind)
			command := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "independent-restoration-request")
			raw, err := f.s.Execute(ctx, command)
			if err != nil {
				t.Fatal(err)
			}
			parent := raw.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			for range 3 {
				forceActivationTick(t, f, parent.ID)
			}
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "independent-restoration-confirm")
			confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			cr, err := f.s.Execute(ctx, confirm)
			if err != nil {
				t.Fatal(err)
			}
			confirmation := cr.(domain.ExternalOperation)
			forceActivationTick(t, f, parent.ID)
			vr, err := f.s.Execute(ctx, restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "independent-restoration-verify"))
			if err != nil {
				t.Fatal(err)
			}
			verification := vr.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, verification.ID)
			release := restorationCommand(t, f, "release_candidate_dev_after_restoration", parent.ID, "independent-restoration-release")
			release.Data["restoration_verification_operation_id"] = verification.ID
			rr, err := f.s.Execute(ctx, release)
			if err != nil {
				t.Fatal(err)
			}
			receipt := rr.(domain.ExternalOperation)
			st, _ = f.store.Read(ctx)
			if !domain.ValidRestorationRelease(&st, receipt, *operationByID(&st, hold.ID)) {
				t.Fatal("valid control chain rejected")
			}
			for name, mutate := range map[string]func(*domain.State){
				"runtime_expected_hash": func(cp *domain.State) {
					v := operationByID(cp, verification.ID)
					v.DevRuntimeResult.ExpectedHash = "forged-unbound-runtime-input"
					v.DevRuntimeResult.Hash = devRuntimeResultHash(*v.DevRuntimeResult)
					r := operationByID(cp, receipt.ID)
					r.DevRestorationRelease.ResultHash = v.DevRuntimeResult.Hash
					r.DevRestorationRelease.Hash = restorationReleaseHash(*r.DevRestorationRelease)
				},
				"runtime_empty_membership": func(cp *domain.State) {
					v := operationByID(cp, verification.ID)
					v.DevRestorationVerification.Runtime.Components = nil
					v.DevRestorationVerification.Hash = restorationVerificationHash(*v.DevRestorationVerification)
					r := operationByID(cp, receipt.ID)
					r.DevRestorationRelease.VerificationHash = v.DevRestorationVerification.Hash
					r.DevRestorationRelease.Hash = restorationReleaseHash(*r.DevRestorationRelease)
				},
				"confirmation_nested_plan": func(cp *domain.State) {
					c := operationByID(cp, confirmation.ID)
					c.DevRestorationConfirmation.Plan.Components[0].Original.Digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
					c.DevRestorationConfirmation.Hash = restorationConfirmationHash(*c.DevRestorationConfirmation)
					v := operationByID(cp, verification.ID)
					v.DevRestorationVerification.ConfirmationHash = c.DevRestorationConfirmation.Hash
					v.DevRestorationVerification.Hash = restorationVerificationHash(*v.DevRestorationVerification)
					r := operationByID(cp, receipt.ID)
					r.DevRestorationRelease.ConfirmationHash = c.DevRestorationConfirmation.Hash
					r.DevRestorationRelease.VerificationHash = v.DevRestorationVerification.Hash
					r.DevRestorationRelease.Hash = restorationReleaseHash(*r.DevRestorationRelease)
				},
			} {
				t.Run(name, func(t *testing.T) {
					cp := cloneStateForActivation(t, st)
					mutate(&cp)
					r := operationByID(&cp, receipt.ID)
					if domain.ValidRestorationRelease(&cp, *r, *operationByID(&cp, hold.ID)) {
						t.Error("invalid nested successor chain recognized as valid and settles hold")
					}
					if activeDevReservation(&cp, "dev") == nil {
						t.Error("invalid nested successor chain unlocked active hold")
					}
				})
			}
		})
	}
}

type restorationConfirmationAdmissionBarrier struct {
	Store
	arrivals atomic.Int32
	release  chan struct{}
}

func (b *restorationConfirmationAdmissionBarrier) Update(ctx context.Context, fn func(*domain.State) error) error {
	if n := b.arrivals.Add(1); n <= 2 {
		if n == 2 {
			close(b.release)
		}
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.Store.Update(ctx, fn)
}

func TestDevRestorationConcurrentDistinctKeyConfirmation(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			f, _, hold, _ := devActivationMultiFixture(t, kind)
			raw, err := f.s.Execute(ctx, restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "concurrent-confirm-request"))
			if err != nil {
				t.Fatal(err)
			}
			parent := raw.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			for range 3 {
				forceActivationTick(t, f, parent.ID)
			}
			st, err := f.store.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			parent = *operationByID(&st, parent.ID)
			if parent.DevRestorationPlan == nil {
				t.Fatal("prepared plan missing")
			}
			commands := [2]domain.Command{}
			for i, key := range []string{"concurrent-confirm-key-a", "concurrent-confirm-key-b"} {
				commands[i] = restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, key)
				commands[i].Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			}
			underlying := f.s.store
			barrier := &restorationConfirmationAdmissionBarrier{Store: underlying, release: make(chan struct{})}
			f.s.store = barrier
			var results [2]any
			var errs [2]error
			var wg sync.WaitGroup
			for i := range commands {
				wg.Add(1)
				go func(i int) { defer wg.Done(); results[i], errs[i] = f.s.Execute(ctx, commands[i]) }(i)
			}
			wg.Wait()
			f.s.store = underlying
			if barrier.arrivals.Load() != 2 {
				t.Fatal("both commands did not reach transaction barrier")
			}
			winner, rejected := -1, 0
			for i, err := range errs {
				if err == nil {
					if winner != -1 {
						t.Fatal("two distinct-key confirmations admitted")
					}
					winner = i
				} else if errors.Is(err, ErrValidation) {
					rejected++
				} else {
					t.Fatal(err)
				}
			}
			if winner == -1 || rejected != 1 {
				t.Fatalf("expected one success and one validation error: %v", errs)
			}
			st, err = f.store.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, op := range st.Operations {
				if op.Kind == devRestorationConfirmationKind && op.ParentID == parent.ID {
					count++
				}
			}
			if count != 1 || restorationConfirmation(&st, *operationByID(&st, parent.ID)) == nil {
				t.Fatal("confirmation history is not exactly one valid receipt")
			}
			replay, err := f.s.Execute(ctx, commands[winner])
			if err != nil || replay.(domain.ExternalOperation).ID != results[winner].(domain.ExternalOperation).ID {
				t.Fatal("winning same-key historical replay changed", err)
			}
			forceActivationTick(t, f, parent.ID)
			vr, err := f.s.Execute(ctx, restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "concurrent-confirm-verify"))
			if err != nil {
				t.Fatal(err)
			}
			verification := vr.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, verification.ID)
			release := restorationCommand(t, f, "release_candidate_dev_after_restoration", parent.ID, "concurrent-confirm-release")
			release.Data["restoration_verification_operation_id"] = verification.ID
			rr, err := f.s.Execute(ctx, release)
			if err != nil {
				t.Fatal(err)
			}
			st, err = f.store.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if activeDevReservation(&st, "dev") != nil || !domain.ValidRestorationRelease(&st, rr.(domain.ExternalOperation), *operationByID(&st, hold.ID)) {
				t.Fatal("single admitted confirmation cannot complete fresh proof/unlock")
			}
		})
	}
}

func TestDevRestorationExpiredProposalFreshVerificationAndUnlock(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, hold, _ := devActivationMultiFixture(t, kind)
			command := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "independent-restoration-request")
			raw, err := f.s.Execute(ctx, command)
			if err != nil {
				t.Fatal(err)
			}
			parent := raw.(domain.ExternalOperation)
			original, _ := f.store.Read(ctx)
			originalHold := workerconfig.Fingerprint(operationByID(&original, hold.ID).DevReservation)
			originalCandidate := workerconfig.Fingerprint(operationByID(&original, hold.DevReservation.CandidateOperationID).MainCandidate)
			baseline, _ := latestBaseline(&original, hold.ID)
			originalBaseline := baselineCaptureHash(*baseline)
			completeRestorationRegistry(t, f)
			for range 3 {
				forceActivationTick(t, f, parent.ID)
			}
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			originalRecovery := workerconfig.Fingerprint(parent.DevRestoration)
			// Only proposal deadline decisions advance. All admitted records,
			// provider reads, fresh proofs and unlock authority retain real time.
			f.s.restorationAuthorityNow = func() time.Time { return parent.CreatedAt.Add(2 * time.Hour) }
			aged, _ := f.store.Read(ctx)
			if e := f.s.restorationAllowed(&aged, *operationByID(&aged, parent.ID), true); e == nil || !strings.Contains(e.Error(), "restoration-only write authority expired") {
				t.Fatal("expected exact expired restoration write denial", e)
			}
			if err = f.s.restorationAllowed(&aged, *operationByID(&aged, parent.ID), false); err != nil {
				t.Fatal(err)
			}
			confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "independent-restoration-confirm")
			confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			cr, err := f.s.Execute(ctx, confirm)
			if err != nil {
				t.Fatal(err)
			}
			_ = cr.(domain.ExternalOperation)
			forceActivationTick(t, f, parent.ID)
			vr, err := f.s.Execute(ctx, restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "independent-restoration-verify"))
			if err != nil {
				t.Fatal(err)
			}
			verification := vr.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, verification.ID)
			release := restorationCommand(t, f, "release_candidate_dev_after_restoration", parent.ID, "independent-restoration-release")
			release.Data["restoration_verification_operation_id"] = verification.ID
			rr, err := f.s.Execute(ctx, release)
			if err != nil {
				t.Fatal(err)
			}
			receipt := rr.(domain.ExternalOperation)
			st, _ = f.store.Read(ctx)
			if !domain.ValidRestorationRelease(&st, receipt, *operationByID(&st, hold.ID)) {
				t.Fatal("valid control chain rejected")
			}
			if workerconfig.Fingerprint(operationByID(&st, hold.ID).DevReservation) != originalHold {
				t.Fatal("original hold changed")
			}
			if workerconfig.Fingerprint(operationByID(&st, hold.DevReservation.CandidateOperationID).MainCandidate) != originalCandidate || workerconfig.Fingerprint(operationByID(&st, parent.ID).DevRestoration) != originalRecovery {
				t.Fatal("original candidate/recovery proof changed")
			}
			after, _ := latestBaseline(&st, hold.ID)
			if baselineCaptureHash(*after) != originalBaseline {
				t.Fatal("original baseline changed")
			}
			if activeDevReservation(&st, "dev") != nil {
				t.Fatal("fresh scoped unlock after proposal expiry retained hold")
			}
			t.Log("proposal deadline decision clock +2h; real local/PG fresh confirmation, verification, availability and unlock; all admitted records immutable")
		})
	}
}

func TestDevRestorationSettledRetentionIsolation(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, hold, _ := devActivationMultiFixture(t, kind)
			raw, err := f.s.Execute(ctx, restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "independent-retention-request"))
			if err != nil {
				t.Fatal(err)
			}
			parent := raw.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			for range 3 {
				forceActivationTick(t, f, parent.ID)
			}
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			cc := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "independent-retention-confirm")
			cc.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			raw, err = f.s.Execute(ctx, cc)
			if err != nil {
				t.Fatal(err)
			}
			confirmation := raw.(domain.ExternalOperation)
			forceActivationTick(t, f, parent.ID)
			raw, err = f.s.Execute(ctx, restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "independent-retention-verify"))
			if err != nil {
				t.Fatal(err)
			}
			verification := raw.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, verification.ID)
			rc := restorationCommand(t, f, "release_candidate_dev_after_restoration", parent.ID, "independent-retention-release")
			rc.Data["restoration_verification_operation_id"] = verification.ID
			raw, err = f.s.Execute(ctx, rc)
			if err != nil {
				t.Fatal(err)
			}
			release := raw.(domain.ExternalOperation)
			st, _ = f.store.Read(ctx)
			if !domain.ValidRestorationRelease(&st, release, *operationByID(&st, hold.ID)) {
				t.Fatal("valid released lifecycle required")
			}
			base := domain.EvaluateRetention(st, "p", time.Now().UTC(), nil)
			for _, im := range base.Images {
				for _, reason := range im.Reasons {
					if strings.HasPrefix(reason, "dev_restoration:") {
						t.Fatal("validated original chain remained permanently pinned", im)
					}
				}
			}
			digest := fmt.Sprintf("sha256:%064x", 888)
			repo := "registry.example/independent/unbound-nested"
			for _, name := range []string{"orphan_confirmation", "quarantined_confirmation", "orphan_verification", "orphan_release", "extra_availability_valid_confirmation", "foreign_application_same_image", "empty_application_same_image", "foreign_release_cannot_settle_own_orphan"} {
				t.Run(name, func(t *testing.T) {
					cp := cloneStateForActivation(t, st)
					wantRepo, wantDigest := repo, digest
					o := domain.ExternalOperation{Meta: domain.Meta{ID: "retention-orphan", ProductID: "p"}, ParentID: parent.ID, Kind: "DEV_RESTORATION_CONFIRMATION", Status: "FAILED"}
					expectedReason := "dev_restoration:" + o.ID
					member := domain.DevRestorationMember{ApplicationID: "component", Original: domain.DevRuntimeGitOpsObservation{ImageRepository: repo, Digest: digest}}
					result := domain.DevBaselineComponentResult{ApplicationID: "component", ImageRepository: repo, Digest: digest}
					switch name {
					case "orphan_confirmation", "quarantined_confirmation", "foreign_release_cannot_settle_own_orphan":
						o.DevRestorationConfirmation = &domain.DevRestorationConfirmation{Plan: domain.DevRestorationPlan{Components: []domain.DevRestorationMember{member}}}
						o.ReservationQuarantined = name == "quarantined_confirmation"
					case "orphan_verification":
						o.Kind = "DEV_RESTORATION_VERIFICATION"
						o.DevRestorationVerification = &domain.DevRestorationVerification{Runtime: domain.DevRuntimeProof{Components: []domain.DevRuntimeComponent{{ApplicationID: "component", ImageRepository: repo, Digest: digest}}}}
					case "orphan_release":
						o.Kind = "DEV_RESTORATION_RELEASE"
						o.DevRestorationRelease = &domain.DevRestorationRelease{ReservationOperationID: hold.ID, Availability: []domain.DevBaselineComponentResult{result}}
					case "extra_availability_valid_confirmation", "foreign_application_same_image", "empty_application_same_image":
						if name != "extra_availability_valid_confirmation" {
							owned := parent.DevRestoration.Components[0]
							wantRepo, wantDigest = owned.ImageRepository, owned.Digest
							result.ImageRepository, result.Digest = wantRepo, wantDigest
							result.ApplicationID = "foreign-application"
							if name == "empty_application_same_image" {
								result.ApplicationID = ""
							}
						}
						c := operationByID(&cp, confirmation.ID)
						c.DevRestorationAvailability = []domain.DevBaselineComponentResult{result}
						expectedReason = "dev_restoration:" + confirmation.ID
						if !domain.ValidRestorationRelease(&cp, *operationByID(&cp, release.ID), *operationByID(&cp, hold.ID)) {
							t.Fatal("auxiliary availability unexpectedly invalidated validated release control")
						}
					}
					if expectedReason == "dev_restoration:"+o.ID {
						cp.Operations = append(cp.Operations, o)
					}
					if name == "foreign_release_cannot_settle_own_orphan" {
						q := *operationByID(&cp, release.ID)
						q.ID = "foreign-release"
						q.ProductID = "foreign"
						cp.Operations = append(cp.Operations, q)
					}
					r := domain.EvaluateRetention(cp, "p", time.Now().UTC(), nil)
					found := false
					for _, im := range r.Images {
						if im.ImageRepository == wantRepo && im.Digest == wantDigest {
							for _, reason := range im.Reasons {
								if reason == expectedReason && im.Protected {
									found = true
								}
							}
						}
					}
					if !found {
						t.Error("unvalidated unique nested image borrowed settlement from valid parent", r)
					}
					if r.DeletionAllowed {
						t.Fatal("projection enabled physical deletion")
					}
				})
			}
		})
	}
}
