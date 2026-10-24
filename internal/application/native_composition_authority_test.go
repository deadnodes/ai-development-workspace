package application

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

func compositionAuthorityFixture(t *testing.T, kind string) (*nativeFixture, domain.ExternalOperation, *fakeFlow) {
	t.Helper()
	f := automaticFixture(t, kind)
	provider := &fakeFlow{main: shaBase, branches: map[string]string{"feature/work": shaHead}, parents: map[string][]string{shaHead: {shaBase}}, deployments: map[string]string{}, applyCounts: map[string]int{}}
	f.s.provider = provider
	exec(t, f.s, domain.Command{Action: "plan_composition", ID: "frozen-native", ProductID: "p", Data: map[string]any{"name": "frozen native", "environment_id": "dev", "components": []domain.CompositionComponent{component("component", "rev")}}})
	v, e := f.s.Execute(context.Background(), domain.Command{Action: "reconcile_composition", Actor: "operator", ID: "frozen-native", Data: map[string]any{}})
	if e != nil {
		t.Fatal(e)
	}
	return f, v.(domain.ExternalOperation), provider
}
func replacementCompositionAuthority(t *testing.T, f *nativeFixture, drift string) *workerconfig.Authority {
	t.Helper()
	r, _ := f.s.externalWorkers.Recipe("build")
	p := f.recipe
	revision := "cfg1"
	switch drift {
	case "config":
		revision = "cfg2"
	case "build":
		r.Revision = "build2"
		r.Execution.TimeoutSeconds++
		r.Hash = workerconfig.RecipeHash(r)
		p.BuildRecipeHash = r.Hash
		p.Hash = workerconfig.PublicationHash(p)
	case "publication":
		p.Revision = "publication2"
		p.TimeoutSeconds++
		p.Hash = workerconfig.PublicationHash(p)
	}
	cfg := workerconfig.Config{Revision: revision, LeaseSeconds: 5, Recipes: []workerconfig.Recipe{r}, Publications: []workerconfig.PublicationRecipe{p}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}
	authority, e := workerconfig.New(cfg, "native-control-fixture", os.Getenv)
	if e != nil {
		t.Fatal(e)
	}
	return authority
}
func TestNativeCompositionAuthorityFrozenBeforeChildren(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, phase := range []string{"COMPOSE_SOURCE", "QUEUE_BUILDS"} {
			for _, drift := range []string{"config", "build", "publication", "unchanged"} {
				t.Run(kind+"/"+phase+"/"+drift, func(t *testing.T) {
					f, parent, provider := compositionAuthorityFixture(t, kind)
					ctx := context.Background()
					if phase == "QUEUE_BUILDS" {
						for range 5 {
							nativeTick(t, f)
							st, _ := f.store.Read(ctx)
							if operationByID(&st, parent.ID).Phase == phase {
								break
							}
						}
					}
					before, e := f.store.Read(ctx)
					if e != nil {
						t.Fatal(e)
					}
					frozen := *operationByID(&before, parent.ID)
					identity := frozen.PreparedDeployments[0].Native
					if identity == nil || identity.ConfigRevision != "cfg1" || identity.BuildRecipeHash == "" || identity.PublicationRecipeHash != f.recipe.Hash || !identity.Deadline.Equal(parent.CreatedAt.Add(time.Hour)) {
						t.Fatal("request did not durably freeze complete authority/deadline")
					}
					if frozen.Phase != phase || len(before.WorkerJobs) != 0 || len(frozen.ChildIDs) != 0 {
						t.Fatal("fixture advanced beyond prechild boundary")
					}
					authority := f.s.externalWorkers
					if drift != "unchanged" {
						authority = replacementCompositionAuthority(t, f, drift)
					}
					f.store.Close()
					f.store = f.reopen()
					f.s = NewWithProvider(f.store, provider)
					f.s.ConfigureWorkers(authority)
					nativeTick(t, f)
					after, e := f.store.Read(ctx)
					if e != nil {
						t.Fatal(e)
					}
					resumed := operationByID(&after, parent.ID)
					if !reflect.DeepEqual(frozen.PreparedDeployments, resumed.PreparedDeployments) {
						t.Fatal("restart rewrote frozen component authority")
					}
					if drift != "unchanged" {
						if resumed.Status != "FAILED" || len(resumed.ChildIDs) != 0 || len(after.WorkerJobs) != 0 || len(after.DeliveryArtifacts) != 0 || len(provider.plans) > len(frozen.Sources) || len(provider.builds) != 0 || provider.advances != 0 {
							t.Fatalf("prechild native authority drift accepted: phase=%s status=%s children=%d jobs=%d", resumed.Phase, resumed.Status, len(resumed.ChildIDs), len(after.WorkerJobs))
						}
					} else {
						for range 15 {
							nativeTick(t, f)
							after, _ = f.store.Read(ctx)
							if len(after.WorkerJobs) > 0 {
								break
							}
						}
						if len(after.WorkerJobs) != 1 {
							t.Fatal("unchanged request-time authority did not resume", after.Operations)
						}
						job := after.WorkerJobs[0]
						if job.Inputs.ConfigRevision != identity.ConfigRevision || job.Inputs.RecipeHash != identity.BuildRecipeHash || job.Inputs.RecipeRevision != identity.BuildRecipeRevision || !job.Deadline.Equal(identity.Deadline) {
							t.Fatal("child silently refroze request-time authority")
						}

					}
				})
			}
		}
	}
}

