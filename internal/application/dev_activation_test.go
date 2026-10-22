package application

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/workerconfig"
)

type devActivationProvider struct {
	desiredImages   map[string]string
	desiredDigests  map[string]string
	baselineImages  map[string]string
	baselineDigests map[string]string
	baselineHead    string
	*mainCandidateProvider
	proposals, lookups, observations int
	lost, merged, closed, absent     bool
	beforePropose, afterPropose      func()
	digest, image                    string
	failPath                         string
	lookupHead                       string
	sourceReads, uncachedSourceReads atomic.Int64
	physicalWrites                   int
	afterPhysicalWrite               func(int)
}

func (p *devActivationProvider) Branches(ctx context.Context, c delivery.Connection, r string) ([]delivery.BranchInfo, error) {
	p.sourceReads.Add(1)
	if !delivery.CachedAuthenticationOnly(ctx) {
		p.uncachedSourceReads.Add(1)
	}
	return p.mainCandidateProvider.Branches(ctx, c, r)
}

func (p *devActivationProvider) ProposeGitOpsBatchGuarded(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply, guard func(context.Context) error) (delivery.GitOpsPullRequest, error) {
	for range 3 {
		if e := guard(ctx); e != nil {
			return delivery.GitOpsPullRequest{}, e
		}
		p.physicalWrites++
		if p.afterPhysicalWrite != nil {
			p.afterPhysicalWrite(p.physicalWrites)
		}
	}
	p.proposals++
	if p.beforePropose != nil {
		p.beforePropose()
	}
	if p.desiredImages == nil {
		p.desiredImages = map[string]string{}
		p.desiredDigests = map[string]string{}
	}
	for _, v := range b.Changes {
		if v.Selector.Path == p.failPath {
			return delivery.GitOpsPullRequest{}, errors.New("uncertain group")
		}
		key := v.Selector.Path + "|" + v.ImageRepository
		p.desiredImages[key] = v.ImageRepository
		p.desiredDigests[key] = v.Digest
		p.digest, p.image = v.Digest, v.ImageRepository
	}
	if p.afterPropose != nil {
		p.afterPropose()
	}
	if p.lost {
		return delivery.GitOpsPullRequest{}, errors.New("lost group response")
	}
	return delivery.GitOpsPullRequest{Number: 81, URL: "https://fixture.invalid/pulls/81", HeadSHA: strings.Repeat("a", 40), State: "open"}, nil
}
func (p *devActivationProvider) LookupGitOpsBatchProposal(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply) (delivery.GitOpsPullRequest, error) {
	for _, v := range b.Changes {
		if p.absent && (p.failPath == "" || v.Selector.Path == p.failPath) {
			p.lookups++
			return delivery.GitOpsPullRequest{}, nil
		}
	}
	return p.LookupGitOpsProposal(ctx, c, delivery.GitOpsApply{Request: b.Changes[0].Selector})
}
func (p *devActivationProvider) ObserveGitOpsBatchPR(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply, pr delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	return p.ObserveGitOpsPR(ctx, c, delivery.GitOpsApply{}, pr)
}

func (p *devActivationProvider) ProposeGitOpsGuarded(ctx context.Context, c delivery.Connection, r delivery.GitOpsApply, guard func(context.Context) error) (delivery.GitOpsPullRequest, error) {
	if guard == nil {
		return delivery.GitOpsPullRequest{}, errors.New("guard required")
	}
	// Model branch/commit/PR mutation boundaries. Production httptest coverage
	// separately verifies the guard at the actual HTTP sender.
	for range 3 {
		if err := guard(ctx); err != nil {
			return delivery.GitOpsPullRequest{}, err
		}
		p.physicalWrites++
		if p.afterPhysicalWrite != nil {
			p.afterPhysicalWrite(p.physicalWrites)
		}
	}
	return p.ProposeGitOps(ctx, c, r)
}

