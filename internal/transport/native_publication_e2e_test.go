package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/publishrunner"
	"releasecontrol/internal/testdocker"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
	"releasecontrol/internal/workerrunner"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type nativePRFixture struct {
	*persistedDeliveryFixture
	sha                    string
	merged                 bool
	last                   delivery.GitOpsApply
	generated              map[string]string
	proposals, inspections atomic.Int32
}

func (f *nativePRFixture) Branches(context.Context, delivery.Connection, string) ([]delivery.BranchInfo, error) {
	result := []delivery.BranchInfo{{Name: "main", SHA: f.sha}, {Name: "feature/work", SHA: f.sha}}
	for branch, sha := range f.generated {
		result = append(result, delivery.BranchInfo{Name: branch, SHA: sha})
	}
	return result, nil
}
func (f *nativePRFixture) ComposeSource(_ context.Context, _ delivery.Connection, plan delivery.SourcePlan) (delivery.SourceResult, error) {
	if f.generated == nil {
		f.generated = map[string]string{}
	}
	f.generated[plan.TargetBranch] = f.sha
	return delivery.SourceResult{Branch: plan.TargetBranch, SHA: f.sha}, nil
}
func (f *nativePRFixture) AdvanceMain(context.Context, delivery.Connection, string, string, string, string) (delivery.SourceResult, error) {
	return delivery.SourceResult{}, fmt.Errorf("native main advancement forbidden")
}
func (f *nativePRFixture) Compare(context.Context, delivery.Connection, string, string, string) (delivery.CompareResult, error) {
	return delivery.CompareResult{Status: "identical"}, nil
}
func (f *nativePRFixture) InspectArtifact(context.Context, delivery.Connection, delivery.ArtifactRequest) (delivery.ArtifactResult, error) {
	f.inspections.Add(1)
	return delivery.ArtifactResult{}, fmt.Errorf("GitHub fallback forbidden")
}
func (f *nativePRFixture) ProposeGitOps(_ context.Context, _ delivery.Connection, a delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	if a.Digest == "" || a.ExpectedHeadSHA == "" || a.ExpectedBlobSHA == "" {
		return delivery.GitOpsPullRequest{}, fmt.Errorf("unpinned GitOps")
	}
	f.proposals.Add(1)
	f.last = a
	return delivery.GitOpsPullRequest{Number: 9, URL: "https://github.test/fixture/pull/9", HeadSHA: f.sha, State: "open"}, nil
}
func (f *nativePRFixture) ObserveGitOpsPR(_ context.Context, _ delivery.Connection, _ delivery.GitOpsApply, p delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	p.Merged = f.merged
	return p, nil
}

func (f *nativePRFixture) ReadGitOps(context.Context, delivery.Connection, delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	result := delivery.GitOpsSnapshot{HeadSHA: strings.Repeat("c", 40), BlobSHA: strings.Repeat("e", 40)}
	if f.merged {
		result.Digest = f.last.Digest
		result.ImageRepository = f.last.Request.ImageRepository
	}
	return result, nil
}

