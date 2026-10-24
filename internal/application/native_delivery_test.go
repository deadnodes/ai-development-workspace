package application

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"releasecontrol/internal/ociverify"
	"releasecontrol/internal/workerconfig"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
)

func automaticFixture(t *testing.T, kind string) *nativeFixture {
	t.Helper()
	f := newNativeFixture(t, kind)
	f.s.provider = &fakeDelivery{}
	seed, e := f.store.Read(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	seed.WorkerJobs = nil
	seed.WorkerAttempts = nil
	seed.Operations = nil
	environmentBinding(&seed, "dev", "component").GitOpsMode = "PR"
	connection(&seed, "conn").ProductID = ""
	seed.ConnectionGrants = append(seed.ConnectionGrants, domain.ConnectionGrant{Meta: domain.Meta{ID: "native-fixture-grant", ProductID: "p"}, ConnectionID: "conn"})
	f.store.Close()
	f.store, f.reopen = openWorkerTestStore(t, kind)
	f.s.store = f.store
	if e = f.store.Update(context.Background(), func(st *domain.State) error { *st = seed; return nil }); e != nil {
		t.Fatal(e)
	}

	exec(t, f.s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE", "profile": f.recipe.Profile, "image_repository": nativeImage(f.recipe), "native": map[string]any{"build_recipe_id": "build", "publication_recipe_id": "publish", "parameters": map[string]any{}}}})
	return f
}
func automaticCommand() domain.Command {
	return domain.Command{Action: "deploy_integration", Actor: "operator", IntegrationID: "i", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": "rev", "request_key": "automatic-native-fixture"}}
}
func nativeTick(t *testing.T, f *nativeFixture) {
	t.Helper()
	if e := f.store.Update(context.Background(), func(st *domain.State) error {
		for i := range st.Operations {
			st.Operations[i].NextAttemptAt = st.Operations[i].UpdatedAt
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func automaticQueue(t *testing.T, f *nativeFixture) domain.ExternalOperation {
	t.Helper()
	v, e := f.s.Execute(context.Background(), automaticCommand())
	if e != nil {
		t.Fatal(e)
	}
	for range 3 {
		nativeTick(t, f)
	}
	return v.(domain.ExternalOperation)
}
func automaticBuild(t *testing.T, f *nativeFixture) domain.WorkerLease {
	t.Helper()
	ctx := context.Background()
	p, _ := f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
	l, e := f.s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
	if e != nil || l == nil {
		t.Fatal("automatic claim", e)
	}
	req := workerRequest(l)
	if _, e = f.s.StartWorker(ctx, p, req); e != nil {
		t.Fatal(e)
	}
	// The archive fixture represents one independently verified supervisor result.
	archive, e := os.ReadFile(f.publisher.Archive)
	if e != nil {
		t.Fatal(e)
	}
	receipt, e := ociverify.VerifyOCIBytes(archive, 1<<20, "linux/arm64")
	if e != nil {
		t.Fatal(e)
	}
	receipt.JobID = l.Job.ID
	receipt.AttemptID = l.Attempt.ID
	receipt.InputHash = l.Job.InputHash
	receipt.SourceSHA = l.Job.Inputs.SourceSHA
	receipt.RecipeHash = l.Job.Inputs.RecipeHash
	receipt.ConfigRevision = l.Job.Inputs.ConfigRevision
	receipt.ToolchainImage = l.Job.Inputs.Execution.ToolchainImage
	out, e := f.s.ReportWorker(ctx, p, domain.WorkerReport{WorkerAttemptRequest: req, Result: "PASSED", LocalOCI: &receipt})
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func TestAutomaticNativeRequestConcurrencyReopenAndCancellation(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f := automaticFixture(t, kind)
			ctx := context.Background()
			var wg sync.WaitGroup
			var mu sync.Mutex
			ids := map[string]bool{}
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, e := f.s.Execute(ctx, automaticCommand())
					if e != nil {
						t.Error(e)
						return
					}
					mu.Lock()
					ids[v.(domain.ExternalOperation).ID] = true
					mu.Unlock()
				}()
			}
			wg.Wait()
			if len(ids) != 1 {
				t.Fatal("request duplicated", ids)
			}
			conflicting := automaticCommand()
			conflicting.Data["expected_digest"] = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
			if _, e := f.s.Execute(ctx, conflicting); e == nil {
				t.Fatal("conflicting intent accepted")
			}
			for range 2 {
				nativeTick(t, f)
			}
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, e := f.s.Tick(ctx); e != nil {
						t.Error(e)
					}
				}()
			}
			wg.Wait()
			st, _ := f.store.Read(ctx)
			if len(st.WorkerJobs) != 1 {
				t.Fatal("concurrent tick duplicated build", st.WorkerJobs)
			}
			original := st.WorkerJobs[0]
			parent := st.Operations[0]
			f.store.Close()
			f.store = f.reopen()
			f.s.store = f.store
			nativeTick(t, f)
			st, _ = f.store.Read(ctx)
			if len(st.WorkerJobs) != 1 || !reflect.DeepEqual(original, st.WorkerJobs[0]) {
				t.Fatal("restart changed queued job")
			}
			p, _ := f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
			l, e := f.s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
			if e != nil || l == nil {
				t.Fatal(e)
			}
			if _, e = f.s.StartWorker(ctx, p, workerRequest(l)); e != nil {
				t.Fatal(e)
			}
			if _, e = f.s.Execute(ctx, domain.Command{Action: "cancel_native_delivery", Actor: "operator", ProductID: "p", ID: parent.ID, Data: map[string]any{}}); e != nil {
				t.Fatal(e)
			}
			if _, e = f.s.HeartbeatWorker(ctx, p, workerRequest(l)); e == nil {
				t.Fatal("cancelled build heartbeat accepted")
			}
			nativeTick(t, f)
			st, _ = f.store.Read(ctx)
			if len(st.WorkerJobs) != 1 || st.WorkerJobs[0].Status != "RECONCILIATION_REQUIRED" || operationByID(&st, parent.ID).Status != "CANCELLED" {
				t.Fatal("cancel fabricated cleanup or downstream job")
			}
		})
	}
}
func TestAutomaticNativeLiveFences(t *testing.T) {
	for _, stage := range []string{"queued", "running", "publication", "admission"} {
		for _, change := range []string{"config", "grant", "quarantine", "cancel", "deadline", "parent"} {
			t.Run(stage+"/"+change, func(t *testing.T) {
				f := automaticFixture(t, "local")
				ctx := context.Background()
				parent := automaticQueue(t, f)
				var lease *domain.WorkerLease
				var principal = f.p
				if stage == "running" {
					principal, _ = f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
					lease, _ = f.s.ClaimWorker(ctx, principal, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
					if _, e := f.s.StartWorker(ctx, principal, workerRequest(lease)); e != nil {
						t.Fatal(e)
					}
				}
				if stage == "publication" || stage == "admission" {
					automaticBuild(t, f)
					nativeTick(t, f)
					lease, _ = f.s.ClaimWorker(ctx, f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish"})
					if lease == nil {
						t.Fatal("publication missing")
					}
					if _, e := f.s.StartWorker(ctx, f.p, workerRequest(lease)); e != nil {
						t.Fatal(e)
					}
				}
				mutate := func(st *domain.State) {
					switch change {
					case "config":
						componentBuild(st, "component").Profile = "changed"
					case "grant":
						for i := range st.ConnectionGrants {
							st.ConnectionGrants[i].ProductID = "other"
						}
					case "quarantine":
						st.NativeQuarantines = append(st.NativeQuarantines, domain.NativeQuarantine{Meta: domain.Meta{ID: "native-quarantine", ProductID: "p"}})
					case "cancel":
						operationByID(st, parent.ID).Status = "CANCELLED"
					case "deadline":
						operationByID(st, parent.ID).Native.Deadline = time.Now().Add(-time.Minute)
					case "parent":
						operationByID(st, parent.ID).ParentID = "missing"
					}
				}
				if stage == "admission" {
					report := domain.WorkerReport{WorkerAttemptRequest: workerRequest(lease), Result: "PASSED", Publication: &domain.WorkerPublicationReport{ManifestDigest: lease.Job.Inputs.Publication.Receipt.ManifestDigest}}
					p := *lease.Job.Inputs.Publication
					f.publisher.Plan = ocipublisher.Plan{ID: lease.Job.ID, Approved: true, ProductID: "p", ComponentID: p.ComponentID, RepositoryID: "source", Origin: p.Origin, Repository: p.Repository, Receipt: p.Receipt}
					// GET triggers a concurrent local mutation after the optimistic eligibility read.
					if _, e := ocipublisher.Publish(ctx, f.publisher); e != nil {
						t.Fatal(e)
					}
					f.registry.hook = func() { f.store.Update(ctx, func(st *domain.State) error { mutate(st); return nil }) }
					if _, e := f.s.ReportWorker(ctx, f.p, report); e == nil {
						t.Fatal("racing admission accepted")
					}
				} else {
					f.store.Update(ctx, func(st *domain.State) error { mutate(st); return nil })
					if stage == "queued" {
						bp, _ := f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
						l, e := f.s.ClaimWorker(ctx, bp, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
						if e == nil && l != nil {
							t.Fatal("stale claim")
						}
					} else if _, e := f.s.HeartbeatWorker(ctx, principal, workerRequest(lease)); e == nil {
						t.Fatal("stale active heartbeat")
					}
				}
				st, _ := f.store.Read(ctx)
				if len(st.DeliveryArtifacts) != 0 {
					t.Fatal("stale admission persisted")
				}
			})
		}
	}
}

var _ delivery.Provider = (*fakeDelivery)(nil)

func TestAutomaticNativeUnknownBuildAndExpiredPublication(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := automaticFixture(t, kind)
			automaticQueue(t, f)
			automaticBuild(t, f)
			nativeTick(t, f)
			lease, e := f.s.ClaimWorker(ctx, f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish"})
			if e != nil || lease == nil {
				t.Fatal(e)
			}
			if _, e = f.s.StartWorker(ctx, f.p, workerRequest(lease)); e != nil {
				t.Fatal(e)
			}
			p := *lease.Job.Inputs.Publication
			f.publisher.Plan = ocipublisher.Plan{ID: lease.Job.ID, Approved: true, ProductID: "p", ComponentID: p.ComponentID, RepositoryID: "source", Origin: p.Origin, Repository: p.Repository, Receipt: p.Receipt}
			if _, e = ocipublisher.Publish(ctx, f.publisher); e != nil {
				t.Fatal(e)
			}
			if e = f.store.Update(ctx, func(st *domain.State) error {
				workerAttempt(st, lease.Attempt.ID).LeaseUntil = time.Now().Add(-time.Minute)
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			for range 3 {
				nativeTick(t, f)
			}
			st, _ := f.store.Read(ctx)
			if len(st.WorkerJobs) != 2 || workerJob(&st, lease.Job.ID).Status != "RECONCILIATION_REQUIRED" {
				t.Fatal("expired publication rebuilt or replaced")
			}
			reconcile := domain.Command{Action: "reconcile_worker_publication", Actor: "operator", ProductID: "p", ID: lease.Job.ID, Data: map[string]any{}}
			for range 2 {
				if _, e = f.s.Execute(ctx, reconcile); e != nil {
					t.Fatal(e)
				}
			}
			nativeTick(t, f)
			st, _ = f.store.Read(ctx)
			a := workerAttempt(&st, lease.Attempt.ID)
			parent := operationByID(&st, lease.Job.Inputs.SourceProof.DeliveryID)
			if parent.Phase != "INSPECT" || parent.ExistingArtifact == nil || parent.Native.PublicationJobID != lease.Job.ID || parent.Native.BuildJobID != lease.Job.Inputs.Publication.Receipt.JobID {
				t.Fatal("reconciled job did not resume same parent and links")
			}

			if len(st.DeliveryArtifacts) != 1 || a.Status != "EXPIRED" || a.Report != nil || st.DeliveryArtifacts[0].Native.Reconciliation == nil || len(st.WorkerJobs) != 2 {
				t.Fatal("reconciliation forged report or duplicate")
			}
		})
	}
	t.Run("unknown-build-never-requeued", func(t *testing.T) {
		f := automaticFixture(t, "local")
		ctx := context.Background()
		automaticQueue(t, f)
		p, _ := f.s.externalWorkers.Authenticate(nativeBuildToken, time.Now())
		l, e := f.s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
		if e != nil || l == nil {
			t.Fatal(e)
		}
		if _, e = f.s.StartWorker(ctx, p, workerRequest(l)); e != nil {
			t.Fatal(e)
		}
		f.store.Update(ctx, func(st *domain.State) error {
			workerAttempt(st, l.Attempt.ID).LeaseUntil = time.Now().Add(-time.Minute)
			return nil
		})
		for range 4 {
			nativeTick(t, f)
		}
		next, e := f.s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
		if e != nil || next != nil {
			t.Fatal("unknown execution replay", e)
		}
		st, _ := f.store.Read(ctx)
		if len(st.WorkerJobs) != 1 || st.WorkerJobs[0].Status != "RECONCILIATION_REQUIRED" || st.WorkerAttempts[0].Report != nil {
			t.Fatal("unknown execution became success or rebuild")
		}
	})
}
func TestNativeReleaseDeniedBeforeProtectedMutation(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, phase := range []string{"new", "COMPOSE_SOURCE", "ADVANCE_MAIN", "QUEUE_BUILDS", "promotion"} {
			t.Run(fmt.Sprintf("mixed=%v/%s", mixed, phase), func(t *testing.T) {
				s, m, provider := flowFixture(t)
				flowEnvironment(t, s, "prod", "PROD")
				exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
				exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
				components := []domain.CompositionComponent{component("component", "rev")}
				if mixed {
					exec(t, s, domain.Command{Action: "create_application", ID: "second", ProductID: "p", Data: map[string]any{"name": "second", "repository_id": "source"}})
					m.state.ComponentBuilds = append(m.state.ComponentBuilds, domain.ComponentBuild{Meta: domain.Meta{ID: "second-build", ProductID: "p"}, ApplicationID: "second", ConnectionID: "conn", Workflow: "build.yml", ImageRepository: "ghcr.io/owner/second"})
					target := *environmentBinding(&m.state, "prod", "component")
					target.ID = "second-target"
					target.ApplicationID = "second"
					m.state.EnvironmentBindings = append(m.state.EnvironmentBindings, target)
					second := component("second", "rev")
					components = append(components, second)
				}
				command := domain.Command{Action: "prepare_release_candidate", Actor: "operator", ID: "candidate", ProductID: "p", Data: map[string]any{"name": "native release rejection", "environment_id": "prod", "components": components, "approve_main_update": true}}
				if phase == "new" {
					componentBuild(&m.state, "component").Mode = "NATIVE"
					componentBuild(&m.state, "component").Native = &domain.NativeBinding{}
					if _, e := s.Execute(context.Background(), command); e == nil {
						t.Fatal("native candidate queued")
					}
				}
				if phase != "new" {
					if _, e := s.Execute(context.Background(), command); e != nil {
						t.Fatal(e)
					}
					candidate := operationByID(&m.state, "candidate")
					componentBuild(&m.state, "component").Mode = "NATIVE"
					candidate.PreparedDeployments[0].Build = *componentBuild(&m.state, "component")
					candidate.Phase = phase
					candidate.Sources[0].Result = &delivery.SourceResult{Branch: candidate.Sources[0].Plan.TargetBranch, SHA: shaHead}
					if phase == "promotion" {
						candidate.Status = "SUCCEEDED"
						candidate.DeploymentState = "READY_FOR_VERIFICATION"
						if _, e := s.Execute(context.Background(), domain.Command{Action: "promote_release_candidate", Actor: "operator", ID: candidate.ID, Data: map[string]any{"approve": true}}); e == nil {
							t.Fatal("native promotion queued")
						}
					} else {
						tickNow(t, s, m)
						if operationByID(&m.state, "candidate").Status != "FAILED" {
							t.Fatal("resumed native candidate did not fail closed")
						}
					}
				}
				if provider.advances != 0 || len(provider.plans) != 0 || len(provider.builds) != 0 || len(provider.applyCounts) != 0 {
					t.Fatal("native candidate mutated protected source or dispatched")
				}
			})
		}
	}
}

func TestNativeSelectedCompositionOriginAndPublicProofDenial(t *testing.T) {
	for _, change := range []string{"desired", "selection", "generated-source", "revision", "scope"} {
		t.Run(change, func(t *testing.T) {
			f := automaticFixture(t, "local")
			ctx := context.Background()
			provider := &fakeFlow{main: shaBase, branches: map[string]string{"feature/work": shaHead}, parents: map[string][]string{shaHead: {shaBase}}, deployments: map[string]string{}, applyCounts: map[string]int{}}
			f.s.provider = provider
			exec(t, f.s, domain.Command{Action: "plan_composition", ID: "selected-native", ProductID: "p", Data: map[string]any{"name": "selected native", "environment_id": "dev", "components": []domain.CompositionComponent{component("component", "rev")}}})
			v, e := f.s.Execute(ctx, domain.Command{Action: "reconcile_composition", Actor: "operator", ID: "selected-native", Data: map[string]any{}})
			if e != nil {
				t.Fatal(e)
			}
			parent := v.(domain.ExternalOperation)
			var st domain.State
			for range 20 {
				nativeTick(t, f)
				st, _ = f.store.Read(ctx)
				if len(st.WorkerJobs) > 0 {
					break
				}
			}
			if len(st.WorkerJobs) != 1 || st.WorkerJobs[0].IntegrationID != "" || st.WorkerJobs[0].Inputs.SourceProof == nil || st.WorkerJobs[0].Inputs.SourceProof.ParentHash == "" {
				t.Fatal("selected origin not server-derived")
			}
			child := operationByID(&st, st.WorkerJobs[0].Inputs.SourceProof.DeliveryID)
			if child.Snapshot.Revision.HeadCommit != operationByID(&st, parent.ID).Sources[0].Result.SHA || len(operationByID(&st, parent.ID).CompositionSnapshot.RevisionSnapshots) != 1 {
				t.Fatal("generated source did not preserve selected revision scope")
			}
			// Public protocol metadata cannot replace this server-derived origin.
			public := domain.Command{Action: "queue_worker_job", Actor: "operator", ProductID: "p", IntegrationID: "i", Data: map[string]any{"request_key": "forged-native-parent-proof", "source_proof": st.WorkerJobs[0].Inputs.SourceProof}}
			if _, e = f.s.Execute(ctx, public); e == nil {
				t.Fatal("caller injected parent proof")
			}
			build := automaticBuild(t, f)
			for range 10 {
				nativeTick(t, f)
				state, _ := f.store.Read(ctx)
				if len(state.WorkerJobs) == 2 {
					break
				}
			}
			lease, e := f.s.ClaimWorker(ctx, f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish"})
			if e != nil || lease == nil {
				t.Fatal(e)
			}
			if _, e = f.s.StartWorker(ctx, f.p, workerRequest(lease)); e != nil {
				t.Fatal(e)
			}
			public = domain.Command{Action: "queue_worker_publication", Actor: "operator", ProductID: "p", Data: map[string]any{"build_job_id": build.Job.ID, "component_id": "component", "recipe_id": f.recipe.ID, "recipe_hash": f.recipe.Hash, "recipe_revision": f.recipe.Revision, "config_revision": "cfg1", "request_key": "forged-managed-publication", "deadline": time.Now().Add(time.Minute)}}
			if _, e = f.s.Execute(ctx, public); e == nil {
				t.Fatal("caller republished managed build")
			}
			p := *lease.Job.Inputs.Publication
			f.publisher.Plan = ocipublisher.Plan{ID: lease.Job.ID, Approved: true, ProductID: "p", ComponentID: p.ComponentID, RepositoryID: "source", Origin: p.Origin, Repository: p.Repository, Receipt: p.Receipt}
			if _, e = ocipublisher.Publish(ctx, f.publisher); e != nil {
				t.Fatal(e)
			}
			var once sync.Once
			f.registry.hook = func() {
				once.Do(func() {
					if e := f.store.Update(ctx, func(st *domain.State) error {
						parent := operationByID(st, parent.ID)
						switch change {
						case "desired":
							environmentByID(st, "dev").DesiredOperationID = "superseded"
						case "selection":
							parent.CompositionSnapshot.Components[0].RevisionIDs = nil
						case "generated-source":
							parent.Sources[0].Result.SHA = shaBase
						case "revision":
							parent.CompositionSnapshot.RevisionSnapshots[0].HeadCommit = shaBase
						case "scope":
							parent.Sources[0].Plan.HeadSHAs = nil
						}
						return nil
					}); e != nil {
						t.Error(e)
					}
				})
			}
			report := domain.WorkerReport{WorkerAttemptRequest: workerRequest(lease), Result: "PASSED", Publication: &domain.WorkerPublicationReport{ManifestDigest: p.Receipt.ManifestDigest}}
			if _, e = f.s.ReportWorker(ctx, f.p, report); e == nil {
				t.Fatal("superseded or altered composition admitted")
			}
			st, _ = f.store.Read(ctx)
			if len(st.DeliveryArtifacts) != 0 || len(st.WorkerJobs) != 2 {
				t.Fatal("altered origin persisted artifact or replacement execution")
			}
		})
	}
}

func TestNativeCancellationAndAuthorityEdges(t *testing.T) {
	for _, phase := range []string{"NATIVE_BUILD", "APPLY_UNCERTAIN", "WAIT_GITOPS_PR"} {
		t.Run("cancel/"+phase, func(t *testing.T) {
			f := automaticFixture(t, "local")
			ctx := context.Background()
			parent := automaticQueue(t, f)
			f.store.Update(ctx, func(st *domain.State) error { operationByID(st, parent.ID).Phase = phase; return nil })
			_, e := f.s.Execute(ctx, domain.Command{Action: "cancel_native_delivery", Actor: "operator", ProductID: "p", ID: parent.ID, Data: map[string]any{}})
			if (phase == "NATIVE_BUILD") != (e == nil) {
				t.Fatal("wrong PR intent cancellation boundary", e)
			}
		})
	}
	t.Run("managed-job-cancels-parent", func(t *testing.T) {
		f := automaticFixture(t, "local")
		ctx := context.Background()
		parent := automaticQueue(t, f)
		st, _ := f.store.Read(ctx)
		_, e := f.s.Execute(ctx, domain.Command{Action: "cancel_worker_job", Actor: "operator", ProductID: "p", ID: st.WorkerJobs[0].ID, Data: map[string]any{}})
		if e != nil {
			t.Fatal(e)
		}
		st, _ = f.store.Read(ctx)
		if operationByID(&st, parent.ID).Status != "CANCELLED" || st.WorkerJobs[0].Status != "CANCELLED" {
			t.Fatal("managed cancellation left delivery active")
		}
	})
	t.Run("recipe-revision-drift", func(t *testing.T) {
		f := automaticFixture(t, "local")
		ctx := context.Background()
		automaticQueue(t, f)
		r, _ := f.s.externalWorkers.Recipe("build")
		r.Revision = "changed"
		r.Hash = workerconfig.RecipeHash(r)
		publication := f.recipe
		publication.BuildRecipeHash = r.Hash
		publication.Hash = workerconfig.PublicationHash(publication)
		cfg := workerconfig.Config{Revision: "cfg2", LeaseSeconds: 5, Recipes: []workerconfig.Recipe{r}, Publications: []workerconfig.PublicationRecipe{publication}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}
		authority, e := workerconfig.New(cfg, "native-control-fixture", os.Getenv)
		if e != nil {
			t.Fatal(e)
		}
		f.s.ConfigureWorkers(authority)
		p, _ := authority.Authenticate(nativeBuildToken, time.Now())
		l, e := f.s.ClaimWorker(ctx, p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "local_oci_build"})
		if e != nil || l != nil {
			t.Fatal("drifted recipe claimed", e)
		}
		nativeTick(t, f)
		st, _ := f.store.Read(ctx)
		if len(st.WorkerJobs) != 1 || st.WorkerJobs[0].Status == "QUEUED" {
			t.Fatal("drift rebuilt old intent")
		}
	})
	t.Run("source-moves-during-registry-read", func(t *testing.T) {
		f := automaticFixture(t, "local")
		ctx := context.Background()
		automaticQueue(t, f)
		automaticBuild(t, f)
		nativeTick(t, f)
		l, e := f.s.ClaimWorker(ctx, f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish"})
		if e != nil || l == nil {
			t.Fatal(e)
		}
		if _, e = f.s.StartWorker(ctx, f.p, workerRequest(l)); e != nil {
			t.Fatal(e)
		}
		p := *l.Job.Inputs.Publication
		f.publisher.Plan = ocipublisher.Plan{ID: l.Job.ID, Approved: true, ProductID: "p", ComponentID: p.ComponentID, RepositoryID: "source", Origin: p.Origin, Repository: p.Repository, Receipt: p.Receipt}
		if _, e = ocipublisher.Publish(ctx, f.publisher); e != nil {
			t.Fatal(e)
		}
		f.registry.hook = func() { f.s.provider.(*fakeDelivery).branchSHA = shaBase }
		if _, e = f.s.ReportWorker(ctx, f.p, domain.WorkerReport{WorkerAttemptRequest: workerRequest(l), Result: "PASSED", Publication: &domain.WorkerPublicationReport{ManifestDigest: p.Receipt.ManifestDigest}}); e == nil {
			t.Fatal("source moved during registry I/O admitted")
		}
		st, _ := f.store.Read(ctx)
		if len(st.DeliveryArtifacts) != 0 {
			t.Fatal("moved source admitted")
		}
	})
}

func TestNativeExplicitIDRequestReplayScope(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f := automaticFixture(t, kind)
			ctx := context.Background()
			request := automaticCommand()
			request.ID = "explicit-native-delivery"
			original, e := f.s.Execute(ctx, request)
			if e != nil {
				t.Fatal(e)
			}
			for range 3 {
				nativeTick(t, f)
			}
			replay, e := f.s.Execute(ctx, request)
			if e != nil {
				t.Fatal(e)
			}
			if original.(domain.ExternalOperation).ID != replay.(domain.ExternalOperation).ID || !original.(domain.ExternalOperation).Native.Deadline.Equal(replay.(domain.ExternalOperation).Native.Deadline) {
				t.Fatal("explicit ID replay changed intent")
			}
			exec(t, f.s, domain.Command{Action: "create_integration", ID: "other-integration", FeatureID: "f", Data: map[string]any{"title": "other", "objective": "scope isolation"}})
			for _, kind := range []string{"key", "payload", "integration", "product", "action", "entity"} {
				bad := request
				bad.Data = map[string]any{}
				for k, v := range request.Data {
					bad.Data[k] = v
				}
				switch kind {
				case "key":
					bad.Data["request_key"] = "different-native-key"
				case "payload":
					bad.Data["environment_id"] = "other"
				case "integration":
					bad.IntegrationID = "other-integration"
				case "product":
					bad.ProductID = "other"
				case "action":
					bad.Action = "deploy_existing_artifact"
				case "entity":
					bad.ID = "f"
				}
				if _, e = f.s.Execute(ctx, bad); e == nil {
					t.Fatal("explicit ID/key bypassed", kind)
				}
			}
			st, _ := f.store.Read(ctx)
			if len(st.WorkerJobs) != 1 {
				t.Fatal("explicit ID retry duplicated execution")
			}
		})
	}
}