func (p *devActivationProvider) ProposeGitOps(_ context.Context, _ delivery.Connection, r delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	p.proposals++
	if r.Request.Path == p.failPath {
		return delivery.GitOpsPullRequest{}, errors.New("second component proposal outcome uncertain")
	}
	p.digest, p.image = r.Digest, r.Request.ImageRepository
	if p.desiredImages == nil {
		p.desiredImages = map[string]string{}
		p.desiredDigests = map[string]string{}
	}
	key := r.Request.Path + "|" + r.Request.ImageRepository
	p.desiredImages[key] = r.Request.ImageRepository
	p.desiredDigests[key] = r.Digest
	if p.beforePropose != nil {
		p.beforePropose()
	}
	pr := delivery.GitOpsPullRequest{Number: 81, URL: "https://fixture.invalid/pulls/81", HeadSHA: strings.Repeat("a", 40), State: "open"}
	if p.afterPropose != nil {
		p.afterPropose()
	}
	if p.lost {
		return delivery.GitOpsPullRequest{}, errors.New("response lost after external mutation")
	}
	return pr, nil
}
func (p *devActivationProvider) ReadGitOps(ctx context.Context, c delivery.Connection, r delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	result, err := p.mainCandidateProvider.ReadGitOps(ctx, c, r)
	if !p.merged && p.baselineImages != nil {
		key := r.Path + "|" + r.ImageRepository
		result.ImageRepository = p.baselineImages[key]
		result.Digest = p.baselineDigests[key]
		result.HeadSHA = p.baselineHead
		result.BlobSHA = strings.Repeat("d", 40)
		result.Content = "fixture baseline complete shared Helm bytes"
	}
	if p.merged {
		result.HeadSHA = strings.Repeat("f", 40)
		result.BlobSHA = strings.Repeat("d", 40)
		result.Digest, result.ImageRepository = p.digest, p.image
		if p.desiredImages != nil {
			key := r.Path + "|" + r.ImageRepository
			result.Digest = p.desiredDigests[key]
			result.ImageRepository = p.desiredImages[key]
		}
	}
	return result, err
}
func (p *devActivationProvider) LookupGitOpsProposal(_ context.Context, _ delivery.Connection, r delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	p.lookups++
	if p.absent && (p.failPath == "" || p.failPath == r.Request.Path) {
		return delivery.GitOpsPullRequest{}, nil
	}
	head := p.lookupHead
	if head == "" {
		head = strings.Repeat("a", 40)
	}
	return delivery.GitOpsPullRequest{Number: 81, URL: "https://fixture.invalid/pulls/81", HeadSHA: head, State: "open"}, nil
}
func (p *devActivationProvider) ObserveGitOpsPR(_ context.Context, _ delivery.Connection, _ delivery.GitOpsApply, pr delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	p.observations++
	pr.Merged = p.merged
	if p.closed || p.merged {
		pr.State = "closed"
	}
	return pr, nil
}
func devActivationFixture(t *testing.T, kind string) (*nativeFixture, *devActivationProvider, domain.ExternalOperation, domain.Command) {
	t.Helper()
	f, _, reserve := devReservationFixture(t, kind)
	p := &devActivationProvider{mainCandidateProvider: f.s.provider.(*mainCandidateProvider)}
	f.s.provider = p
	v, err := f.s.Execute(context.Background(), reserve)
	if err != nil {
		t.Fatal(err)
	}
	hold := v.(domain.ExternalOperation)
	prepareActivationFixtureBaseline(t, f, p, hold)
	return f, p, hold, domain.Command{Action: "deploy_candidate_dev", Actor: hold.RequestedBy, ProductID: hold.ProductID, Data: map[string]any{"reservation_operation_id": hold.ID, "request_key": "fixture-activate-main-A", "expected_selection": hold.DevReservation.ReservedSelection}}
}
func activationTicks(t *testing.T, f *nativeFixture, n int) {
	t.Helper()
	for range n {
		nativeTick(t, f)
	}
}