// Explicit opt-in owns one exact-ID tmpfs registry and per-attempt build
// containers. No live RCP, registry credentials or application targets are used.
func TestRealNativeAPIWorkerPublisherDistributionPR(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("explicit Docker fixture opt-in required")
	}
	fixtureDocker := testdocker.Load(t)
	ctx := context.Background()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	binaries := map[string]string{}
	for _, name := range []string{"worker", "publisher"} {
		file := filepath.Join(root, name)
		cmd := exec.Command("go", "build", "-o", file, "../../cmd/"+name)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("build %s: %v %s", name, e, b)
		}
		binaries[name] = file
	}
	docker := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("docker", append([]string{"--context", fixtureDocker.Context}, args...)...)
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("owned Docker fixture: %v %s", e, b)
		}
		return strings.TrimSpace(string(b))
	}
	owner := fmt.Sprintf("native-e2e-%d", time.Now().UnixNano())
	id := docker("run", "--rm", "-d", "--label", "rcp.fixture.owner="+owner, "--read-only", "--tmpfs", "/var/lib/registry:rw,size=134217728", "--memory", "256m", "--cpus", "1", "--pids-limit", "128", "-p", "127.0.0.1::5000", "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		t.Fatal("invalid owned container identity")
	}
	t.Cleanup(func() { docker("rm", "--force", id) })
	origin := "http://" + docker("port", id, "5000/tcp")
	t.Logf("owned Distribution fixture owner=%s id=%s origin=%s", owner, id, origin)
	ready := false
	for range 50 {
		res, e := http.Get(origin + "/v2/")
		if e == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("Distribution not ready")
	}
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			dir, _ = filepath.EvalSymlinks(dir)
			repo := filepath.Join(dir, "source")
			os.Mkdir(repo, 0700)
			os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main(){}\n"), 0600)
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				b, e := cmd.CombinedOutput()
				if e != nil {
					t.Fatalf("fixture Git %v %s", e, b)
				}
				return strings.TrimSpace(string(b))
			}
			git("init")
			git("add", "main.go")
			git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-m", "Fixture")
			sha := git("rev-parse", "HEAD")
			var store interface {
				application.Store
				Close()
			}
			var err error
			if kind == "local" {
				store, err = persistence.OpenLocal(ctx, filepath.Join(dir, "rcp.db"))
			} else {
				store, err = persistence.Open(ctx, isolatedDatabase(t))
			}
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			e := &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b", BuildKitImage: fixtureDocker.BuildKitImage, Platform: fixtureDocker.Platform, CheckArgv: []string{"go", "test", "/source/main.go"}, BuildArgv: []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", "/output/app", "/source/main.go"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 300, MemoryMB: 1536, StorageMB: 512, CPUs: 2, PIDs: 128, SourceBytes: 1 << 20, OutputBytes: 16 << 20, LogBytes: 1 << 20}
			recipe := workerconfig.Recipe{ID: "build", Revision: "v1", ProductID: "p", RepositoryID: "source", RepositoryURL: "https://github.com/team/source", Capability: "local_oci_build", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, Execution: e}
			recipe.Hash = workerconfig.RecipeHash(recipe)
			publication := workerconfig.PublicationRecipe{ID: "publish", Revision: "v1", ProductID: "p", RepositoryID: "source", ComponentID: "component", Profile: "native-test", BuildRecipeHash: recipe.Hash, Origin: origin, Repository: "fixture/image", MaxBytes: 16 << 20, TimeoutSeconds: 30, Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}}
			publication.Hash = workerconfig.PublicationHash(publication)
			publication2 := publication
			publication2.ID = "publish2"
			publication2.ComponentID = "component2"
			publication2.Repository = "fixture/image2"
			publication2.Hash = workerconfig.PublicationHash(publication2)
			buildToken := "native-e2e-build-token-000000000000000001"
			publishToken := "native-e2e-publish-token-000000000000001"
			control := "native-e2e-control-token-000000000000001"
			t.Setenv("NATIVE_E2E_BUILD", buildToken)
			t.Setenv("NATIVE_E2E_PUBLISH", publishToken)
			authority, err := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 20, Recipes: []workerconfig.Recipe{recipe}, Publications: []workerconfig.PublicationRecipe{publication, publication2}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_E2E_BUILD", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_E2E_PUBLISH", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}, control, os.Getenv)
			if err != nil {
				t.Fatal(err)
			}
			provider := &nativePRFixture{persistedDeliveryFixture: &persistedDeliveryFixture{}, sha: sha}
			service := application.NewWithProvider(store, provider)
			service.ConfigureWorkers(authority)
			var starts, reports atomic.Int32
			base := transport.New(service, transport.Options{Token: control, Workers: authority})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer "+publishToken {
					lose := r.URL.Path == "/api/worker/start" && starts.Add(1) == 1 || r.URL.Path == "/api/worker/report" && reports.Add(1) == 1
					if lose {
						recorded := httptest.NewRecorder()
						base.ServeHTTP(recorded, r)
						if recorded.Code != 200 {
							t.Errorf("lost response action rejected: %d %s", recorded.Code, recorded.Body.String())
						}
						conn, _, e := w.(http.Hijacker).Hijack()
						if e == nil {
							conn.Close()
						}
						return
					}
				}
				base.ServeHTTP(w, r)
			}))
			defer server.Close()
			post := func(c domain.Command, out any) {
				t.Helper()
				c.Actor = "fixture/operator"
				if c.Data == nil {
					c.Data = map[string]any{}
				}
				b, _ := json.Marshal(c)
				req, _ := http.NewRequest("POST", server.URL+"/api/commands", bytes.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+control)
				res, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				defer res.Body.Close()
				b, _ = io.ReadAll(res.Body)
				if res.StatusCode != 200 {
					t.Fatalf("%s: %d %s", c.Action, res.StatusCode, b)
				}
				if out != nil && json.Unmarshal(b, out) != nil {
					t.Fatal("invalid command response")
				}
			}
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "native fixture"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "native fixture", "goal": "test"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "native fixture", "objective": "test"}}, {Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "fixture", "app_id": 1, "installation_id": 2, "private_key_ref": "env:UNUSED_FIXTURE_KEY", "owner": "team"}}} {
				post(c, nil)
			}
			if _, err = service.Query(ctx, "discover_repositories", "conn"); err != nil {
				t.Fatal(err)
			}
			for _, r := range []struct{ id, role string }{{"source", "APPLICATION"}, {"gitops", "GITOPS"}} {
				post(domain.Command{Action: "import_repository", ID: r.id, ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "team/" + r.id, "role": r.role, "default_branch": "main"}}, nil)
			}
			for _, c := range []domain.Command{{Action: "create_application", ID: "component", ProductID: "p", Data: map[string]any{"name": "fixture", "repository_id": "source"}}, {Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE", "native": map[string]any{"build_recipe_id": recipe.ID, "publication_recipe_id": publication.ID, "parameters": map[string]any{}}, "profile": publication.Profile, "image_repository": strings.TrimPrefix(origin, "http://") + "/fixture/image"}}, {Action: "create_environment", ID: "dev", ProductID: "p", Data: map[string]any{"name": "fixture Dev"}}, {Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}}, {Action: "record_integration_revision", ID: "rev", IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": sha, "head_commit": sha, "commits": []string{sha}}}} {
				post(c, nil)
			}

			home, _ := os.UserHomeDir()
			spoolRoot := filepath.Join(dir, "spool")
			if err := os.Mkdir(spoolRoot, 0700); err != nil {
				t.Fatal(err)
			}
			spool := &artifactspool.Config{Root: spoolRoot, MaxArchiveBytes: publication.MaxBytes, MaxTotalBytes: 64 << 20, MaxEntries: 8}
			workerCfg := workerrunner.Config{ArtifactSpool: spool, Endpoint: server.URL, TokenEnv: "NATIVE_E2E_BUILD", ProductID: "p", RepositoryID: "source", ConfigRevision: "cfg1", Recipe: recipe, SourceMirror: filepath.Join(repo, ".git"), StateDir: filepath.Join(dir, "worker-state"), DockerContext: fixtureDocker.Context, DockerConfig: filepath.Join(home, ".docker"), AllowRootlessUnconfined: true}
			workerFile := filepath.Join(dir, "worker.json")
			b, _ := json.Marshal(workerCfg)
			os.WriteFile(workerFile, b, 0600)
			pubDir := filepath.Join(dir, "publisher-state")
			os.Mkdir(pubDir, 0700)
			publisherCfg := publishrunner.Config{ArtifactSpool: spool, Endpoint: server.URL, TokenEnv: "NATIVE_E2E_PUBLISH", ConfigRevision: "cfg1", Recipe: publication, Publisher: ocipublisher.Config{AllowedOrigin: origin, AllowedRepository: publication.Repository, StateDir: pubDir, MaxArchiveBytes: publication.MaxBytes, TimeoutSeconds: publication.TimeoutSeconds, FixtureLoopbackHTTP: true}}
			pubFile := filepath.Join(dir, "publisher.json")
			b, _ = json.Marshal(publisherCfg)
			os.WriteFile(pubFile, b, 0600)
			run := func(binary, file string) error {
				cmd := exec.Command(binary, "run-once", file)
				b, e := cmd.CombinedOutput()
				if e != nil {
					t.Logf("subprocess %v: %s", e, b)
				}
				return e
			}
			tick := func() {
				t.Helper()
				store.Update(ctx, func(st *domain.State) error {
					for i := range st.Operations {
						st.Operations[i].NextAttemptAt = st.Operations[i].UpdatedAt
					}
					return nil
				})
				if _, e := service.Tick(ctx); e != nil {
					t.Fatal(e)
				}
			}
			var previousDigest string
			for deliveryIndex := 0; deliveryIndex < 2; deliveryIndex++ {
				if deliveryIndex > 0 {
					os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main(){println(42)}\n"), 0600)
					git("add", "main.go")
					git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Changed fixture")
					sha = git("rev-parse", "HEAD")
					provider.sha = sha
					provider.merged = false
					post(domain.Command{Action: "record_integration_revision", ID: "rev2", IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": sha, "head_commit": sha, "commits": []string{sha}}}, nil)
				}
				revision := "rev"
				if deliveryIndex > 0 {
					revision = "rev2"
				}
				request := domain.Command{Action: "deploy_integration", IntegrationID: "i", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": revision, "request_key": fmt.Sprintf("automatic-native-%d", deliveryIndex)}}
				var deploy, replay domain.ExternalOperation
				post(request, &deploy)
				post(request, &replay)
				if deploy.ID != replay.ID || deploy.Native == nil || !deploy.Native.Deadline.Equal(replay.Native.Deadline) {
					t.Fatal("delivery request replay changed identity or deadline")
				}
				for range 3 {
					tick()
				}
				st, _ := store.Read(ctx)
				if len(st.WorkerJobs) != (deliveryIndex*2 + 1) {
					t.Fatalf("automatic build not queued: %+v", st.Operations)
				}
				if e := run(binaries["worker"], workerFile); e != nil {
					t.Fatal("real automatic worker", e)
				}
				tick()
				if deliveryIndex == 0 {
					if run(binaries["publisher"], pubFile) == nil {
						t.Fatal("lost START accepted")
					}
					if run(binaries["publisher"], pubFile) == nil {
						t.Fatal("lost REPORT accepted")
					}
				}
				if e := run(binaries["publisher"], pubFile); e != nil {
					t.Fatal("real automatic publication/report", e)
				}
				for range 5 {
					tick()
				}
				st, _ = store.Read(ctx)
				var current domain.ExternalOperation
				for _, op := range st.Operations {
					if op.ID == deploy.ID {
						current = op
					}
				}
				if current.Phase != "WAIT_GITOPS_PR" || current.GitOpsResult != nil || provider.proposals.Load() != int32(deliveryIndex+1) {
					t.Fatalf("automatic graph did not reach one PR %+v", current)
				}
				artifact := st.DeliveryArtifacts[len(st.DeliveryArtifacts)-1]
				if artifact.Native == nil || artifact.SourceCommit != sha || artifact.Native.Receipt.ArchiveHash == "" || deliveryIndex > 0 && artifact.Digest == previousDigest {
					t.Fatal("exact changed-source native artifact missing")
				}
				previousDigest = artifact.Digest
				provider.merged = true
				tick()
				st, _ = store.Read(ctx)
				for _, op := range st.Operations {
					if op.ID == deploy.ID && op.Status != "SUCCEEDED" {
						t.Fatalf("external fixture PR merge not observed: %+v", op)
					}
				}
			}
			st, _ := store.Read(ctx)
			if len(st.WorkerJobs) != 4 || len(st.WorkerAttempts) != 4 || len(st.DeliveryArtifacts) != 2 || len(st.DeliveryBuildRuns) != 0 || provider.dispatches.Load() != 0 || provider.applies.Load() != 0 || provider.inspections.Load() != 0 {
				t.Fatal("automatic graph duplicates or GitHub fallback")
			}
			t.Logf("two changed-source protected API→Tick→worker subprocess→durable shared CAS spool→separate publisher subprocess→pinned Distribution→admission→GitOps PR, unchanged configs; lost START before execution and known REPORT replay; zero manual queue/archive copy/GHA/direct")

			post(domain.Command{Action: "create_application", ID: "component2", ProductID: "p", Data: map[string]any{"name": "second native component", "repository_id": "source"}}, nil)
			post(domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component2", "connection_id": "conn", "mode": "NATIVE", "profile": publication2.Profile, "image_repository": strings.TrimPrefix(origin, "http://") + "/" + publication2.Repository, "native": map[string]any{"build_recipe_id": recipe.ID, "publication_recipe_id": publication2.ID, "parameters": map[string]any{}}}}, nil)
			post(domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component2", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "second.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}}, nil)
			// Base-only two-component composition has no IntegrationID. Both jobs
			// prove their immutable generated-source origin through the parent.
			post(domain.Command{Action: "plan_composition", ID: "native-pair", ProductID: "p", Data: map[string]any{"environment_id": "dev", "name": "native pair", "components": []any{map[string]any{"application_id": "component", "base_ref": "main", "base_commit": sha, "target_branch": "generated/native-pair", "revision_ids": []string{}}, map[string]any{"application_id": "component2", "base_ref": "main", "base_commit": sha, "target_branch": "generated/native-pair", "revision_ids": []string{}}}}}, nil)
			var composition domain.ExternalOperation
			post(domain.Command{Action: "reconcile_composition", ID: "native-pair", ProductID: "p"}, &composition)
			provider.merged = false
			for range 10 {
				tick()
			}
			st, _ = store.Read(ctx)
			if len(st.WorkerJobs) != 6 {
				t.Fatalf("two native compose builds missing %+v", st.Operations)
			}
			for range 2 {
				if e := run(binaries["worker"], workerFile); e != nil {
					t.Fatal("real compose worker", e)
				}
			}
			for range 5 {
				tick()
			}
			pubDir2 := filepath.Join(dir, "publisher2-state")
			os.Mkdir(pubDir2, 0700)
			publisherCfg2 := publisherCfg
			publisherCfg2.Recipe = publication2
			publisherCfg2.Publisher.AllowedRepository = publication2.Repository
			publisherCfg2.Publisher.StateDir = pubDir2
			pubFile2 := filepath.Join(dir, "publisher2.json")
			b, _ = json.Marshal(publisherCfg2)
			os.WriteFile(pubFile2, b, 0600)
			for _, file := range []string{pubFile, pubFile2} {
				if e := run(binaries["publisher"], file); e != nil {
					t.Fatal("real compose publisher", e)
				}
			}
			for range 10 {
				tick()
			}
			st, _ = store.Read(ctx)
			componentChildren := 0
			for _, op := range st.Operations {
				if op.ParentID == composition.ID && op.Kind == "DEPLOY" {
					componentChildren++
					if op.IntegrationID != "" || op.Phase != "WAIT_GITOPS_PR" || op.ExistingArtifact == nil || op.ExistingArtifact.SourceCommit != sha {
						t.Fatalf("invalid composed native child %+v", op)
					}
				}
			}
			if componentChildren != 2 || len(st.WorkerJobs) != 8 || len(st.DeliveryArtifacts) != 4 || provider.proposals.Load() != 4 {
				t.Fatal("two-component compose did not use same native path")
			}
			for _, job := range st.WorkerJobs[4:] {
				if job.Inputs.SourceProof == nil || job.Inputs.SourceProof.ParentID != composition.ID || job.IntegrationID != "" {
					t.Fatal("composed jobs use forged integration or lack origin proof")
				}
			}
			t.Log("actual two-component COMPOSE→generated-source proof→2 real builds→2 spool publications→2 exact admitted artifacts→2 GitOps PRs; no fabricated IntegrationID")

		})
	}
}