func TestNativeCompositionMalformedAuthorityFailsBeforeSource(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, malformed := range []string{"missing", "omitted", "duplicate", "deadline", "config", "job-link", "nonnative"} {
			t.Run(kind+"/"+malformed, func(t *testing.T) {
				f, parent, provider := compositionAuthorityFixture(t, kind)
				ctx := context.Background()
				// Put a GitHub component first so validation cannot silently stop before the
				// native second component. This is an immutable malformed-record fixture.
				if e := f.store.Update(ctx, func(st *domain.State) error {
					op := operationByID(st, parent.ID)
					native := op.PreparedDeployments[0]
					app := native.Application
					app.ID = "gha-component"
					st.Applications = append(st.Applications, app)
					build := native.Build
					build.ID = "gha-build"
					build.ApplicationID = app.ID
					build.Mode = "GITHUB_ACTIONS"
					build.Native = nil
					build.Profile = ""
					build.Workflow = "build.yml"
					build.ImageRepository = "ghcr.io/owner/gha"
					st.ComponentBuilds = append(st.ComponentBuilds, build)
					target := native.Target
					target.ID = "gha-target"
					target.ApplicationID = app.ID
					target.Path = "gha.yaml"
					st.EnvironmentBindings = append(st.EnvironmentBindings, target)
					gha := native
					gha.Application = app
					gha.Build = build
					gha.Target = target
					gha.Native = nil
					op.CompositionSnapshot.Components = append([]domain.CompositionComponent{component(app.ID, "rev")}, op.CompositionSnapshot.Components...)
					op.PreparedDeployments = append([]domain.DeliverySnapshot{gha}, op.PreparedDeployments...)
					switch malformed {
					case "missing":
						op.PreparedDeployments[1].Native = nil
					case "omitted":
						op.PreparedDeployments = op.PreparedDeployments[:1]
					case "duplicate":
						op.PreparedDeployments[1] = gha
					case "deadline":
						op.PreparedDeployments[1].Native.Deadline = time.Time{}
					case "config":
						op.PreparedDeployments[1].Native.ConfigRevision = ""
					case "job-link":
						op.PreparedDeployments[1].Native.BuildJobID = "forged"
					case "nonnative":
						op.PreparedDeployments[0].Native = op.PreparedDeployments[1].Native
					}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				f.store.Close()
				f.store = f.reopen()
				f.s.store = f.store
				nativeTick(t, f)
				st, _ := f.store.Read(ctx)
				op := operationByID(&st, parent.ID)
				if op.Status != "FAILED" || len(op.ChildIDs) != 0 || len(st.WorkerJobs) != 0 || len(provider.plans) != 0 || len(provider.builds) != 0 || provider.advances != 0 {
					t.Fatalf("malformed %s authority reached source/child execution %+v", malformed, op)
				}
			})
		}
	}
}

func TestMixedNativeParentAuthorityFencesGHAChild(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, drift := range []string{"config", "build", "publication", "unchanged", "post-io"} {
			t.Run(kind+"/"+drift, func(t *testing.T) {
				f := automaticFixture(t, kind)
				ctx := context.Background()
				provider := &fakeFlow{main: shaBase, branches: map[string]string{"feature/work": shaHead}, parents: map[string][]string{shaHead: {shaBase}}, deployments: map[string]string{}, applyCounts: map[string]int{}}
				f.s.provider = provider
				exec(t, f.s, domain.Command{Action: "create_application", ID: "gha", ProductID: "p", Data: map[string]any{"name": "GHA sibling", "repository_id": "source"}})
				exec(t, f.s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "gha", "connection_id": "conn", "workflow": "build.yml", "image_repository": "ghcr.io/owner/gha", "rebuild_missing": true}})
				exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "gha", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "gha.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
				exec(t, f.s, domain.Command{Action: "plan_composition", ID: "mixed-native", ProductID: "p", Data: map[string]any{"name": "mixed native", "environment_id": "dev", "components": []domain.CompositionComponent{component("gha", "rev"), component("component", "rev")}}})
				v, e := f.s.Execute(ctx, domain.Command{Action: "reconcile_composition", Actor: "operator", ID: "mixed-native", Data: map[string]any{}})
				if e != nil {
					t.Fatal(e)
				}
				parentID := v.(domain.ExternalOperation).ID
				var before domain.State
				for range 10 {
					nativeTick(t, f)
					before, _ = f.store.Read(ctx)
					if len(operationByID(&before, parentID).ChildIDs) == 2 {
						break
					}
				}
				parent := operationByID(&before, parentID)
				if len(parent.ChildIDs) != 2 || len(before.WorkerJobs) != 0 {
					t.Fatal("fixture must stop immediately after queuing children")
				}
				var ghaID string
				if e = f.store.Update(ctx, func(st *domain.State) error {
					for i := range st.Operations {
						op := &st.Operations[i]
						op.NextAttemptAt = time.Now().Add(time.Hour)
						if op.ParentID == parentID && op.ApplicationID == "gha" {
							ghaID = op.ID
							op.Phase = "DISPATCH"
							if drift == "post-io" {
								op.Phase = "PREFLIGHT"
							}
							op.NextAttemptAt = time.Time{}
						}
					}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				authority := f.s.externalWorkers
				if drift != "unchanged" && drift != "post-io" {
					authority = replacementCompositionAuthority(t, f, drift)
				}
				f.store.Close()
				f.store = f.reopen()
				f.s = NewWithProvider(f.store, provider)
				f.s.ConfigureWorkers(authority)
				if drift == "post-io" {
					provider.onRead = func() { f.s.ConfigureWorkers(replacementCompositionAuthority(t, f, "config")) }
				}
				worked, tickErr := f.s.Tick(ctx)
				if !worked || drift != "post-io" && tickErr != nil || drift == "post-io" && tickErr == nil {
					t.Fatal("targeted GHA sibling tick", worked, tickErr)
				}
				after, _ := f.store.Read(ctx)
				child := operationByID(&after, ghaID)
				if drift == "unchanged" {
					if len(provider.builds) != 1 || child.Phase != "BUILD_LOOKUP" {
						t.Fatal("valid mixed parent blocked existing GHA dispatch")
					}
				} else if drift == "post-io" {
					if len(provider.builds) != 0 || child.Phase != "PREFLIGHT" || len(after.WorkerJobs) != 0 {
						t.Fatal("post-I/O authority drift committed dispatch intent or executed sibling")
					}
				} else if len(provider.builds) != 0 || child.Status != "CANCELLED" || len(after.WorkerJobs) != 0 || len(after.DeliveryArtifacts) != 0 {
					t.Fatalf("GHA sibling dispatched under invalid native authority: status=%s dispatches=%d", child.Status, len(provider.builds))
				}
			})
		}
	}
}