func TestDevActivationExactArtifactsPreservedBaselineAndOneShotRecovery(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			before, _ := f.store.Read(ctx)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			st, _ := f.store.Read(ctx)
			if op.Kind != devActivationKind || len(op.ChildIDs) != 1 || op.DevActivation == nil {
				t.Fatal("missing exact activation")
			}
			child := operationByID(&st, op.ChildIDs[0])
			if child.Native != nil || child.BuildOnly || child.Phase != "INSPECT" || child.ExistingArtifact == nil || child.Snapshot.Target.Purpose != "DEV" || child.Snapshot.Build.Profile != child.ExistingArtifact.Native.Profile {
				t.Fatal("activation forged build or lost original profile")
			}
			if !reflect.DeepEqual(before.Environments, st.Environments) || !reflect.DeepEqual(before.ComponentBuilds, st.ComponentBuilds) || !reflect.DeepEqual(before.DeliveryArtifacts, st.DeliveryArtifacts) || !reflect.DeepEqual(before.WorkerJobs, st.WorkerJobs) {
				t.Fatal("activation mutated baseline/build/artifact")
			}
			candidateBefore := *operationByID(&before, hold.DevReservation.CandidateOperationID)
			// The original feature branch is absent and current main may advance.
			newer := strings.Repeat("e", 40)
			p.main = newer
			p.parents[newer] = []string{shaHead}
			p.lost = true
			p.absent = true
			activationTicks(t, f, 10)
			st, _ = f.store.Read(ctx)
			child = operationByID(&st, op.ChildIDs[0])
			if p.proposals != 1 || p.lookups == 0 || child.Phase != "APPLY_UNCERTAIN" || child.ReservationMutationIntent != "GITOPS" {
				t.Fatalf("unknown outcome repeated or lost: proposals=%d lookups=%d phase=%s detail=%s", p.proposals, p.lookups, child.Phase, child.Detail)
			}
			// Simulate process/store restart. Only exact existing PR lookup may recover.
			f.store.Close()
			f.store = f.reopen()
			f.s.store = f.store
			p.absent = false
			activationTicks(t, f, 8)
			st, _ = f.store.Read(ctx)
			child = operationByID(&st, op.ChildIDs[0])
			if p.proposals != 1 || child.GitOpsPR == nil || child.Phase != "WAIT_GITOPS_PR" {
				t.Fatal("GET-only PR recovery failed", child.Detail)
			}
			for name, mutate := range map[string]func(*domain.ExternalOperation){
				"durable-base":      func(op *domain.ExternalOperation) { op.GitOpsBase.HeadSHA = "changed" },
				"frozen-pr":         func(op *domain.ExternalOperation) { op.GitOpsPR.HeadSHA = strings.Repeat("b", 40) },
				"observed-artifact": func(op *domain.ExternalOperation) { op.Artifact.Digest = "sha256:" + strings.Repeat("f", 64) },
			} {
				t.Run(name, func(t *testing.T) {
					copy := cloneStateForActivation(t, st)
					mutate(operationByID(&copy, child.ID))
					if f.s.devActivationAllowed(&copy, *operationByID(&copy, child.ID)) == nil {
						t.Fatal("frozen intent/PR/artifact mutation admitted")
					}
				})
			}
			if _, err = f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("activation allowed blind release")
			}
			candidateAfter := *operationByID(&st, candidateBefore.ID)
			candidateAfter.NextAttemptAt = candidateBefore.NextAttemptAt // nativeTick schedules every fixture operation, including terminal history.
			if !reflect.DeepEqual(candidateBefore, candidateAfter) || !reflect.DeepEqual(before.WorkerJobs, st.WorkerJobs) || !reflect.DeepEqual(before.DeliveryArtifacts, st.DeliveryArtifacts) {
				t.Fatal("original build/candidate/provenance changed")
			}
			if v, err = f.s.Execute(ctx, c); err != nil || v.(domain.ExternalOperation).ID != op.ID {
				t.Fatal("historical replay", err)
			}
			q := ReservationState(&st, "dev")
			if q["candidate_deployment_id"] != op.ID || q["candidate_deployed"] != false || q["release_action"] != "verified_restoration_required" {
				t.Fatal("query conflates intent/runtime", q)
			}
			p.closed = true
			activationTicks(t, f, 8)
			st, _ = f.store.Read(ctx)
			if _, err = f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("closed activation allowed blind release")
			}
		})
	}
}