func (f *nativePRFixture) EnsureRef(ctx context.Context, _ delivery.Connection, _, branch, sha string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.generated == nil {
		f.generated = map[string]string{}
	}
	if prior := f.generated[branch]; prior != "" && prior != sha {
		return "", fmt.Errorf("retained ref moved")
	}
	f.generated[branch] = sha
	return sha, nil
}
func (f *nativePRFixture) ObserveBranch(ctx context.Context, _ delivery.Connection, _, branch string) (delivery.BranchInfo, error) {
	if err := ctx.Err(); err != nil {
		return delivery.BranchInfo{}, err
	}
	sha := f.generated[branch]
	if sha == "" {
		return delivery.BranchInfo{}, fmt.Errorf("retained ref missing")
	}
	return delivery.BranchInfo{Name: branch, SHA: sha}, nil
}
func (f *nativePRFixture) DiscoverBranchPullRequests(context.Context, delivery.Connection, string, string) ([]delivery.PullRequestObservation, error) {
	return nil, nil
}
func (f *nativePRFixture) VerifyComposedSource(ctx context.Context, _ delivery.Connection, p delivery.SourcePlan, r delivery.SourceResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.Branch != p.TargetBranch || r.SHA != f.generated[p.TargetBranch] {
		return fmt.Errorf("generated source changed")
	}
	return nil
}