func TestDevActivationAdmissionAndCapabilityGuards(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			before, _ := f.store.Read(ctx)
			for name, mut := range map[string]func(*domain.Command){
				"owner": func(c *domain.Command) { c.Actor = "other" }, "product": func(c *domain.Command) { c.ProductID = "foreign" }, "artifact": func(c *domain.Command) { c.Data["artifact_id"] = "fake" }, "case": func(c *domain.Command) { c.Data["Request_key"] = "fake" }, "null-selection": func(c *domain.Command) { c.Data["expected_selection"] = nil }, "CAS": func(c *domain.Command) { c.Data["expected_selection"] = domain.EnvironmentSelection{} },
			} {
				t.Run(name, func(t *testing.T) {
					copy := c
					copy.Data = map[string]any{}
					for k, v := range c.Data {
						copy.Data[k] = v
					}
					mut(&copy)
					if _, err := f.s.Execute(ctx, copy); err == nil {
						t.Fatal("invalid activation admitted")
					}
				})
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			ids := []string{}
			errs := []error{}
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, e := f.s.Execute(ctx, c)
					mu.Lock()
					defer mu.Unlock()
					if e != nil {
						errs = append(errs, e)
					} else {
						ids = append(ids, v.(domain.ExternalOperation).ID)
					}
				}()
			}
			wg.Wait()
			if len(errs) > 0 || len(ids) != 4 {
				t.Fatal("concurrent identical receipt", errs)
			}
			for _, id := range ids {
				if id != ids[0] {
					t.Fatal("duplicate activation")
				}
			}
			st, _ := f.store.Read(ctx)
			parent := operationByID(&st, ids[0])
			child := operationByID(&st, parent.ChildIDs[0])
			if err := f.s.devActivationAllowed(&st, *child); err != nil {
				t.Fatal(err)
			}
			for name, mut := range map[string]func(*domain.State){
				"childlink":  func(s *domain.State) { operationByID(s, child.ID).ParentID = "fake" },
				"proof":      func(s *domain.State) { operationByID(s, parent.ID).DevActivation.CandidateHash = "fake" },
				"target":     func(s *domain.State) { environmentBinding(s, "dev", "component").Ref = "other" },
				"epoch":      func(s *domain.State) { environmentByID(s, "dev").ReservationEpoch++ },
				"owner":      func(s *domain.State) { operationByID(s, hold.ID).RequestedBy = "other" },
				"quarantine": func(s *domain.State) { operationByID(s, hold.ID).ReservationQuarantined = true },
				"buildonly":  func(s *domain.State) { operationByID(s, child.ID).BuildOnly = true },
				"native":     func(s *domain.State) { operationByID(s, child.ID).Native = &domain.NativeDelivery{} },
				"artifact": func(s *domain.State) {
					operationByID(s, child.ID).Artifact = &delivery.ArtifactResult{Available: true, Digest: "sha256:" + strings.Repeat("f", 64), ObservedAt: time.Now()}
				},
			} {
				t.Run(name, func(t *testing.T) {
					copy := cloneStateForActivation(t, st)
					mut(&copy)
					current := operationByID(&copy, child.ID)
					if f.s.devActivationAllowed(&copy, *current) == nil {
						t.Fatal("tampered capability admitted")
					}
				})
			}
			leased := *child
			leased.Artifact = &delivery.ArtifactResult{Available: true, Digest: "sha256:" + strings.Repeat("f", 64), ObservedAt: time.Now()}
			if f.s.devActivationAllowed(&st, leased) == nil {
				t.Fatal("forged leased artifact accepted")
			}
			// A late causal FAIL fences the admitted activation even with old timestamps.
			if err := f.store.Update(ctx, func(s *domain.State) error {
				s.Results = append(s.Results, domain.CheckResult{Meta: domain.Meta{ID: "late-failure", ProductID: "p", FeatureID: "f", CreatedAt: time.Unix(1, 0)}, CheckID: "source-check", Commit: shaHead, Result: "failed"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			activationTicks(t, f, 5)
			st, _ = f.store.Read(ctx)
			if p.proposals != 0 {
				t.Fatal("late causal FAIL reached proposal")
			}
			if _, err := f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("failed activation released")
			}
			if !reflect.DeepEqual(before.Environments, st.Environments) {
				t.Fatal("guard changed baseline")
			}
		})
	}
}
func cloneStateForActivation(t *testing.T, st domain.State) domain.State {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var out domain.State
	if err = json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDevActivationInflightInvalidationRetainsUncertainHold(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			// The hook runs inside the external provider call, after intent was
			// committed. Its result cannot be admitted after current authority changes.
			p.afterPropose = func() {
				if err := f.store.Update(ctx, func(st *domain.State) error {
					environmentBinding(st, "dev", "component").AllowDeploy = false
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			var lateErr error
			for range 12 {
				if err := f.store.Update(ctx, func(st *domain.State) error {
					for i := range st.Operations {
						st.Operations[i].NextAttemptAt = time.Now().Add(time.Hour)
						if st.Operations[i].ID == op.ChildIDs[0] {
							st.Operations[i].NextAttemptAt = time.Time{}
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				_, lateErr = f.s.Tick(ctx)
				if lateErr != nil {
					break
				}
			}
			if lateErr == nil || p.proposals != 1 {
				t.Fatal("late provider effect was accepted", lateErr, p.proposals)
			}
			st, _ := f.store.Read(ctx)
			child := operationByID(&st, op.ChildIDs[0])
			if child.Phase != "APPLY_UNCERTAIN" || child.ReservationMutationIntent != "GITOPS" || child.GitOpsPR != nil || child.GitOpsResult != nil || activeDevReservation(&st, "dev") == nil {
				t.Fatal("inflight invalidation lost uncertain effect/hold")
			}
			if _, err := f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("inflight effect blind release")
			}
			// Lease expiry is not evidence that the HTTP mutation did not happen.
			if err := f.store.Update(ctx, func(st *domain.State) error {
				for i := range st.Operations {
					st.Operations[i].LeaseUntil = time.Time{}
					st.Operations[i].NextAttemptAt = time.Time{}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			activationTicks(t, f, 5)
			if p.proposals != 1 {
				t.Fatal("inflight invalidation repeated proposal")
			}
		})
	}
}

func TestDevActivationMergedDesiredStateAndRestoreQuarantine(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			activationTicks(t, f, 8)
			p.merged = true
			activationTicks(t, f, 8)
			st, _ := f.store.Read(ctx)
			parent := operationByID(&st, op.ID)
			if parent.Status != "SUCCEEDED" || parent.DeploymentState != "GITOPS_APPLIED" || p.proposals != 1 || len(st.RuntimeObservations) != 0 {
				t.Fatal("desired GitOps completion invented runtime", parent.Status, parent.DeploymentState, parent.Detail)
			}
			if _, err := f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("merged activation released")
			}
			archive, err := f.s.CreateBackup(ctx, "operator")
			if err != nil {
				t.Fatal(err)
			}
			destStore, _ := openWorkerTestStore(t, kind)
			defer destStore.Close()
			dest := NewWithProvider(destStore, p)
			dest.ConfigureWorkers(f.s.externalWorkers)
			if _, err = dest.RestoreBackup(ctx, "restore-owner", archive.ArchiveBase64, archive.SHA256); err != nil {
				t.Fatal(err)
			}
			restored, _ := destStore.Read(ctx)
			if !operationByID(&restored, op.ID).ReservationQuarantined || !operationByID(&restored, hold.ID).ReservationQuarantined || activeDevReservation(&restored, "dev") == nil {
				t.Fatal("activation restore lost quarantine")
			}
			if v, err = dest.Execute(ctx, c); err != nil || v.(domain.ExternalOperation).ID != op.ID {
				t.Fatal("restore historical replay", err)
			}
			if _, err = dest.Execute(ctx, releaseReservationCommand(restored, hold)); err == nil {
				t.Fatal("restored activation release")
			}
			if f.s.devActivationAllowed(&restored, *operationByID(&restored, op.ID)) == nil {
				t.Fatal("restored activation minted rights")
			}
		})
	}
}

// Both native components are configured, built, published and admitted through
// supported commands. No candidate, artifact or source-proof row is forged.
func devActivationMultiFixture(t *testing.T, kind string, setup ...func(*nativeFixture)) (*nativeFixture, *devActivationProvider, domain.ExternalOperation, domain.Command) {
	t.Helper()
	ctx := context.Background()
	f, source, c := mainCandidateFixture(t, kind)
	b, _ := f.s.externalWorkers.Recipe("build")
	second := f.recipe
	second.ID = "publish-b"
	second.ComponentID = "component-b"
	second.Repository += "-b"
	second.Hash = workerconfig.PublicationHash(second)
	cfg := workerconfig.Config{Revision: "cfg1", LeaseSeconds: 5, Recipes: []workerconfig.Recipe{b}, Publications: []workerconfig.PublicationRecipe{f.recipe, second}, Credentials: []workerconfig.Credential{
		{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)},
		{ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)},
	}}
	a, err := workerconfig.New(cfg, "native-control-fixture", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	f.s.ConfigureWorkers(a)
	f.p, _ = a.Authenticate(nativeToken, time.Now())
	exec(t, f.s, domain.Command{Action: "create_application", ID: "component-b", ProductID: "p", Data: map[string]any{"name": "Component B", "repository_id": "source"}})
	exec(t, f.s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component-b", "connection_id": "conn", "mode": "NATIVE", "profile": second.Profile, "image_repository": nativeImage(second), "native": domain.NativeBinding{BuildRecipeID: b.ID, PublicationRecipeID: second.ID, Parameters: map[string]any{}}}})
	for _, env := range []string{"dev", "prod"} {
		purpose := "DEV"
		if env == "prod" {
			purpose = "PROD"
		}
		exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": env, "application_id": "component-b", "purpose": purpose, "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "component-b.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
	}
	for _, configure := range setup {
		configure(f)
	}
	var request domain.MainCandidateRequest
	raw, _ := json.Marshal(c.Data)
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	selected := request.MainSources[0]
	selected.ApplicationID = "component-b"
	selected.Native.PublicationRecipeID = second.ID
	request.MainSources = append(request.MainSources, selected)
	raw, _ = json.Marshal(request)
	if err = json.Unmarshal(raw, &c.Data); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Execute(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	candidate := v.(domain.ExternalOperation)
	activationTicks(t, f, 8)
	automaticBuild(t, f)
	automaticBuild(t, f)
	activationTicks(t, f, 10)
	for range 2 {
		lease, err := f.s.ClaimWorker(ctx, f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish"})
		if err != nil || lease == nil {
			t.Fatal("multi publisher claim", err)
		}
		if _, err = f.s.StartWorker(ctx, f.p, workerRequest(lease)); err != nil {
			t.Fatal(err)
		}
		pub := lease.Job.Inputs.Publication
		f.publisher.AllowedOrigin = pub.Origin
		f.publisher.AllowedRepository = pub.Repository
		f.publisher.Plan = ocipublisher.Plan{ID: lease.Job.ID, Approved: true, ProductID: "p", ComponentID: pub.ComponentID, RepositoryID: "source", Origin: pub.Origin, Repository: pub.Repository, Receipt: pub.Receipt}
		if _, err = ocipublisher.Publish(ctx, f.publisher); err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.ReportWorker(ctx, f.p, domain.WorkerReport{WorkerAttemptRequest: workerRequest(lease), Result: "PASSED", Publication: &domain.WorkerPublicationReport{ManifestDigest: pub.Receipt.ManifestDigest}}); err != nil {
			t.Fatal(err)
		}
	}
	activationTicks(t, f, 16)
	st, _ := f.store.Read(ctx)
	candidate = *operationByID(&st, candidate.ID)
	if candidate.Status != "SUCCEEDED" || len(candidate.ChildIDs) != 2 {
		t.Fatal("multi candidate completion", candidate.Status, candidate.Detail)
	}
	exec(t, f.s, domain.Command{Action: "plan_composition", ID: "dev-multi-baseline", ProductID: "p", Data: map[string]any{"name": "shared baseline", "environment_id": "dev", "components": []domain.CompositionComponent{component("component", "rev"), component("component-b", "rev")}}})
	exec(t, f.s, domain.Command{Action: "select_composition", ID: "dev-multi-baseline"})
	st, _ = f.store.Read(ctx)
	v, err = f.s.Execute(ctx, domain.Command{Action: "reserve_candidate_dev", Actor: "operator", ProductID: "p", Data: map[string]any{"candidate_operation_id": candidate.ID, "environment_id": "dev", "request_key": "fixture-reserve-multi-A", "expected_selection": selectionToken(*environmentByID(&st, "dev"))}})
	if err != nil {
		t.Fatal(err)
	}
	hold := v.(domain.ExternalOperation)
	p := &devActivationProvider{mainCandidateProvider: source}
	f.s.provider = p
	prepareActivationFixtureBaseline(t, f, p, hold)
	return f, p, hold, domain.Command{Action: "deploy_candidate_dev", Actor: "operator", ProductID: "p", Data: map[string]any{"reservation_operation_id": hold.ID, "request_key": "fixture-activate-multi-A", "expected_selection": hold.DevReservation.ReservedSelection}}
}

func TestDevActivationMultiComponentPartialOutcomeRetainsHold(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationMultiFixture(t, kind, func(f *nativeFixture) {
				exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component-b", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "another-dev", "path": "component-b.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
			})
			before, _ := f.store.Read(ctx)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			p.failPath = "component-b.yaml"
			p.absent = true
			activationTicks(t, f, 20)
			st, _ := f.store.Read(ctx)
			var known, uncertain int
			for _, cid := range op.ChildIDs {
				child := operationByID(&st, cid)
				if child.GitOpsPR != nil && child.Phase == "WAIT_GITOPS_PR" {
					known++
				}
				if child.Phase == "APPLY_UNCERTAIN" && child.ReservationMutationIntent == "GITOPS" {
					uncertain++
				}
			}
			if p.proposals != 2 || known != 1 || uncertain != 1 || operationByID(&st, op.ID).Status == "SUCCEEDED" {
				t.Fatal("partial activation conflated outcomes", p.proposals, known, uncertain)
			}
			if !reflect.DeepEqual(before.Environments, st.Environments) || !reflect.DeepEqual(before.DeliveryArtifacts, st.DeliveryArtifacts) || !reflect.DeepEqual(before.WorkerJobs, st.WorkerJobs) {
				t.Fatal("partial activation mutated baseline/builds")
			}
			if _, err = f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("partial activation released")
			}
			activationTicks(t, f, 8)
			if p.proposals != 2 {
				t.Fatal("partial outcome repeated write")
			}
		})
	}
}

func TestDevActivationMalformedUncertainEvidenceFailsClosed(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			parent := v.(domain.ExternalOperation)
			if err = f.store.Update(ctx, func(st *domain.State) error {
				for i := range st.Operations {
					st.Operations[i].NextAttemptAt = time.Now().Add(time.Hour)
				}
				child := operationByID(st, parent.ChildIDs[0])
				child.Phase = "APPLY_UNCERTAIN"
				child.ReservationMutationIntent = "GITOPS"
				child.GitOpsBase = nil
				child.Artifact = nil
				child.NextAttemptAt = time.Time{}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if worked, err := f.s.Tick(ctx); err != nil || !worked {
				t.Fatal("malformed unknown effect failed to stop safely", worked, err)
			}
			st, _ := f.store.Read(ctx)
			child := operationByID(&st, parent.ChildIDs[0])
			if child.Status != "FAILED" || child.DeploymentState != "BLOCKED" || child.ReservationMutationIntent != "GITOPS" || child.DevActivationIntentHash != "" || child.GitOpsPR != nil || p.proposals != 0 || activeDevReservation(&st, "dev") == nil {
				t.Fatal("malformed uncertain record minted rights or lost hold")
			}
			if _, err = f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("malformed uncertain effect released")
			}
		})
	}
}

func forceActivationTick(t *testing.T, f *nativeFixture, id string) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Update(ctx, func(st *domain.State) error {
		for i := range st.Operations {
			st.Operations[i].NextAttemptAt = time.Now().Add(time.Hour)
		}
		operationByID(st, id).NextAttemptAt = time.Time{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if worked, err := f.s.Tick(ctx); err != nil || !worked {
		t.Fatal("isolated activation tick", worked, err)
	}
}

func TestDevActivationFirstProposalHeadRequiresCompleteLookupAgreement(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			p.lookupHead = strings.Repeat("b", 40)
			forceActivationTick(t, f, op.ChildIDs[0])
			forceActivationTick(t, f, op.ChildIDs[0])
			st, _ := f.store.Read(ctx)
			child := operationByID(&st, op.ChildIDs[0])
			if p.proposals != 1 || p.lookups != 1 || child.GitOpsPR != nil || child.Phase != "APPLY_UNCERTAIN" || child.ReservationMutationIntent != "GITOPS" || child.DevActivationPRHash != "" {
				t.Fatal("first unknown/moved PR head admitted", p.proposals, p.lookups, child.Phase, child.Detail)
			}
			if _, err = f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("unverified first proposal released")
			}
		})
	}
}

func TestDevActivationAlteredCompletedChildResultBlocksAggregate(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			forceActivationTick(t, f, op.ChildIDs[0])
			forceActivationTick(t, f, op.ChildIDs[0])
			p.merged = true
			forceActivationTick(t, f, op.ChildIDs[0])
			st, _ := f.store.Read(ctx)
			child := operationByID(&st, op.ChildIDs[0])
			if child.Status != "SUCCEEDED" || child.GitOpsResult == nil || child.DevActivationResultHash == "" {
				t.Fatal("exact provider result never admitted", child.Status, child.Detail)
			}
			if err = f.store.Update(ctx, func(st *domain.State) error {
				child := operationByID(st, op.ChildIDs[0])
				child.GitOpsResult.CommitSHA = "unrelated-commit"
				child.GitOpsResult.BlobSHA = "unrelated-blob"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			forceActivationTick(t, f, op.ID)
			st, _ = f.store.Read(ctx)
			parent := operationByID(&st, op.ID)
			if parent.Status != "FAILED" || parent.DeploymentState != "BLOCKED" || p.proposals != 1 {
				t.Fatal("altered completed-child result accepted", parent.Status, parent.Detail)
			}
			if _, err = f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("tampered desired-state effect released")
			}
		})
	}
}

func TestDevActivationRevalidatesBetweenPhysicalWrites(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			p.afterPhysicalWrite = func(count int) {
				if count == 1 {
					if err := f.store.Update(ctx, func(st *domain.State) error {
						environmentBinding(st, "dev", "component").AllowDeploy = false
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			forceActivationTick(t, f, op.ChildIDs[0])
			if err = f.store.Update(ctx, func(st *domain.State) error {
				for i := range st.Operations {
					st.Operations[i].NextAttemptAt = time.Now().Add(time.Hour)
				}
				operationByID(st, op.ChildIDs[0]).NextAttemptAt = time.Time{}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.Tick(ctx); err == nil {
				t.Fatal("between-write revocation accepted result")
			}
			st, _ := f.store.Read(ctx)
			child := operationByID(&st, op.ChildIDs[0])
			if p.physicalWrites != 1 || p.proposals != 0 || child.Phase != "APPLY_UNCERTAIN" || child.ReservationMutationIntent != "GITOPS" || child.GitOpsPR != nil || child.GitOpsResult != nil || activeDevReservation(&st, "dev") == nil {
				t.Fatal("revocation allowed subsequent mutation or lost uncertain hold", p.physicalWrites, p.proposals)
			}
			if _, err = f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("partial physical write blindly released")
			}
		})
	}
}

type devActivationUnguardedProvider struct {
	baselineReader *devActivationProvider
	*mainCandidateProvider
	proposals int
}

func (p *devActivationUnguardedProvider) ProposeGitOps(context.Context, delivery.Connection, delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	p.proposals++
	return delivery.GitOpsPullRequest{}, errors.New("unguarded legacy mutation")
}

func TestDevActivationNeverFallsBackToUnguardedProposer(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, c := devActivationFixture(t, kind)
			legacy := &devActivationUnguardedProvider{mainCandidateProvider: p.mainCandidateProvider, baselineReader: p}
			f.s.provider = legacy
			v, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			op := v.(domain.ExternalOperation)
			forceActivationTick(t, f, op.ChildIDs[0])
			forceActivationTick(t, f, op.ChildIDs[0])
			st, _ := f.store.Read(ctx)
			child := operationByID(&st, op.ChildIDs[0])
			if legacy.proposals != 0 || child.Status != "FAILED" || child.DevActivationBatch != nil || child.ReservationMutationIntent != "" || child.GitOpsPR != nil {
				t.Fatal("activation fell back to legacy unguarded mutation")
			}
			if _, err = f.s.Execute(ctx, releaseReservationCommand(st, hold)); err == nil {
				t.Fatal("unsupported activation blind release")
			}
		})
	}
}
